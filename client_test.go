package energontrol

// Client construction and configuration: the options and how they are
// reconciled, the logger, the address-space root, the polling budget, the
// command timeout, the availability check, and the per-plant command lock.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// msec keeps the polling settings in these tests short and readable.
const msec = time.Millisecond

// recordingLogger collects what the package logs, so a test can assert on the
// one channel through which a tolerated gap in the verification is reported.
type recordingLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *recordingLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *recordingLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newRecordingLogger() (*recordingLogger, *slog.Logger) {
	rec := &recordingLogger{}
	return rec, slog.New(slog.NewTextHandler(rec, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// errOrResult renders whichever of the two carries the message, so a test can
// assert on the text without caring at which level the failure surfaced.
func errOrResult(err error, res Results) string {
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	for _, r := range res {
		b.WriteString(r.String())
	}
	return b.String()
}

// rootedFake is a fake whose address space sits under a different root, the way
// an installation that does not expose its park under "Loc" would.
func rootedFake(root string) *fakeOPC {
	f := newFakeOPC()
	f.Root = root
	f.Ctrl[2] = uint64(CtrlStart)
	f.Branches = map[uint8][]string{2: {"Ctrl", "SetCtrl"}}
	return f
}

// A nil logger must not replace the default and must not panic.
func TestWithLoggerIgnoresNil(t *testing.T) {
	c := New(newFakeOPC(), WithLogger(nil))
	if c.log == nil {
		t.Fatal("WithLogger(nil) left the client without a logger")
	}
	if _, err := c.Reset(context.Background(), testUser, 2); err != nil {
		t.Fatalf("Reset: %v", err)
	}
}

// GetStatus failing means the server could not be reached at all.
func TestGetStatusFailureSurfaces(t *testing.T) {
	f := newFakeOPC()
	f.StatusErr = errTransport

	if err := New(f).ServerAvailable(context.Background()); !errors.Is(err, errTransport) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
	if _, err := New(f).Stop(context.Background(), testUser, true, true, 2); !errors.Is(err, errTransport) {
		t.Fatalf("Stop: err = %v, want it to wrap the transport error", err)
	}
}

// The polling budget used to apply per state transition with nothing
// bounding the session as a whole, so four waits could run for four times the
// configured budget — long past the 60 s the session survives.
func TestPollBudgetsDoNotAccumulateBeyondTheSessionLifetime(t *testing.T) {
	f := newFakeOPC()
	for _, p := range []uint8{1, 2, 3, 4} {
		f.Ctrl[p] = uint64(CtrlStart)
	}
	f.Occupied[1] = true                 // never reaches "free"
	f.StuckAt[2] = SessionFree           // never reserved
	f.StuckAt[3] = SessionReserved       // never reaches parameter input
	f.StuckAt[4] = SessionParameterInput // never reaches session end

	// Scaled down to milliseconds so the clamp is observable in a unit test:
	// four transitions at one budget each would be 400 ms, while the lifetime
	// caps everything after the reservation at 100 ms.
	const budget = 100 * time.Millisecond
	c := New(f, WithSessionPolling(10*time.Millisecond, budget),
		WithSessionLifetime(budget))

	start := time.Now()
	res, err := c.Stop(context.Background(), testUser, true, true, 1, 2, 3, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)
	// One budget for the wait on "session free" — no session exists yet, so no
	// lifetime applies — plus one lifetime for everything after the
	// reservation, plus slack for the polling granularity.
	if limit := 2*budget + 100*time.Millisecond; elapsed > limit {
		t.Errorf("command ran for %s, want at most %s: the session lifetime does not bound the waits",
			elapsed, limit)
	}
	if res.InRequestedState() {
		t.Error("stuck sessions must not report success")
	}
	// The plants that ran out of session rather than out of budget say so.
	var expired int
	for _, r := range res {
		if errors.Is(r.Err, ErrSessionExpired) {
			expired++
		}
	}
	if expired == 0 {
		t.Errorf("no plant reported an expired session: %v", res)
	}
}

// The polling budget must not be configurable beyond the session lifetime: a
// client waiting longer than that is waiting on a session that is already gone.
func TestSessionPollingIsClampedToTheSessionLifetime(t *testing.T) {
	c := New(newFakeOPC(), WithSessionPolling(time.Second, 10*time.Minute))
	if c.pollTimeout > c.sessionLifetime {
		t.Errorf("poll timeout %s exceeds the session lifetime %s", c.pollTimeout, c.sessionLifetime)
	}
}

// "no two goroutines command the same plant" was documented but not
// enforced, although a Client owns one park and can enforce it.
//
// Reset is the command used here because it always opens a session: there is no
// target state that could make it a no-op for the second caller.
func TestCommandsForOnePlantAreSerialised(t *testing.T) {
	f := newFakeOPC()
	f.AutoFreeSessions = true

	var mu sync.Mutex
	var concurrent, maxConcurrent int
	f.OnWrite = func(name string) {
		if !strings.HasSuffix(name, "/SessionRequest") {
			return
		}
		mu.Lock()
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
	}

	c := New(f, WithSessionPolling(time.Millisecond, 500*time.Millisecond))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Overlapping plant sets: plant 2 is in every call.
			if _, err := c.Reset(context.Background(), testUser, 2, 5); err != nil {
				t.Errorf("Reset: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent > 1 {
		t.Errorf("%d sessions were reserved for the same plant at once", maxConcurrent)
	}
}

// The lock must not deadlock on overlapping plant sets given in different
// orders, and must be released on every path.
func TestPlantLockHandlesOverlappingSetsAndCancellation(t *testing.T) {
	f := newFakeOPC()
	for _, p := range []uint8{1, 2, 3} {
		f.Ctrl[p] = uint64(CtrlStop90)
	}
	c := New(f, WithSessionPolling(time.Millisecond, 20*time.Millisecond))

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, plants := range [][]uint8{{1, 2, 3}, {3, 2, 1}, {2, 3}, {1, 3}} {
			wg.Add(1)
			go func(p []uint8) {
				defer wg.Done()
				if _, err := c.Start(context.Background(), testUser, p...); err != nil {
					t.Errorf("Start(%v): %v", p, err)
				}
			}(plants)
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("overlapping plant sets deadlocked")
	}

	// A cancelled context must not leave a plant locked for good.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Start(ctx, testUser, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, err := c.Start(context.Background(), testUser, 1); err != nil {
		t.Errorf("plant 1 stayed locked after a cancelled call: %v", err)
	}
}

// A read-only call must not be blocked by the plant lock: monitoring has to
// keep working while a command on the same plant is in flight.
func TestReadsAreNotBlockedByThePlantLock(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	c := New(f)

	release, err := c.lockPlants(context.Background(), []uint8{2})
	if err != nil {
		t.Fatalf("lockPlants: %v", err)
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := c.PlantCtrlState(context.Background(), 2)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PlantCtrlState: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a read blocked on the plant lock")
	}
}

// Before the fix every item name was built from a hardcoded "Loc" prefix, so a
// server that exposes its park anywhere else answered nothing: every item came
// back missing, for every plant, with no hint as to why.
func TestItemRootIsConfigurable(t *testing.T) {
	f := rootedFake("Park")
	c := New(f, WithItemRoot("Park"))

	states, err := c.PlantCtrlState(context.Background(), 2)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if states[0].Err != nil {
		t.Fatalf("plant 2: %v — the configured root did not reach the item names", states[0].Err)
	}
	if states[0].Ctrl != CtrlStart {
		t.Errorf("state = %s, want %s", states[0].Ctrl, CtrlStart)
	}
	if got := f.ReadNames[0]; !strings.HasPrefix(got, "Park/") {
		t.Errorf("read %q, want it under the configured root", got)
	}
}

// The park number sits beside the plant branch rather than under it, so it has
// to follow the root as well.
func TestItemRootCoversTheParkNumber(t *testing.T) {
	f := rootedFake("Park")
	f.ParkNo = 4242
	got, err := New(f, WithItemRoot("Park")).ParkNo(context.Background())
	if err != nil {
		t.Fatalf("ParkNo: %v", err)
	}
	if got != 4242 {
		t.Errorf("ParkNo = %d, want 4242", got)
	}
}

// A whole command works under a different root, not just a read.
func TestCommandWorksUnderAConfiguredRoot(t *testing.T) {
	f := rootedFake("Park")
	res, err := New(f, WithItemRoot("Park")).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded", res[0])
	}
	if f.Ctrl[2] != uint64(CtrlStop90) {
		t.Errorf("plant state = %d, want %d", f.Ctrl[2], CtrlStop90)
	}
}

// The default is unchanged, which is what the data sheet documents.
func TestDefaultItemRootFollowsTheDataSheet(t *testing.T) {
	c := New(newFakeOPC())
	if got := c.items.ctrl(2); got != "Loc/Wec/Plant2/Ctrl/Ctrl" {
		t.Errorf("ctrl item = %q, want the documented default", got)
	}
	if got := c.items.parkNo(); got != "Loc/LocNo" {
		t.Errorf("park number item = %q, want the documented default", got)
	}
}

// An empty or slash-only root is a configuration mistake, not a reason to build
// item names that begin with a separator.
func TestItemRootIsValidated(t *testing.T) {
	for _, root := range []string{"", "   ", "/", "///"} {
		if _, err := NewWithOptions(newFakeOPC(), WithItemRoot(root)); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("WithItemRoot(%q): err = %v, want it to wrap ErrInvalidOption", root, err)
		}
	}
	// A root given with separators around it is usable; they are just trimmed.
	c, err := NewWithOptions(newFakeOPC(), WithItemRoot("/Park/"))
	if err != nil {
		t.Fatalf("WithItemRoot(%q): %v", "/Park/", err)
	}
	if got := c.items.parkNo(); got != "Park/LocNo" {
		t.Errorf("park number item = %q, want %q", got, "Park/LocNo")
	}
}

// Every polling attempt is a round trip. On a SCADA that answers slower than
// the whole budget, a pure wall-clock deadline gives exactly one attempt — and
// an earlier draft justified a one-second default as "ten attempts, the budget
// v1 used", which under any real latency it was not. A minimum number of
// attempts is therefore made regardless of the clock.
func TestPollingMakesAMinimumNumberOfAttempts(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Occupied[2] = true // never reaches "free", so the wait runs out

	// A budget far shorter than a single answer: without a floor the first
	// attempt would also be the last.
	c := New(f, WithSessionPolling(time.Microsecond, time.Microsecond))
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.InRequestedState() {
		t.Fatal("an occupied session must not report success")
	}
	if f.ReadCalls < minPollAttempts {
		t.Errorf("%d reads for a wait that ran out, want at least %d",
			f.ReadCalls, minPollAttempts)
	}
	if !errors.Is(res[0].Err, ErrSessionOccupied) {
		t.Errorf("err = %v, want it to wrap ErrSessionOccupied", res[0].Err)
	}
}

// The floor must not outlive the session: once the lifetime has run out the
// server has dropped the reservation, so a guaranteed attempt would be an
// attempt on a session that no longer exists.
//
// The interval is long and the lifetime short, so the two outcomes are far
// apart on the clock: honouring the lifetime costs at most one interval, while
// insisting on four attempts would cost three.
func TestSessionLifetimeOverridesTheMinimumAttempts(t *testing.T) {
	const interval = 100 * time.Millisecond
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved // never reaches parameter input

	c := New(f, WithSessionPolling(interval, interval),
		WithSessionLifetime(5*time.Millisecond))

	start := time.Now()
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, ErrSessionExpired) {
		t.Fatalf("err = %v, want it to wrap ErrSessionExpired", res[0].Err)
	}
	if limit := 2 * interval; elapsed > limit {
		t.Errorf("command ran for %s, want at most %s: the minimum attempt count "+
			"kept polling a session the server had already dropped", elapsed, limit)
	}
}

// The default budget is the one a real SCADA has to live with, so it is worth
// pinning: a value that leaves room for only a couple of round trips is what
// this finding was about.
func TestDefaultPollTimeoutLeavesRoomForRealRoundTrips(t *testing.T) {
	if defaultPollTimeout < time.Second {
		t.Errorf("defaultPollTimeout = %s: every attempt is a SOAP round trip",
			defaultPollTimeout)
	}
	if defaultPollTimeout > defaultSessionLifetime {
		t.Errorf("defaultPollTimeout %s exceeds the session lifetime %s",
			defaultPollTimeout, defaultSessionLifetime)
	}
}

// WithLenientWriteConfirmation covers a server that does not echo written
// items. It must not also switch off the session verification — the godoc of
// the single combined option claimed the value read-back remained as a check
// while in fact the same flag disabled it.
func TestLenientWriteConfirmationDoesNotWeakenSessionVerification(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ItemFault[setCtrlItem(2)] = "E_UNKNOWN_ITEM_NAME" // read-back impossible

	res, err := New(f, WithLenientWriteConfirmation()).
		Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatalf("result = %v: the write tolerance must not cover the read-back", res[0])
	}
	if !errors.Is(res[0].Err, ErrSessionUnverified) {
		t.Errorf("err = %v, want it to wrap ErrSessionUnverified", res[0].Err)
	}
}

// And the other way round: tolerating an unverifiable session must not accept
// an unconfirmed write, which is the only evidence a Reset has.
func TestLenientSessionVerificationDoesNotAcceptAnUnconfirmedWrite(t *testing.T) {
	f := newFakeOPC()
	f.OmitWrite[setResetItem(2)] = true

	res, err := New(f, WithLenientSessionVerification()).
		Reset(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatalf("result = %v: an unconfirmed write is not a write", res[0])
	}
	if !errors.Is(res[0].Err, ErrItemMissing) {
		t.Errorf("err = %v, want it to wrap ErrItemMissing", res[0].Err)
	}
}

// Each tolerance does cover its own case.
func TestEachLenientOptionCoversItsOwnCase(t *testing.T) {
	t.Run("session verification", func(t *testing.T) {
		f := newFakeOPC()
		f.Ctrl[2] = uint64(CtrlStart)
		f.ItemFault[setCtrlItem(2)] = "E_UNKNOWN_ITEM_NAME"

		rec, logger := newRecordingLogger()
		res, err := New(f, WithLenientSessionVerification(), WithLogger(logger)).
			Stop(context.Background(), testUser, true, true, 2)
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if res[0].Outcome != OutcomeCommanded {
			t.Fatalf("result = %v, want commanded under the session tolerance", res[0])
		}
		// The warning is the only remaining trace, so it has to be emitted.
		if got := rec.text(); !strings.Contains(got, "cannot read back") {
			t.Errorf("the tolerated gap was not logged:\n%s", got)
		}
	})
	t.Run("write confirmation", func(t *testing.T) {
		f := newFakeOPC()
		f.OmitWrite[setResetItem(2)] = true

		res, err := New(f, WithLenientWriteConfirmation()).
			Reset(context.Background(), testUser, 2)
		if err != nil {
			t.Fatalf("Reset: %v", err)
		}
		if res[0].Outcome != OutcomeCommanded {
			t.Fatalf("result = %v, want commanded under the write tolerance", res[0])
		}
	})
}

func TestNewRejectsANilTransport(t *testing.T) {
	if _, err := NewWithOptions(nil); err == nil {
		t.Fatal("New accepted a nil Transport; the first command would panic")
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("New(nil) did not panic")
		}
	}()
	_ = New(nil)
}

func TestNewRejectsUnusableOptionValues(t *testing.T) {
	for name, opt := range map[string]Option{
		"zero polling interval":     WithSessionPolling(0, time.Second),
		"zero polling timeout":      WithSessionPolling(time.Second, 0),
		"negative polling":          WithSessionPolling(-time.Second, time.Second),
		"zero session lifetime":     WithSessionLifetime(0),
		"negative session lifetime": WithSessionLifetime(-time.Second),
		"negative max state age":    WithMaxStateAge(-time.Second),
		"zero command timeout":      WithCommandTimeout(0),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewWithOptions(newFakeOPC(), opt)
			if !errors.Is(err, ErrInvalidOption) {
				t.Errorf("err = %v, want it to wrap ErrInvalidOption", err)
			}
		})
	}
	// Every rejected value is reported, not just the first.
	_, err := NewWithOptions(newFakeOPC(), WithSessionLifetime(0), WithCommandTimeout(-1))
	if err == nil || strings.Count(err.Error(), "invalid option") != 2 {
		t.Errorf("err = %v, want both rejected options named", err)
	}
	// A maximum state age of zero is the documented way to switch the check
	// off, so it is not an error.
	if _, err := NewWithOptions(newFakeOPC(), WithMaxStateAge(0)); err != nil {
		t.Errorf("WithMaxStateAge(0) rejected: %v", err)
	}
	// A nil logger stays documented as ignored: it is harmless, and a caller
	// may pass one straight from a configuration that has not set one up.
	if _, err := NewWithOptions(newFakeOPC(), WithLogger(nil)); err != nil {
		t.Errorf("WithLogger(nil) rejected: %v", err)
	}
}

// An earlier draft clamped the polling budget inside WithSessionPolling, against
// whichever session lifetime happened to be set at that moment. The same two
// options therefore produced a different client depending on their order.
func TestOptionOrderDoesNotMatter(t *testing.T) {
	first := New(newFakeOPC(),
		WithSessionPolling(time.Second, 90*time.Second),
		WithSessionLifetime(120*time.Second))
	second := New(newFakeOPC(),
		WithSessionLifetime(120*time.Second),
		WithSessionPolling(time.Second, 90*time.Second))

	if first.pollTimeout != second.pollTimeout {
		t.Errorf("poll timeout depends on option order: %s vs %s",
			first.pollTimeout, second.pollTimeout)
	}
	if first.pollTimeout != 90*time.Second {
		t.Errorf("poll timeout = %s, want the 90s that fits inside the 120s lifetime",
			first.pollTimeout)
	}
	if first.sessionLifetime != second.sessionLifetime {
		t.Errorf("session lifetime depends on option order: %s vs %s",
			first.sessionLifetime, second.sessionLifetime)
	}
}

// The clamp itself stays: waiting longer than the session lifetime means
// waiting on a session the server has already dropped.
func TestPollTimeoutIsStillClampedToTheLifetime(t *testing.T) {
	for _, c := range []*Client{
		New(newFakeOPC(), WithSessionPolling(time.Second, 10*time.Minute)),
		New(newFakeOPC(), WithSessionPolling(time.Second, 30*time.Second),
			WithSessionLifetime(10*time.Second)),
		New(newFakeOPC(), WithSessionLifetime(10*time.Second),
			WithSessionPolling(time.Second, 30*time.Second)),
	} {
		if c.pollTimeout > c.sessionLifetime {
			t.Errorf("poll timeout %s exceeds the session lifetime %s",
				c.pollTimeout, c.sessionLifetime)
		}
		if c.pollInterval > c.pollTimeout {
			t.Errorf("poll interval %s exceeds the poll timeout %s",
				c.pollInterval, c.pollTimeout)
		}
	}
}

// The session lifetime bounds everything from the reservation onwards, but the
// wait for state "free" happens before a session exists — so a generous polling
// budget could be spent waiting to start and a full lifetime spent afterwards.
func TestCommandTimeoutBoundsTheWaitBeforeAnyReservation(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Occupied[2] = true // never reaches "free"

	c := New(f,
		WithSessionPolling(5*time.Millisecond, time.Minute), // would wait a minute
		WithCommandTimeout(60*time.Millisecond))

	start := time.Now()
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// A context that runs out inside the session procedure is a per-plant
	// failure, the way a cancelled context is: the command was attempted.
	if !errors.Is(res[0].Err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", res[0].Err)
	}
	if res.InRequestedState() {
		t.Error("a command that ran out of time must not report success")
	}
	if elapsed > 2*time.Second {
		t.Errorf("command ran for %s despite a 60ms command timeout", elapsed)
	}
}

// Without the option the only bound is the caller's own context, which is the
// documented default.
func TestCommandTimeoutIsOffByDefault(t *testing.T) {
	if c := New(newFakeOPC()); c.commandTimeout != 0 {
		t.Errorf("commandTimeout = %s, want it off by default", c.commandTimeout)
	}
}

// The attribute is an xs:int in milliseconds, so an age beyond that range
// cannot be expressed. It is rejected as an option error rather than as a read
// that fails at run time.
func TestMaxStateAgeUpperBoundIsAnOptionError(t *testing.T) {
	tooLong := time.Duration(math.MaxInt32)*time.Millisecond + time.Millisecond
	_, err := NewWithOptions(newFakeOPC(), WithMaxStateAge(tooLong))
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("err = %v, want it to wrap ErrInvalidOption", err)
	}
	// The largest expressible value is accepted.
	if _, err := NewWithOptions(newFakeOPC(),
		WithMaxStateAge(time.Duration(math.MaxInt32)*time.Millisecond)); err != nil {
		t.Errorf("the largest expressible age was rejected: %v", err)
	}
}

// v1 answered (false, nil) for a server that was reachable but not running,
// and every caller then propagated a nil error.
func TestServerNotRunningIsAnError(t *testing.T) {
	f := newFakeOPC()
	f.ServerState = "suspended"
	f.Ctrl[2] = uint64(CtrlStart)

	_, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err == nil {
		t.Fatal("a suspended server produced no error")
	}
	if !errors.Is(err, ErrServerNotRunning) {
		t.Errorf("err = %v, want it to wrap ErrServerNotRunning", err)
	}
	if !strings.Contains(err.Error(), "suspended") {
		t.Errorf("err = %q, want it to name the observed state", err)
	}
}

// Tab. 81: the session timeout for control access to a single plant is 60 s.
// The default polling budget has to stay well below it, so the client is not
// still waiting on a session that has already expired.
func TestSpecPollingBudgetStaysBelowTheSessionTimeout(t *testing.T) {
	const enerconSessionTimeout = 60 * time.Second
	c := New(newFakeOPC())
	if c.pollTimeout >= enerconSessionTimeout {
		t.Errorf("poll timeout %s is not below the %s session timeout", c.pollTimeout, enerconSessionTimeout)
	}
	if c.pollTimeout < time.Second {
		t.Errorf("poll timeout %s leaves no room for a loaded SCADA", c.pollTimeout)
	}
}
