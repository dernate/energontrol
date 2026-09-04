package energontrol

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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
	// finished is set once the plant completed the whole procedure.
	finished bool
}

// newSessionCred draws the session id and the private key from crypto/rand.
// v1 seeded a fresh math/rand source from time.Now().UnixNano() on every call,
// which is both predictable and prone to producing identical keys for plants
// processed in the same nanosecond tick.
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
	written map[uint8][]writeItem
}

func newTracker(cmds []plantCommand) *tracker {
	t := &tracker{
		order:   make([]uint8, 0, len(cmds)),
		cmd:     make(map[uint8]plantCommand, len(cmds)),
		cred:    make(map[uint8]*sessionCred, len(cmds)),
		results: make(map[uint8]*PlantResult, len(cmds)),
		written: make(map[uint8][]writeItem, len(cmds)),
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
	// An empty result means nothing would be written, which is treated as a
	// failure rather than as success.
	parameterItems func(plant uint8, cmd plantCommand, cred *sessionCred) []writeItem
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
	deadline := c.now().Add(c.pollTimeout)
	var last map[uint8]SessionState
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
		pending := false
		for _, plant := range t.active() {
			if s, ok := states[plant]; ok && s != want {
				pending = true
				break
			}
		}
		if !pending {
			return nil
		}
		if !c.now().Before(deadline) {
			break
		}
		if err := c.sleep(ctx, c.pollInterval); err != nil {
			return err
		}
	}
	for _, plant := range t.active() {
		if s, ok := last[plant]; ok && s != want {
			t.fail(plant, &SessionStateError{PlantNo: plant, Want: want, Got: s})
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
		names[i] = sessionStateItem(plant, kind)
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
	items := make([]writeItem, 0, len(t.active()))
	owner := make(map[string]uint8, len(items))
	for _, plant := range t.active() {
		cred, err := newSessionCred()
		if err != nil {
			t.fail(plant, err)
			continue
		}
		// Mark the session as requested before the write: if the write fails at
		// transport level the server may still have reserved it, and a session
		// that might be open has to be accounted for.
		cred.requested = true
		t.cred[plant] = cred
		user, err := longWord(userID, "user id")
		if err != nil {
			t.fail(plant, err)
			continue
		}
		name := sessionRequestItem(plant, kind)
		owner[name] = plant
		items = append(items, writeItem{
			Name:  name,
			Value: []uint32{uint32(cred.sessionID), user, uint32(cred.privateKey)},
		})
	}
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
		names[i] = sessionPubKeyItem(plant, kind)
		owner[names[i]] = plant
	}
	values, err := c.readValues(ctx, names)
	if err != nil {
		return err
	}
	for name, v := range values {
		plant := owner[name]
		if v.Err != nil {
			t.fail(plant, v.Err)
			continue
		}
		if v.Value == 0 {
			t.fail(plant, fmt.Errorf("energontrol: plant %d: %w", plant, ErrPublicKey))
			continue
		}
		t.cred[plant].publicKey = v.Value
	}
	return nil
}

// writeParameters writes the actual command value of every active plant, in one
// request.
func (c *Client) writeParameters(ctx context.Context, t *tracker, p sessionProcedure) error {
	var items []writeItem
	owners := make(map[string]uint8)
	written := make(map[uint8][]string)
	for _, plant := range t.active() {
		cred := t.cred[plant]
		pending := p.parameterItems(plant, t.cmd[plant], cred)
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
	items := make([]writeItem, 0, len(plants))
	owner := make(map[string]uint8, len(plants))
	for _, plant := range plants {
		cred := t.cred[plant]
		name := sessionSubmitItem(plant, kind)
		owner[name] = plant
		pub, err := longWord(cred.publicKey, "public key")
		if err != nil {
			t.fail(plant, err)
			continue
		}
		items = append(items, writeItem{
			Name:  name,
			Value: []uint32{uint32(cred.privateKey), pub},
		})
	}
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
// A session id read back as zero means the server did not report one — this
// package never draws zero — and is treated as "cannot verify" rather than as a
// mismatch, so a server that does not expose the id logs a warning instead of
// failing every command.
func (c *Client) verifySession(ctx context.Context, t *tracker, kind SessionKind, checkParameters bool) error {
	plants := t.active()
	if len(plants) == 0 {
		return nil
	}
	var names []string
	for _, plant := range plants {
		names = append(names, sessionRequestItem(plant, kind))
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
		got := values[sessionRequestItem(plant, kind)]
		switch {
		case got.Err != nil:
			c.log.Warn("cannot read back the session id",
				"plant", plant, "kind", string(kind), "err", got.Err)
		case len(got.Values) == 0 || got.Values[0] == 0:
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
		for _, w := range t.written[plant] {
			readBack := values[w.Name]
			if readBack.Err != nil {
				c.log.Warn("cannot read back a written value",
					"plant", plant, "item", w.Name, "err", readBack.Err)
				continue
			}
			if len(readBack.Values) == 0 || len(w.Value) == 0 {
				continue
			}
			if readBack.Values[0] != uint64(w.Value[0]) {
				t.fail(plant, fmt.Errorf("%w: %s holds %d, this client wrote %d",
					ErrParameterNotAccepted, w.Name, readBack.Values[0], w.Value[0]))
				break
			}
		}
	}
	return nil
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
		if state == SessionFree || state == SessionWaitEnd {
			continue // nothing is holding the session
		}
		if c.release != nil {
			cred := t.cred[plant]
			if err := c.release(rctx, c.opc, plant, kind, cred.privateKey, cred.publicKey); err != nil {
				c.log.Warn("session release failed", "plant", plant, "kind", string(kind), "err", err)
				t.fail(plant, fmt.Errorf("%w: release failed: %w", ErrSessionLeftOpen, err))
				continue
			}
			c.log.Info("session released", "plant", plant, "kind", string(kind))
			continue
		}
		c.log.Warn("control session left open; it expires on the server's timeout",
			"plant", plant, "kind", string(kind), "state", state.String(),
			"timeoutRemaining", c.sessionTimeoutRemaining(rctx, plant, kind))
		t.fail(plant, ErrSessionLeftOpen)
	}
}

// sessionTimeoutRemaining reads the SessionTimeOut item, which reports how much
// of the session timeout is left. It is diagnostic only: a failure to read it
// must not turn into a failure of the command.
func (c *Client) sessionTimeoutRemaining(ctx context.Context, plant uint8, kind SessionKind) string {
	name := sessionTimeoutItem(plant, kind)
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
		parameterItems: func(plant uint8, cmd plantCommand, cred *sessionCred) []writeItem {
			pub, err := longWord(cred.publicKey, "public key")
			if err != nil {
				return nil
			}
			priv := uint32(cred.privateKey)
			var items []writeItem
			if cmd.SetCtrl {
				items = append(items, writeItem{
					Name:  setCtrlItem(plant),
					Value: []uint32{uint32(cmd.CtrlValue), priv, pub},
				})
			}
			if cmd.SetRbh {
				items = append(items, writeItem{
					Name:  setRbhItem(plant),
					Value: []uint32{uint32(cmd.RbhValue), priv, pub},
				})
			}
			if cmd.SetIceDet {
				items = append(items, writeItem{
					Name:  setIceDetItem(plant),
					Value: []uint32{uint32(cmd.IceDetValue), priv, pub},
				})
			}
			return items
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
		parameterItems: func(plant uint8, _ plantCommand, cred *sessionCred) []writeItem {
			pub, err := longWord(cred.publicKey, "public key")
			if err != nil {
				return nil
			}
			return []writeItem{{
				Name:  setResetItem(plant),
				Value: []uint32{uint32(plant), uint32(cred.privateKey), pub},
			}}
		},
	}, userID, cmds)
}
