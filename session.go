package energontrol

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"time"
)

// releaseTimeout bounds the best-effort cleanup of sessions that were reserved
// but could not be completed.
const releaseTimeout = 5 * time.Second

// plantCommand is what one plant should be told to do. Every command carries its
// own plant number, so no step of the procedure ever indexes a parallel slice.
//
// This is the structural fix for the v1 defect in which ControlAndRbh built
// action slices over the unfiltered plant list and controlProcedure then indexed
// them with the position in the filtered list: as soon as one plant was filtered
// out, the remaining plants were checked against another plant's action flag,
// the SetCtrl/SetRbh write was skipped, and the session still ran to completion
// and reported success.
type plantCommand struct {
	PlantNo     uint8
	SetCtrl     bool
	CtrlValue   CtrlValue
	SetRbh      bool
	RbhValue    RbhValue
	SetIceDet   bool
	IceDetValue IceDetValue
}

// sessionCred holds the credentials of one plant's control session.
type sessionCred struct {
	sessionID  uint8
	privateKey uint16
	publicKey  uint64
	// requested is set once a SessionRequest write was attempted. From that
	// moment the session may be reserved on the server and has to be accounted
	// for, even if the write itself failed.
	requested bool
	// valueWritten records that the actual SetCtrl/SetRbh/SetReset value reached
	// the server. Success is reported only for plants where this is true — the
	// session state alone does not prove that anything was written.
	valueWritten bool
	// submitted records that the server confirmed the SessionSubmit write. From
	// that moment the command may have taken effect, so a later failure is not
	// evidence that nothing happened.
	submitted bool
	// finished is set once the plant completed the whole procedure.
	finished bool
}

// newSessionCred draws the session id and the private key from crypto/rand.
// v1 seeded a fresh math/rand source from time.Now().UnixNano() on every call,
// which is both predictable and prone to producing identical keys for plants
// processed in the same nanosecond tick.
//
// The id is drawn from 1 to 19, which is what the session item accepts. Two
// clients competing for one plant therefore draw the same id about once in
// nineteen attempts, and in that case the ownership check confirms the other
// client's reservation as this client's. The check narrows the race, it does not
// close it; commands for one park belong in one process.
func newSessionCred() (*sessionCred, error) {
	// 1..19: a session id of 0 is reserved to mean "the server did not report
	// one", so verification can tell that case from a real mismatch.
	id, err := randUint(19)
	if err != nil {
		return nil, err
	}
	id++
	key, err := randUint(32000)
	if err != nil {
		return nil, err
	}
	return &sessionCred{
		sessionID: uint8(id),
		// Keep the private key away from zero: a zero key is indistinguishable
		// from an unset one.
		privateKey: uint16(key + 1),
	}, nil
}

func randUint(n int64) (uint64, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0, fmt.Errorf("energontrol: generating session credentials: %w", err)
	}
	return v.Uint64(), nil
}

// tracker carries the state of one run of the session procedure: which plants
// are still in play, what each of them was asked to do, and why the others
// dropped out.
type tracker struct {
	order   []uint8
	cmd     map[uint8]plantCommand
	cred    map[uint8]*sessionCred
	results map[uint8]*PlantResult
	// written records the parameter items actually written per plant, so they
	// can be read back before the session is submitted.
	written map[uint8][]ItemWrite
	// deadline is the moment the server drops the sessions this run reserved.
	// It is zero until a reservation was attempted, because before that there
	// is no session whose lifetime could run out.
	deadline time.Time
}

func newTracker(cmds []plantCommand) *tracker {
	t := &tracker{
		order:   make([]uint8, 0, len(cmds)),
		cmd:     make(map[uint8]plantCommand, len(cmds)),
		cred:    make(map[uint8]*sessionCred, len(cmds)),
		results: make(map[uint8]*PlantResult, len(cmds)),
		written: make(map[uint8][]ItemWrite, len(cmds)),
	}
	for _, c := range cmds {
		t.order = append(t.order, c.PlantNo)
		t.cmd[c.PlantNo] = c
		t.results[c.PlantNo] = &PlantResult{PlantNo: c.PlantNo, Outcome: OutcomeFailed}
	}
	return t
}

// active lists the plants that are still progressing through the procedure.
func (t *tracker) active() []uint8 {
	out := make([]uint8, 0, len(t.order))
	for _, p := range t.order {
		if t.results[p].Err == nil && t.results[p].Outcome != OutcomeCommanded {
			out = append(out, p)
		}
	}
	return out
}

// fail removes a plant from the run with a reason. The first reason wins;
// later ones are joined so that, for example, a session that was also left open
// is still visible.
func (t *tracker) fail(plant uint8, err error) {
	r, ok := t.results[plant]
	if !ok || err == nil {
		return
	}
	if r.Err == nil {
		r.Err = err
		r.Outcome = OutcomeFailed
		return
	}
	r.Err = errors.Join(r.Err, err)
}

// failAll ends the run for every plant still active, with the same reason.
func (t *tracker) failAll(err error) {
	for _, p := range t.active() {
		t.fail(p, err)
	}
}

// expired reports whether the sessions this run reserved have outlived the
// session lifetime.
func (t *tracker) expired(now time.Time) bool {
	return !t.deadline.IsZero() && !now.Before(t.deadline)
}

// waitDeadline is the moment a wait for a state transition has to give up: the
// polling budget, or the end of the session lifetime if that comes first.
func (t *tracker) waitDeadline(now time.Time, budget time.Duration) time.Time {
	deadline := now.Add(budget)
	if !t.deadline.IsZero() && t.deadline.Before(deadline) {
		return t.deadline
	}
	return deadline
}

func (t *tracker) list() Results {
	out := make(Results, 0, len(t.order))
	for _, p := range t.order {
		out = append(out, *t.results[p])
	}
	return out
}

// sessionProcedure describes one variant of the Enercon session state machine.
// Control commands and resets differ only in the branch they run in and in the
// item they write in the parameter step.
type sessionProcedure struct {
	kind SessionKind
	// parameterItems returns the items to write while the session is reserved.
	//
	// An empty result means nothing would be written, which is treated as a
	// failure rather than as success. An error means the items could not be
	// built at all, which is a different statement and gets its own reason: an
	// earlier draft discarded the error and returned no items, so a public key
	// that does not fit in a long word was reported to the caller as "nothing
	// to write" — a substituted reason, in a package whose whole argument is
	// that a check which cannot be carried out says so.
	parameterItems func(plant uint8, cmd plantCommand, cred *sessionCred) ([]ItemWrite, error)
	// verifyParameters reads the written items back before submitting. Enercon
	// documents SetCtrl, SetRbh and SetIceDet as readable for exactly this
	// check; it makes no such statement about SetReset.
	verifyParameters bool
}

// run drives the session state machine for every plant in cmds.
//
// The state machine is: free (0) -> reserved (1) -> parameter input (2) ->
// waiting time session end (4). Each transition is polled for, and every step
// works on the whole set of remaining plants in one request instead of one
// request per plant.
func (c *Client) run(ctx context.Context, p sessionProcedure, userID uint64, cmds []plantCommand) (res Results) {
	t := newTracker(cmds)
	if len(cmds) == 0 {
		return t.list()
	}
	// A session that was reserved but not completed must not be abandoned
	// silently. This runs on every exit path, including a cancelled context, and
	// rebuilds the result list so that a session left open is visible to the
	// caller rather than only in the log.
	defer func() {
		c.markUncertain(t)
		c.releaseSessions(ctx, t, p.kind)
		res = t.list()
	}()

	// The order follows the session schema in the Enercon technical data sheet,
	// including its two "check session state and session id" steps.
	steps := []func() error{
		func() error { return c.waitState(ctx, t, p.kind, SessionFree) },
		func() error { return c.requestSessions(ctx, t, p.kind, userID) },
		func() error { return c.waitState(ctx, t, p.kind, SessionReserved) },
		func() error { return c.verifySession(ctx, t, p.kind, false) },
		func() error { return c.fetchPublicKeys(ctx, t, p.kind) },
		func() error { return c.writeParameters(ctx, t, p) },
		func() error { return c.waitState(ctx, t, p.kind, SessionParameterInput) },
		func() error { return c.verifySession(ctx, t, p.kind, p.verifyParameters) },
		func() error { return c.submitSessions(ctx, t, p.kind) },
		func() error { return c.waitState(ctx, t, p.kind, SessionWaitEnd) },
	}
	for _, step := range steps {
		if len(t.active()) == 0 {
			return t.list()
		}
		// Once the session lifetime has run out the server has dropped the
		// reservation, so no later step can achieve anything — and writing into
		// an expired session is exactly what this bound exists to prevent.
		if t.expired(c.now()) {
			t.failAll(ErrSessionExpired)
			return t.list()
		}
		if err := step(); err != nil {
			// A request-level failure says nothing about the individual plants,
			// so every plant still in play inherits it.
			t.failAll(err)
			return t.list()
		}
	}
	for _, plant := range t.active() {
		cred := t.cred[plant]
		if cred == nil || !cred.valueWritten {
			// Reaching the final session state without having written a value is
			// exactly the v1 failure mode; report it instead of claiming success.
			t.fail(plant, fmt.Errorf("energontrol: plant %d: session completed but no value was written", plant))
			continue
		}
		cred.finished = true
		r := t.results[plant]
		r.Outcome = OutcomeCommanded
		r.Err = nil
	}
	return t.list()
}

// waitState polls the session state of the active plants until all of them reach
// want, or until the poll timeout expires. Plants that do not get there are
// failed with a SessionStateError, which unwraps to a specific sentinel such as
// ErrSessionOccupied or ErrInsufficientRights.
func (c *Client) waitState(ctx context.Context, t *tracker, kind SessionKind, want SessionState) error {
	deadline := t.waitDeadline(c.now(), c.pollTimeout)
	var last map[uint8]SessionState
	attempts := 0
	for {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		plants := t.active()
		if len(plants) == 0 {
			return nil
		}
		states, itemErrs, err := c.sessionStates(ctx, kind, plants)
		if err != nil {
			return err
		}
		for plant, e := range itemErrs {
			t.fail(plant, e)
		}
		last = states
		attempts++
		pending := false
		for _, plant := range t.active() {
			if s, ok := states[plant]; ok && !s.satisfies(want) {
				pending = true
				break
			}
		}
		if !pending {
			return nil
		}
		// The session lifetime is unconditional: once it has run out the server
		// has dropped the reservation, so no further attempt can achieve
		// anything. It therefore overrides the minimum attempt count below.
		if t.expired(c.now()) {
			break
		}
		// The polling budget is a wall-clock deadline and every attempt is a
		// round trip, so on a SCADA that answers slower than the whole budget
		// the first attempt would also be the last. A minimum number of
		// attempts is made regardless — bounded, like everything else, by the
		// session lifetime above.
		if attempts >= minPollAttempts && !c.now().Before(deadline) {
			break
		}
		if err := c.sleep(ctx, c.pollInterval); err != nil {
			return err
		}
	}
	// A wait that ran into the session lifetime rather than into its polling
	// budget failed for a different reason, and the caller needs to be able to
	// tell them apart: a state error invites a retry, an expired session says
	// the reservation is gone. The state error is kept alongside, so
	// errors.As still reports the state that was observed.
	expired := t.expired(c.now())
	for _, plant := range t.active() {
		if s, ok := last[plant]; ok && !s.satisfies(want) {
			var err error = &SessionStateError{PlantNo: plant, Want: want, Got: s}
			if expired {
				err = errors.Join(err, ErrSessionExpired)
			}
			t.fail(plant, err)
		}
	}
	return nil
}

// sessionStates reads the session state of the given plants in one request. The
// returned map holds the plants that could be read; itemErrs holds the reason
// for the ones that could not.
func (c *Client) sessionStates(ctx context.Context, kind SessionKind, plants []uint8) (
	map[uint8]SessionState, map[uint8]error, error) {
	names := make([]string, len(plants))
	owner := make(map[string]uint8, len(plants))
	for i, plant := range plants {
		names[i] = c.items.sessionState(plant, kind)
		owner[names[i]] = plant
	}
	values, err := c.readValues(ctx, names)
	if err != nil {
		return nil, nil, err
	}
	states := make(map[uint8]SessionState, len(plants))
	itemErrs := make(map[uint8]error)
	for name, v := range values {
		plant := owner[name]
		switch {
		case v.Err != nil:
			itemErrs[plant] = v.Err
		case v.Value > math.MaxUint16:
			itemErrs[plant] = &ItemError{ItemName: name, Reason: ErrUnexpectedType,
				Detail: fmt.Sprintf("session state %d out of range", v.Value)}
		default:
			states[plant] = SessionState(v.Value)
		}
	}
	return states, itemErrs, nil
}

// requestSessions reserves a session for every active plant, in one write.
func (c *Client) requestSessions(ctx context.Context, t *tracker, kind SessionKind, userID uint64) error {
	// The user id is the same for every plant, so it is a property of the
	// request, not of a plant. Public entry points reject an out-of-range id
	// before anything is sent; this is the last line of defence for a value
	// that reached the procedure some other way.
	user, err := longWord(userID, "user id")
	if err != nil {
		return err
	}
	plants := t.active()
	items := make([]ItemWrite, 0, len(plants))
	owner := make(map[string]uint8, len(plants))
	for _, plant := range plants {
		cred, err := newSessionCred()
		if err != nil {
			t.fail(plant, err)
			continue
		}
		t.cred[plant] = cred
		name := c.items.sessionRequest(plant, kind)
		owner[name] = plant
		items = append(items, ItemWrite{
			Name:  name,
			Value: []uint32{uint32(cred.sessionID), user, uint32(cred.privateKey)},
		})
	}
	// Mark the sessions as requested immediately before the write, and start
	// the lifetime clock with it: if the write fails at transport level the
	// server may still have reserved them, and a session that might be open has
	// to be accounted for. Nothing before this point can leave one behind.
	for _, plant := range owner {
		t.cred[plant].requested = true
	}
	t.deadline = c.now().Add(c.sessionLifetime)

	results, err := c.writeValues(ctx, items)
	if err != nil {
		return err
	}
	for name, e := range results {
		if e != nil {
			t.fail(owner[name], e)
		}
	}
	return nil
}

// fetchPublicKeys reads the session public key of every active plant in one
// request. A key of zero means the server did not hand out a session.
func (c *Client) fetchPublicKeys(ctx context.Context, t *tracker, kind SessionKind) error {
	plants := t.active()
	names := make([]string, len(plants))
	owner := make(map[string]uint8, len(plants))
	for i, plant := range plants {
		names[i] = c.items.sessionPubKey(plant, kind)
		owner[names[i]] = plant
	}
	values, err := c.readValues(ctx, names)
	if err != nil {
		return err
	}
	for name, v := range values {
		plant := owner[name]
		cred := t.cred[plant]
		if cred == nil {
			// The step order guarantees a credential for every active plant.
			// Report rather than dereference nil, so a future reordering of the
			// procedure surfaces as a failed command and not as a panic in a
			// library that moves turbines.
			t.fail(plant, fmt.Errorf("energontrol: plant %d: no session credentials", plant))
			continue
		}
		if v.Err != nil {
			t.fail(plant, v.Err)
			continue
		}
		if v.Value == 0 {
			t.fail(plant, fmt.Errorf("energontrol: plant %d: %w", plant, ErrPublicKey))
			continue
		}
		cred.publicKey = v.Value
	}
	return nil
}

// writeParameters writes the actual command value of every active plant, in one
// request.
func (c *Client) writeParameters(ctx context.Context, t *tracker, p sessionProcedure) error {
	var items []ItemWrite
	owners := make(map[string]uint8)
	written := make(map[uint8][]string)
	for _, plant := range t.active() {
		cred := t.cred[plant]
		if cred == nil {
			t.fail(plant, fmt.Errorf("energontrol: plant %d: no session credentials", plant))
			continue
		}
		pending, err := p.parameterItems(plant, t.cmd[plant], cred)
		if err != nil {
			t.fail(plant, fmt.Errorf("energontrol: plant %d: %w", plant, err))
			continue
		}
		if len(pending) == 0 {
			t.fail(plant, fmt.Errorf("energontrol: plant %d: nothing to write", plant))
			continue
		}
		for _, it := range pending {
			owners[it.Name] = plant
			written[plant] = append(written[plant], it.Name)
		}
		t.written[plant] = pending
		items = append(items, pending...)
	}
	results, err := c.writeValues(ctx, items)
	if err != nil {
		return err
	}
	for name, e := range results {
		if e != nil {
			t.fail(owners[name], e)
		}
	}
	for plant, names := range written {
		ok := true
		for _, n := range names {
			if results[n] != nil {
				ok = false
				break
			}
		}
		if ok {
			t.cred[plant].valueWritten = true
		}
	}
	return nil
}

// submitSessions commits the written values for every active plant, in one
// request.
func (c *Client) submitSessions(ctx context.Context, t *tracker, kind SessionKind) error {
	plants := t.active()
	items := make([]ItemWrite, 0, len(plants))
	owner := make(map[string]uint8, len(plants))
	for _, plant := range plants {
		cred := t.cred[plant]
		if cred == nil {
			t.fail(plant, fmt.Errorf("energontrol: plant %d: no session credentials", plant))
			continue
		}
		name := c.items.sessionSubmit(plant, kind)
		owner[name] = plant
		pub, err := longWord(cred.publicKey, "public key")
		if err != nil {
			t.fail(plant, err)
			continue
		}
		items = append(items, ItemWrite{
			Name:  name,
			Value: []uint32{uint32(cred.privateKey), pub},
		})
	}
	results, err := c.writeValues(ctx, items)
	if err != nil {
		return err
	}
	for name, e := range results {
		plant := owner[name]
		if e != nil {
			t.fail(plant, e)
			continue
		}
		// The server took the submit. Whatever fails after this, the command
		// may have taken effect, and the plant's result has to say so rather
		// than invite a retry. See markUncertain.
		if cred := t.cred[plant]; cred != nil {
			cred.submitted = true
		}
	}
	return nil
}

// markUncertain flags every plant whose submit the server confirmed but which
// did not complete the procedure.
//
// A command that fails after the submit was written and confirmed may
// nevertheless have taken effect: the server holds the value and has committed
// it, and only the confirmation that the session wound down is missing.
// Reporting that as a plain failure invites a retry, and a retry of a Start or
// a Reset is a second command to a plant that may already have had one — a Stop
// is protected by the 360 s delay before a new reservation, those two are not.
//
// It runs on every exit path of run, after the success pass, so a plant that
// did complete is already marked finished and is left alone.
func (c *Client) markUncertain(t *tracker) {
	for _, plant := range t.order {
		cred := t.cred[plant]
		if cred == nil || !cred.submitted || cred.finished {
			continue
		}
		if t.results[plant].Err == nil {
			continue
		}
		t.fail(plant, ErrOutcomeUncertain)
	}
}

// verifySession checks that the reserved session is this client's own, and
// optionally that the value written into it is the value that will be
// submitted.
//
// The Enercon session schema requires the client to verify its session id after
// the reservation and again after entering the parameters: the session id is the
// only means by which a client can tell its own reservation from another
// client's. Without this check, a client that lost a race writes its command
// into somebody else's session.
//
// Exactly one outcome is tolerated: a session id read back as zero means the
// server does not report the id at all, because this package never draws zero.
// That is the absence of information about a working session, and it is logged.
//
// Everything else that prevents the check from being carried out — a faulted
// item, a missing item, unusable quality, a stale value, a value of an
// unexpected type — fails the plant with ErrSessionUnverified. Those are
// statements about the item, not a missing server capability, and an
// unverifiable session is not a verified one. Treating them as tolerable is
// what let an earlier draft report OutcomeCommanded with Err == nil for a
// session it had never confirmed, and with the default logger discarding
// everything the caller had no way to find out.
//
// The session id being unverifiable does not stop the parameter check: both
// results are collected, so a plant's error names every check that could not be
// carried out rather than only the first.
func (c *Client) verifySession(ctx context.Context, t *tracker, kind SessionKind, checkParameters bool) error {
	plants := t.active()
	if len(plants) == 0 {
		return nil
	}
	var names []string
	for _, plant := range plants {
		names = append(names, c.items.sessionRequest(plant, kind))
		if checkParameters {
			for _, w := range t.written[plant] {
				names = append(names, w.Name)
			}
		}
	}
	values, err := c.readArrays(ctx, names)
	if err != nil {
		return err
	}
	for _, plant := range plants {
		cred := t.cred[plant]
		got := values[c.items.sessionRequest(plant, kind)]
		switch {
		case got.Err != nil:
			if c.lenientSessionVerify {
				c.log.Warn("cannot read back the session id",
					"plant", plant, "kind", string(kind), "err", got.Err)
				break
			}
			t.fail(plant, fmt.Errorf("%w: plant %d: the session id could not be read back: %w",
				ErrSessionUnverified, plant, got.Err))
			continue
		case len(got.Values) == 0 || got.Values[0] == 0:
			// The one tolerated case: the server does not report the id.
			c.log.Warn("server does not report the session id; reservation cannot be verified",
				"plant", plant, "kind", string(kind))
		case got.Values[0] != uint64(cred.sessionID):
			// The reservation is somebody else's, so it is also not ours to
			// release.
			cred.requested = false
			t.fail(plant, fmt.Errorf("%w: plant %d holds session id %d, this client reserved %d",
				ErrSessionIDMismatch, plant, got.Values[0], cred.sessionID))
			continue
		}
		if !checkParameters {
			continue
		}
		c.verifyParameters(t, plant, values)
	}
	return nil
}

// verifyParameters checks that every value written for one plant is the value
// the session now holds, and fails the plant if any of them cannot be confirmed.
func (c *Client) verifyParameters(t *tracker, plant uint8, values map[string]arrayValue) {
	for _, w := range t.written[plant] {
		readBack := values[w.Name]
		if readBack.Err != nil {
			if c.lenientSessionVerify {
				c.log.Warn("cannot read back a written value",
					"plant", plant, "item", w.Name, "err", readBack.Err)
				continue
			}
			t.fail(plant, fmt.Errorf("%w: plant %d: %s could not be read back: %w",
				ErrSessionUnverified, plant, w.Name, readBack.Err))
			return
		}
		if len(readBack.Values) == 0 {
			if c.lenientSessionVerify {
				c.log.Warn("written value read back empty", "plant", plant, "item", w.Name)
				continue
			}
			t.fail(plant, fmt.Errorf("%w: plant %d: %s read back without a value",
				ErrSessionUnverified, plant, w.Name))
			return
		}
		if len(w.Value) == 0 {
			// Cannot happen: every parameter item is written with three
			// elements. Guard rather than index into an empty slice.
			continue
		}
		if readBack.Values[0] != uint64(w.Value[0]) {
			t.fail(plant, fmt.Errorf("%w: %s holds %d, this client wrote %d",
				ErrParameterNotAccepted, w.Name, readBack.Values[0], w.Value[0]))
			return
		}
		// Comparing the value alone proves nothing when that value is zero:
		// CtrlStart, RbhSetStandard and IceDetLampOff all write 0, and an item
		// that was never written reads back exactly the same. The private key
		// is never drawn as zero, so where the server reports it back it is
		// what shows that the value in the session is this client's.
		switch {
		case len(readBack.Values) > 1 && len(w.Value) > 1 && readBack.Values[1] != 0:
			if readBack.Values[1] != uint64(w.Value[1]) {
				t.fail(plant, fmt.Errorf("%w: %s holds private key %d, this client wrote %d",
					ErrParameterNotAccepted, w.Name, readBack.Values[1], w.Value[1]))
				return
			}
		case w.Value[0] == 0:
			// The server does not report the key back, so the read-back cannot
			// distinguish this write from an untouched item. The write
			// confirmation is the remaining evidence; say so rather than count
			// a check that established nothing.
			c.log.Warn("read-back cannot confirm a value of zero; the write confirmation is the only evidence",
				"plant", plant, "item", w.Name)
		}
	}
}

// releaseSessions accounts for every session that was reserved but not
// completed.
//
// It first reads the current session state, so a plant whose session never got
// reserved, or which the server has already wound down, is not reported. For the
// rest it calls the release function from WithSessionRelease if one is
// configured, and otherwise attaches ErrSessionLeftOpen to the plant's result.
//
// Enercon documents no way to abort a session: a session ends by running into
// its timeout, which the technical data sheet gives as 60 s for control access
// to a single plant. The remaining time is readable from the SessionTimeOut
// item, and is included in the log line so an operator can see how long the
// plant will answer "occupied".
func (c *Client) releaseSessions(ctx context.Context, t *tracker, kind SessionKind) {
	var candidates []uint8
	for _, plant := range t.order {
		cred := t.cred[plant]
		if cred != nil && cred.requested && !cred.finished {
			candidates = append(candidates, plant)
		}
	}
	if len(candidates) == 0 {
		return
	}
	// Cleanup has to run even when the caller's context is already cancelled.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	states, _, err := c.sessionStates(rctx, kind, candidates)
	if err != nil {
		c.log.Warn("cannot determine session state during cleanup",
			"kind", string(kind), "plants", candidates, "err", err)
		for _, plant := range candidates {
			t.fail(plant, ErrSessionLeftOpen)
		}
		return
	}
	for _, plant := range candidates {
		state, ok := states[plant]
		if !ok {
			t.fail(plant, ErrSessionLeftOpen)
			continue
		}
		if state == SessionFree || state.satisfies(SessionWaitEnd) {
			// The session ran its course: either it is already free, or it is
			// winding down after a submit. It still answers "occupied" until
			// the session end delay has passed — 360 s after a stop — but there
			// is nothing left for this client to release or to report.
			continue
		}
		if c.release != nil {
			cred := t.cred[plant]
			if err := c.release(rctx, c.transport, plant, kind, cred.privateKey, cred.publicKey); err != nil {
				c.log.Warn("session release failed", "plant", plant, "kind", string(kind), "err", err)
				t.fail(plant, fmt.Errorf("%w: release failed: %w", ErrSessionLeftOpen, err))
				continue
			}
			c.log.Info("session released", "plant", plant, "kind", string(kind))
			continue
		}
		// Reading the remaining timeout is diagnosis, and Go evaluates a call
		// argument whether or not the handler keeps the record — so with the
		// default logger, which discards everything, every left-open session
		// used to cost an extra OPC read nobody would ever see, at the moment
		// the server was already in trouble.
		if c.log.Enabled(rctx, slog.LevelWarn) {
			c.log.Warn("control session left open; it expires on the server's timeout",
				"plant", plant, "kind", string(kind), "state", state.String(),
				"timeoutRemaining", c.sessionTimeoutRemaining(rctx, plant, kind))
		}
		t.fail(plant, ErrSessionLeftOpen)
	}
}

// sessionTimeoutRemaining reads the SessionTimeOut item, which reports how much
// of the session timeout is left. It is diagnostic only: a failure to read it
// must not turn into a failure of the command.
func (c *Client) sessionTimeoutRemaining(ctx context.Context, plant uint8, kind SessionKind) string {
	name := c.items.sessionTimeout(plant, kind)
	values, err := c.readValues(ctx, []string{name})
	if err != nil {
		return "unknown"
	}
	v := values[name]
	if v.Err != nil {
		return "unknown"
	}
	return (time.Duration(v.Value) * time.Second).String()
}

// sleep waits for d, or returns early when the context is cancelled. v1 used a
// bare time.Sleep in its retry loops, so a cancelled context did not stop a
// command until its whole retry budget was spent.
func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// controlProcedure runs the Ctrl branch of the session machine.
func (c *Client) controlProcedure(ctx context.Context, userID uint64, cmds []plantCommand) Results {
	return c.run(ctx, sessionProcedure{
		kind: SessionCtrl,
		parameterItems: func(plant uint8, cmd plantCommand, cred *sessionCred) ([]ItemWrite, error) {
			pub, err := longWord(cred.publicKey, "public key")
			if err != nil {
				return nil, err
			}
			priv := uint32(cred.privateKey)
			var items []ItemWrite
			if cmd.SetCtrl {
				items = append(items, ItemWrite{
					Name:  c.items.setCtrl(plant),
					Value: []uint32{uint32(cmd.CtrlValue), priv, pub},
				})
			}
			if cmd.SetRbh {
				items = append(items, ItemWrite{
					Name:  c.items.setRbh(plant),
					Value: []uint32{uint32(cmd.RbhValue), priv, pub},
				})
			}
			if cmd.SetIceDet {
				items = append(items, ItemWrite{
					Name:  c.items.setIceDet(plant),
					Value: []uint32{uint32(cmd.IceDetValue), priv, pub},
				})
			}
			return items, nil
		},
		verifyParameters: true,
	}, userID, cmds)
}

// resetProcedure runs the Reset branch of the session machine. v1 walked this
// branch one plant at a time with its own four polling loops each; it now uses
// the same batched machine as a control command.
func (c *Client) resetProcedure(ctx context.Context, userID uint64, cmds []plantCommand) Results {
	return c.run(ctx, sessionProcedure{
		kind: SessionReset,
		parameterItems: func(plant uint8, _ plantCommand, cred *sessionCred) ([]ItemWrite, error) {
			pub, err := longWord(cred.publicKey, "public key")
			if err != nil {
				return nil, err
			}
			return []ItemWrite{{
				Name:  c.items.setReset(plant),
				Value: []uint32{uint32(plant), uint32(cred.privateKey), pub},
			}}, nil
		},
	}, userID, cmds)
}
