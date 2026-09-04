package energontrol

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this file pin down the defects found in the audit of the first
// v2 draft. Each one reproduced a way in which the package could report success
// without the evidence its own documentation promises, or could keep working on
// a server that had stopped being trustworthy.

// A1: a write the server never confirmed must not count as a write.
//
// writeValues used to map an item missing from the WriteResponse to "accepted",
// which is the exact inverse of the rule reads follow. Reset is the case with no
// second line of defence: it does not read its parameters back, so the
// WriteResponse is the only evidence that SetReset arrived.
func TestUnconfirmedWriteIsNotASuccess(t *testing.T) {
	f := newFakeOPC()
	f.OmitWrite[setResetItem(2)] = true

	res, err := New(f).Reset(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatalf("result = %v, but the server never confirmed SetReset", res[0])
	}
	if !errors.Is(res[0].Err, ErrItemMissing) {
		t.Errorf("err = %v, want it to wrap ErrItemMissing", res[0].Err)
	}
}

// A1: the same for a control command, where the omitted item is one of several
// in a combined command.
func TestUnconfirmedWriteInACombinedCommandFailsThePlant(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Rbh[2] = RbhInstalled | RbhAutoDeicingAllowed
	f.OmitWrite[setRbhItem(2)] = true

	res, err := New(f).ControlAndRbh(context.Background(), testUser, ControlAndRbhValue{
		SetCtrlValue: true, CtrlValue: CtrlStop90,
		SetRbhValue: true, RbhValue: RbhSetManualOn,
	}, 2)
	if err != nil {
		t.Fatalf("ControlAndRbh: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatalf("result = %v, but SetRbh was never confirmed", res[0])
	}
	if f.Wrote(sessionSubmitItem(2, SessionCtrl)) {
		t.Error("a session with an unconfirmed value was submitted")
	}
}

// A1: WithLenientVerification is the documented way back to the old behaviour
// for a server that does not echo written items.
func TestLenientVerificationAcceptsAnUnconfirmedWrite(t *testing.T) {
	f := newFakeOPC()
	f.OmitWrite[setResetItem(2)] = true

	res, err := New(f, WithLenientVerification()).Reset(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded under WithLenientVerification", res[0])
	}
}

// A2: a value that could not be read back is not a verified value.
//
// verifySession used to log a warning and carry on, so the plant was reported
// as commanded with Err == nil — and because the default logger discards
// everything, a caller had no way to know the check had not happened.
func TestUnreadableParameterReadbackFailsThePlant(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ItemFault[setCtrlItem(2)] = "E_UNKNOWN_ITEM_NAME"

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatalf("result = %v, but the written value could not be verified", res[0])
	}
	if !errors.Is(res[0].Err, ErrSessionUnverified) {
		t.Errorf("err = %v, want it to wrap ErrSessionUnverified", res[0].Err)
	}
	if f.Wrote(sessionSubmitItem(2, SessionCtrl)) {
		t.Error("an unverified session was submitted")
	}
}

// A3: an unreadable session id means the reservation is unproven, which is a
// different statement from "the server does not report the id".
func TestUnreadableSessionIDFailsThePlant(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ItemFault[sessionRequestItem(2, SessionCtrl)] = "E_ACCESS_DENIED"

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatalf("result = %v, but session ownership was never verified", res[0])
	}
	if !errors.Is(res[0].Err, ErrSessionUnverified) {
		t.Errorf("err = %v, want it to wrap ErrSessionUnverified", res[0].Err)
	}
	if f.Wrote(setCtrlItem(2)) {
		t.Error("a command was written into a session whose ownership was unproven")
	}
}

// A2/A3: bad quality on a readback is as unusable as a fault.
func TestBadQualityOnAReadbackFailsThePlant(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Quality[setCtrlItem(2)] = "badNotConnected"

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatalf("result = %v, want the plant to fail on an unusable readback", res[0])
	}
}

// A server that types a one-element array as a bare scalar is answering the
// question that was asked; that shape must not be mistaken for "unverifiable".
func TestScalarArrayIsAValidReadback(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ScalarArrays = true

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded: a scalar carries the same value", res[0])
	}
}

// A3, tolerated case: a session id read back as 0 means the server does not
// report it. The client never draws 0, so this stays a warning.
func TestUnreportedSessionIDIsStillTolerated(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.SessionID[2] = 0

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded", res[0])
	}
}

// A14: the ServerState every OPC XML-DA response carries has to be checked, not
// only the one GetStatus reports before the command starts. A server can
// degrade in the middle of a session.
func TestServerStateInAReadResponseStopsTheCommand(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ResponseServerState = "failed"

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err == nil && res[0].Outcome == OutcomeCommanded {
		t.Fatal("a turbine was commanded while the server reported ServerState=failed")
	}
	if err != nil && !errors.Is(err, ErrServerNotRunning) {
		t.Errorf("err = %v, want it to wrap ErrServerNotRunning", err)
	}
	if err == nil && !errors.Is(res[0].Err, ErrServerNotRunning) {
		t.Errorf("err = %v, want it to wrap ErrServerNotRunning", res[0].Err)
	}
	if !strings.Contains(errOrResult(err, res), "failed") {
		t.Errorf("the observed state is not named: %v / %v", err, res)
	}
}

// A server that does not fill ServerState on a Read is not making a statement,
// so it must not be treated as broken.
func TestEmptyServerStateInAResponseIsTolerated(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ResponseServerState = " " // whitespace only: no state reported

	if _, err := New(f).PlantCtrlState(context.Background(), 2); err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
}

// A14: the check also has to cover a read that is not part of a command.
func TestServerStateIsCheckedOnAPlainRead(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90)
	f.ResponseServerState = "suspended"

	if _, err := New(f).PlantCtrlState(context.Background(), 2); !errors.Is(err, ErrServerNotRunning) {
		t.Fatalf("err = %v, want it to wrap ErrServerNotRunning", err)
	}
}

// A13: WithMaxStateAge used to fail open. A server that returns no item
// timestamp made the check the caller explicitly asked for disappear.
func TestMaxStateAgeRejectsAnItemWithoutATimestamp(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Timestamps = false // the server does not support ReturnItemTime

	c := New(f, WithMaxStateAge(time.Second))
	res, err := c.Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res[0].Err == nil {
		t.Fatal("an item without a timestamp was accepted although a maximum age was required")
	}
	if !errors.Is(res[0].Err, ErrNoItemTime) {
		t.Errorf("err = %v, want it to wrap ErrNoItemTime", res[0].Err)
	}
	// A caller that classifies staleness must still catch it.
	if !errors.Is(res[0].Err, ErrStaleValue) {
		t.Errorf("err = %v, want it to wrap ErrStaleValue as well", res[0].Err)
	}
}

// Without the option nothing changes: no age requirement, no timestamp needed.
func TestMissingTimestampIsFineWithoutMaxStateAge(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Timestamps = false

	res, err := New(f).Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !res[0].InRequestedState() {
		t.Fatalf("result = %v, want already-in-state", res[0])
	}
}

// A5: the polling budget used to apply per state transition with nothing
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

// A5: once the session lifetime has run out, the remaining steps cannot
// achieve anything and must not be attempted.
func TestExpiredSessionIsReportedAsExpired(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved

	c := New(f, WithSessionPolling(time.Millisecond, 50*time.Millisecond),
		WithSessionLifetime(20*time.Millisecond))

	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, ErrSessionExpired) {
		t.Errorf("err = %v, want it to wrap ErrSessionExpired", res[0].Err)
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

// A4: SessionWaitLoop (3) is a documented state of the machine. Reaching it
// already proves the submit was accepted, because only a submit moves a session
// out of parameter input.
func TestSessionWaitLoopCountsAsASubmittedSession(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.LoopModeAfterSubmit = true
	f.StuckAt[2] = SessionWaitLoop // and it stays there for this command

	c := New(f, WithSessionPolling(time.Millisecond, 20*time.Millisecond))
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded: state 3 means the submit was accepted", res[0])
	}
	if errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Error("a submitted session was reported as left open")
	}
}

// A7: an out-of-range user id is an argument error. It must be rejected before
// anything reaches the SCADA, and as an error of the call, not of a plant.
func TestUserIDIsValidatedAtTheAPIBoundary(t *testing.T) {
	const tooBig = uint64(1) << 40
	ctx := context.Background()

	calls := map[string]func(*Client) error{
		"Start": func(c *Client) error { _, err := c.Start(ctx, tooBig, 2); return err },
		"Stop":  func(c *Client) error { _, err := c.Stop(ctx, tooBig, true, true, 2); return err },
		"SetCtrl": func(c *Client) error {
			_, err := c.SetCtrl(ctx, tooBig, CtrlStop90, true, 2)
			return err
		},
		"SetRbh":    func(c *Client) error { _, err := c.SetRbh(ctx, tooBig, RbhSetManualOn, 2); return err },
		"SetIceDet": func(c *Client) error { _, err := c.SetIceDet(ctx, tooBig, IceDetLampOn, 2); return err },
		"Reset":     func(c *Client) error { _, err := c.Reset(ctx, tooBig, 2); return err },
		"ControlAndRbh": func(c *Client) error {
			_, err := c.ControlAndRbh(ctx, tooBig, ControlAndRbhValue{
				SetCtrlValue: true, CtrlValue: CtrlStop90}, 2)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			f := newFakeOPC()
			f.Ctrl[2] = uint64(CtrlStart)
			if err := call(New(f)); !errors.Is(err, ErrInvalidUserID) {
				t.Fatalf("err = %v, want it to wrap ErrInvalidUserID", err)
			}
			if f.ReadCalls != 0 || f.WriteCalls != 0 {
				t.Errorf("%d reads and %d writes reached the server before the argument was checked",
					f.ReadCalls, f.WriteCalls)
			}
		})
	}
}

// A9: "no two goroutines command the same plant" was documented but not
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
