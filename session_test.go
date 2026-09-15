package energontrol

// The Enercon control session state machine: reserving, verifying, writing,
// submitting, and what happens to a session that cannot be completed.

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// A server that does not report the session id is the single case the strict
// verification still tolerates. That makes the log line the only evidence the
// check did not happen, so it has to actually be emitted.
func TestUnreportedSessionIDIsLogged(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.SessionID[2] = 0

	rec, logger := newRecordingLogger()
	res, err := New(f, WithLogger(logger)).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded", res[0])
	}
	if got := rec.text(); !strings.Contains(got, "does not report the session id") {
		t.Errorf("the tolerated gap was not logged:\n%s", got)
	}
}

// A left-open session is reported in the result and logged with the time the
// plant will keep answering "occupied", read from SessionTimeOut.
func TestLeftOpenSessionLogsTheRemainingTimeout(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved
	f.SessionTO = 45

	rec, logger := newRecordingLogger()
	c := New(f, WithLogger(logger), WithSessionPolling(msec, 10*msec))
	if _, err := c.Stop(context.Background(), testUser, true, true, 2); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := rec.text()
	if !strings.Contains(got, "left open") {
		t.Errorf("the open session was not logged:\n%s", got)
	}
	if !strings.Contains(got, "45s") {
		t.Errorf("the log line does not carry the remaining timeout:\n%s", got)
	}
}

// The remaining timeout is diagnostic only: failing to read it must not change
// the outcome of the command.
func TestUnreadableSessionTimeoutDoesNotChangeTheOutcome(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved
	f.ItemFault[sessionTimeoutItem(2, SessionCtrl)] = "E_UNKNOWN_ITEM_NAME"

	rec, logger := newRecordingLogger()
	c := New(f, WithLogger(logger), WithSessionPolling(msec, 10*msec))
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !strings.Contains(rec.text(), "unknown") {
		t.Errorf("an unreadable timeout should be logged as unknown:\n%s", rec.text())
	}
	// Still the same failure it would have been with a readable timeout.
	if res[0].Outcome == OutcomeCommanded {
		t.Error("a stuck session was reported as commanded")
	}
}

// WithLenientVerification keeps the command going, but the gap it accepts has
// to be visible in the log — that is the whole trade the option makes.
func TestLenientVerificationLogsWhatItTolerates(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ItemFault[setCtrlItem(2)] = "E_UNKNOWN_ITEM_NAME"

	rec, logger := newRecordingLogger()
	c := New(f, WithLogger(logger), WithLenientVerification())
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded under WithLenientVerification", res[0])
	}
	if got := rec.text(); !strings.Contains(got, "cannot read back a written value") {
		t.Errorf("the tolerated gap was not logged:\n%s", got)
	}
}

// A failure before anything was reserved fails the command and leaves nothing
// open.
func TestTransportFailureWhileReadingTheStateFailsTheCall(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ReadErrOn[ctrlItem(2)] = errTransport

	_, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if !errors.Is(err, errTransport) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
	if f.WriteCalls != 0 {
		t.Errorf("%d writes went out after the state could not be read", f.WriteCalls)
	}
}

// A failure on the reservation write is the interesting case: the server may
// have reserved the session even though the response never arrived, so the
// session has to be accounted for.
func TestTransportFailureOnTheReservationIsReportedPerPlant(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.WriteErrOn[sessionRequestItem(2, SessionCtrl)] = errTransport

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatal("a plant whose reservation never completed was reported as commanded")
	}
	if !errors.Is(res[0].Err, errTransport) {
		t.Errorf("err = %v, want it to wrap the transport error", res[0].Err)
	}
	// The fake never applied the write, so the session is provably free and
	// must not be reported as left open. The cleanup has to establish that by
	// reading the state, not by guessing.
	if errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Errorf("a session that was never reserved was reported as left open: %v", res[0].Err)
	}
}

// The same failure, but the server did reserve the session before the
// connection broke: now it is left open and the caller has to be told.
func TestTransportFailureAfterAReservationReportsTheOpenSession(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	// The value write fails at transport level, after SessionRequest landed.
	f.WriteErrOn[setCtrlItem(2)] = errTransport

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatal("a plant whose value write failed was reported as commanded")
	}
	if !errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Errorf("err = %v, want it to wrap ErrSessionLeftOpen", res[0].Err)
	}
	if f.SessionStateOf(2, SessionCtrl) != SessionReserved {
		t.Errorf("session state = %v, want it still reserved on the server",
			f.SessionStateOf(2, SessionCtrl))
	}
}

// A failure on the submit leaves a session that holds a value but was never
// committed. Nothing may claim the command was carried out.
func TestTransportFailureOnTheSubmitIsNotASuccess(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.WriteErrOn[sessionSubmitItem(2, SessionCtrl)] = errTransport

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].InRequestedState() {
		t.Fatal("a session that was never submitted was reported as in the requested state")
	}
	if got := f.Ctrl[2]; got != uint64(CtrlStart) {
		t.Errorf("plant state = %d, want it unchanged at %d", got, CtrlStart)
	}
}

// A failure while polling for a state transition is a failure of the whole
// request: it says nothing about the individual plants, so every plant still in
// play inherits it.
func TestTransportFailureWhilePollingFailsEveryActivePlant(t *testing.T) {
	f := newFakeOPC()
	for _, p := range []uint8{2, 5} {
		f.Ctrl[p] = uint64(CtrlStart)
	}
	f.ReadErrOn[sessionStateItem(5, SessionCtrl)] = errTransport

	res, err := New(f, WithSessionPolling(time.Millisecond, 10*time.Millisecond)).
		Stop(context.Background(), testUser, true, true, 2, 5)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, r := range res {
		if r.InRequestedState() {
			t.Errorf("plant %d was reported as stopped although polling failed", r.PlantNo)
		}
		if !errors.Is(r.Err, errTransport) {
			t.Errorf("plant %d: err = %v, want it to wrap the transport error", r.PlantNo, r.Err)
		}
	}
}

// The cleanup read runs on a context of its own, so it still happens when the
// caller's context is already cancelled — and if it too fails, the session is
// reported as left open rather than assumed to be free.
func TestCleanupFailureReportsTheSessionAsLeftOpen(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved

	c := New(f, WithSessionPolling(time.Millisecond, 10*time.Millisecond))
	// Break the connection only once the session has been reserved and the
	// value written, so the read the cleanup needs is the one that fails.
	f.OnWrite = func(name string) {
		if name == setCtrlItem(2) {
			f.ReadErr = errTransport
		}
	}

	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Errorf("err = %v, want it to wrap ErrSessionLeftOpen", res[0].Err)
	}
	// The transport failure that caused it stays visible next to it.
	if !errors.Is(res[0].Err, errTransport) {
		t.Errorf("err = %v, want the transport error to remain visible", res[0].Err)
	}
}

// A write the server never confirmed must not count as a write.
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

// WithLenientVerification is the documented way back to the old behaviour
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

// A value that could not be read back is not a verified value.
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

// An unreadable session id means the reservation is unproven, which is a
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

// Bad quality on a readback is as unusable as a fault.
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

// A server that reports a written parameter back as the value alone, without
// the two keys, is answering the question that was asked; that shape must not
// be mistaken for "unverifiable". (Whether a bare scalar on the wire arrives
// here as a one-element slice is the adapter's business — see
// TestAdapterScalarBecomesAOneElementSlice.)
func TestScalarArrayIsAValidReadback(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.RawValues[setCtrlItem(2)] = []uint64{uint64(CtrlStop90)}

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded: a scalar carries the same value", res[0])
	}
}

// The one tolerated case: a session id read back as 0 means the server does not
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

// Once the session lifetime has run out, the remaining steps cannot
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

// SessionWaitLoop (3) is a documented state of the machine. Reaching it
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

// parameterItems used to discard the error from longWord and return no items,
// which writeParameters then reported as "nothing to write" — a wrong reason
// for a public key that does not fit in a long word, in a package whose whole
// argument is that a check which cannot be carried out says so.
func TestUnusablePublicKeyIsReportedAsSuch(t *testing.T) {
	for name, f := range map[string]*fakeOPC{
		"control command": func() *fakeOPC {
			f := newFakeOPC()
			f.Ctrl[2] = uint64(CtrlStart)
			f.PubKey = math.MaxUint32 + 1
			return f
		}(),
		"reset": func() *fakeOPC {
			f := newFakeOPC()
			f.PubKey = math.MaxUint32 + 1
			return f
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			var res Results
			var err error
			if name == "reset" {
				res, err = New(f).Reset(context.Background(), testUser, 2)
			} else {
				res, err = New(f).Stop(context.Background(), testUser, true, true, 2)
			}
			if err != nil {
				t.Fatalf("command: %v", err)
			}
			if res[0].Outcome == OutcomeCommanded {
				t.Fatalf("result = %v, want a failure", res[0])
			}
			if !errors.Is(res[0].Err, ErrInvalidValue) {
				t.Errorf("err = %v, want it to wrap ErrInvalidValue", res[0].Err)
			}
			if strings.Contains(res[0].Err.Error(), "nothing to write") {
				t.Errorf("err = %v, want the public key named, not a substituted reason", res[0].Err)
			}
			if !strings.Contains(res[0].Err.Error(), "public key") {
				t.Errorf("err = %v, want it to name the public key", res[0].Err)
			}
		})
	}
}

// The remaining session timeout went into a log line as a call argument, so Go
// evaluated it whether or not the logger kept anything. With the default logger
// — which discards everything — every left-open session therefore cost an extra
// OPC read nobody would ever see, at the moment the server was already in
// trouble.
func TestSessionTimeoutIsNotReadWhenTheLogIsDiscarded(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved // the session is left open

	res, err := New(f, WithSessionPolling(msec, 5*msec)).
		Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Fatalf("err = %v, want it to wrap ErrSessionLeftOpen", res[0].Err)
	}
	if n := f.TimesRead(f.item(sessionTimeoutItem(2, SessionCtrl))); n != 0 {
		t.Errorf("the session timeout was read %d time(s) although the log is discarded", n)
	}
}

// With a logger attached the figure is read and reported, because that is the
// point of it.
func TestSessionTimeoutIsReadWhenSomebodyIsListening(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved
	f.SessionTO = 42

	rec, logger := newRecordingLogger()
	if _, err := New(f, WithLogger(logger), WithSessionPolling(msec, 5*msec)).
		Stop(context.Background(), testUser, true, true, 2); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := f.TimesRead(f.item(sessionTimeoutItem(2, SessionCtrl))); n == 0 {
		t.Error("the session timeout was not read although a logger is attached")
	}
	if got := rec.text(); !strings.Contains(got, "42s") {
		t.Errorf("the remaining timeout was not logged:\n%s", got)
	}
}

// If only the last step fails, the submit was written and confirmed: the server
// holds the value and has committed it, and only the confirmation that the
// session wound down is missing. Reporting that as a plain failure invites a
// retry, and a retry of a Start or a Reset is a second command to a plant that
// may already have had one.
func TestSubmittedButUnfinishedSessionIsReportedAsUncertain(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	// The submit is accepted, but the session never reports its final state.
	f.StuckAt[2] = SessionParameterInput

	res, err := New(f, WithSessionPolling(time.Millisecond, 5*time.Millisecond)).
		Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].InRequestedState() {
		t.Fatal("a session that never completed must not report success")
	}
	if !f.Wrote(sessionSubmitItem(2, SessionCtrl)) {
		t.Fatal("the test needs the submit to have been written")
	}
	if !errors.Is(res[0].Err, ErrOutcomeUncertain) {
		t.Errorf("err = %v, want it to wrap ErrOutcomeUncertain", res[0].Err)
	}
}

// A failure before the submit is not uncertain at all: nothing was committed,
// and saying otherwise would make every failure unactionable.
func TestFailureBeforeTheSubmitIsNotUncertain(t *testing.T) {
	for name, f := range map[string]*fakeOPC{
		"never reserved": func() *fakeOPC {
			f := newFakeOPC()
			f.Ctrl[2] = uint64(CtrlStart)
			f.StuckAt[2] = SessionFree
			return f
		}(),
		"never reached parameter input": func() *fakeOPC {
			f := newFakeOPC()
			f.Ctrl[2] = uint64(CtrlStart)
			f.StuckAt[2] = SessionReserved
			return f
		}(),
		"submit refused": func() *fakeOPC {
			f := newFakeOPC()
			f.Ctrl[2] = uint64(CtrlStart)
			f.WriteBad[sessionSubmitItem(2, SessionCtrl)] = "E_ACCESS_DENIED"
			return f
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			res, err := New(f, WithSessionPolling(time.Millisecond, 5*time.Millisecond)).
				Stop(context.Background(), testUser, true, true, 2)
			if err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if errors.Is(res[0].Err, ErrOutcomeUncertain) {
				t.Errorf("err = %v, but nothing was ever committed", res[0].Err)
			}
		})
	}
}

// A command that completes is certain, so the marker must not appear on it.
func TestASuccessfulCommandIsNotUncertain(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded || res[0].Err != nil {
		t.Fatalf("result = %v, want a clean commanded", res[0])
	}
}

// CtrlStart, RbhSetStandard and IceDetLampOff all write 0, and an item that was
// never written reads back exactly the same. The private key is never drawn as
// zero, so where the server reports it back it is what shows whose write the
// session holds.
func TestZeroValueReadBackIsCheckedAgainstThePrivateKey(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90) // stopped, so a start is needed
	// The value matches what Start writes, but the key belongs to nobody.
	f.RawValues[setCtrlItem(2)] = []uint64{uint64(CtrlStart), 9999, 4711}

	res, err := New(f).Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatalf("result = %v: the session holds another client's key", res[0])
	}
	if !errors.Is(res[0].Err, ErrParameterNotAccepted) {
		t.Errorf("err = %v, want it to wrap ErrParameterNotAccepted", res[0].Err)
	}
	if f.Wrote(sessionSubmitItem(2, SessionCtrl)) {
		t.Error("a session holding another client's parameters was submitted")
	}
}

// A server that reports the key back correctly is verified, zero value or not.
func TestZeroValueReadBackPassesWithTheOwnPrivateKey(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90)
	// The fake echoes the whole written array, keys included.
	res, err := New(f).Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded", res[0])
	}
}

// A server that reports only the value back cannot answer the question at all.
// The write confirmation is then the remaining evidence, and saying so in the
// log is the difference between a check that passed and a check that could not
// be carried out.
func TestZeroValueReadBackWithoutAKeyIsLogged(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90)
	f.RawValues[setCtrlItem(2)] = []uint64{uint64(CtrlStart)} // value only

	rec, logger := newRecordingLogger()
	res, err := New(f, WithLogger(logger)).Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded: the value did read back", res[0])
	}
	if got := rec.text(); !strings.Contains(got, "cannot confirm a value of zero") {
		t.Errorf("the gap in the evidence was not logged:\n%s", got)
	}
}

// A non-zero value needs no key to be conclusive, so nothing is logged for it.
func TestNonZeroValueReadBackNeedsNoKey(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.RawValues[setCtrlItem(2)] = []uint64{uint64(CtrlStop90)}

	rec, logger := newRecordingLogger()
	res, err := New(f, WithLogger(logger)).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded", res[0])
	}
	if got := rec.text(); strings.Contains(got, "cannot confirm a value of zero") {
		t.Errorf("a conclusive read-back was reported as inconclusive:\n%s", got)
	}
}

// The other half of the same defect: success must follow from a value having been written, not
// from the session reaching its final state.
func TestCommandedRequiresAWrite(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[3] = uint64(CtrlStart)
	f.WriteBad[setCtrlItem(3)] = "E_ACCESS_DENIED"

	res, err := New(f).Stop(context.Background(), testUser, true, true, 3)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatal("SetCtrl was rejected by the server, yet the plant was reported as commanded")
	}
	if !errors.Is(res[0].Err, ErrItemFault) {
		t.Errorf("err = %v, want it to wrap ErrItemFault", res[0].Err)
	}
}

// A session that was reserved but could not be completed must not be
// abandoned silently.
func TestUnfinishedSessionIsReported(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved // never advances to "parameter input"

	c := New(f, WithSessionPolling(time.Millisecond, 10*time.Millisecond))
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatal("a session that never completed was reported as commanded")
	}
	if !errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Errorf("err = %v, want it to wrap ErrSessionLeftOpen", res[0].Err)
	}
}

// With a release function configured, an unfinished session is handed to it.
func TestSessionReleaseHookIsCalled(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved

	var released []uint8
	c := New(f,
		WithSessionPolling(time.Millisecond, 10*time.Millisecond),
		WithSessionRelease(func(_ context.Context, _ Transport, plant uint8, kind SessionKind,
			priv uint16, pub uint64) error {
			released = append(released, plant)
			if kind != SessionCtrl {
				t.Errorf("kind = %v, want %v", kind, SessionCtrl)
			}
			if priv == 0 || pub == 0 {
				t.Errorf("release got empty credentials: priv=%d pub=%d", priv, pub)
			}
			return nil
		}))
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(released) != 1 || released[0] != 2 {
		t.Fatalf("release called for %v, want [2]", released)
	}
	if errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Error("session was released, but still reported as left open")
	}
}

// A session that completed normally is not treated as left open.
func TestCompletedSessionIsNotReportedAsLeftOpen(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded || res[0].Err != nil {
		t.Fatalf("result = %v, want a clean commanded result", res[0])
	}
	if got := f.Ctrl[2]; got != uint64(CtrlStop90) {
		t.Errorf("plant state = %d, want %d", got, CtrlStop90)
	}
}

// Every step of the session procedure batches all plants into one request.
// v1 sent one request per plant for the session request, the public key, the
// value and the submit.
func TestSessionStepsAreBatched(t *testing.T) {
	f := newFakeOPC()
	plants := []uint8{1, 2, 3, 4, 5, 6, 7, 8}
	for _, p := range plants {
		f.Ctrl[p] = uint64(CtrlStart)
	}
	res, err := New(f).Stop(context.Background(), testUser, true, true, plants...)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.InRequestedState() {
		t.Fatalf("not all plants stopped: %v", res)
	}
	// 4 session state polls, 1 control state read, 1 public key read and the two
	// session id checks the Enercon session schema requires.
	if f.ReadCalls > 8 {
		t.Errorf("%d read requests for %d plants, want at most 8", f.ReadCalls, len(plants))
	}
	// SessionRequest, SetCtrl, SessionSubmit.
	if f.WriteCalls > 3 {
		t.Errorf("%d write requests for %d plants, want at most 3", f.WriteCalls, len(plants))
	}
}

// H10: v1 slept in its retry loops without watching the context, so a cancelled
// context did not stop a command until the retry budget was spent.
func TestContextCancellationStopsPolling(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionFree // the session never gets reserved

	ctx, cancel := context.WithCancel(context.Background())
	c := New(f, WithSessionPolling(20*time.Millisecond, time.Hour))
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := c.Stop(ctx, testUser, true, true, 2)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("command ran for %s after cancellation", elapsed)
	}
	if err == nil && res[0].Err == nil {
		t.Fatal("cancellation produced no error")
	}
}

// A session another client holds is reported with a specific, classifiable
// error rather than a generic string.
func TestOccupiedSessionIsClassified(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Occupied[2] = true

	c := New(f, WithSessionPolling(time.Millisecond, 5*time.Millisecond))
	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, ErrSessionOccupied) {
		t.Fatalf("err = %v, want it to wrap ErrSessionOccupied", res[0].Err)
	}
	var sessErr *SessionStateError
	if !errors.As(res[0].Err, &sessErr) {
		t.Fatal("err does not carry a *SessionStateError")
	}
	if sessErr.Got != SessionOccupied || sessErr.PlantNo != 2 {
		t.Errorf("SessionStateError = %+v", sessErr)
	}
}

// Reset runs the same batched session machine as a control command.
func TestResetUsesTheResetBranch(t *testing.T) {
	f := newFakeOPC()
	res, err := New(f).Reset(context.Background(), testUser, 2, 5)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !res.InRequestedState() {
		t.Fatalf("reset failed: %v", res)
	}
	for _, plant := range []uint8{2, 5} {
		if !f.Wrote(setResetItem(plant)) {
			t.Errorf("SetReset not written for plant %d; writes: %v", plant, f.WrittenNames())
		}
	}
	if f.WriteCalls > 3 {
		t.Errorf("%d write requests, want the same batching as a control command", f.WriteCalls)
	}
}

// 3.4.1 and Abb. 9: the client must verify its session id, because that is the
// only way to tell its own reservation from another client's. A mismatch means
// the session belongs to somebody else and must not be written to.
func TestSpecSessionIDMismatchAbortsTheCommand(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.SessionID[2] = 99 // the client never draws 99

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, ErrSessionIDMismatch) {
		t.Fatalf("err = %v, want it to wrap ErrSessionIDMismatch", res[0].Err)
	}
	if f.Wrote(setCtrlItem(2)) {
		t.Error("a command was written into a session belonging to another client")
	}
	if f.Wrote(sessionSubmitItem(2, SessionCtrl)) {
		t.Error("another client's session was submitted")
	}
	// The session is not ours, so it must not be reported as ours to release.
	if errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Error("another client's session was reported as left open by this client")
	}
}

// A server that does not report the session id must not break every command;
// the client never draws 0, so 0 means "not reported".
func TestSpecUnreportedSessionIDDoesNotFail(t *testing.T) {
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

func TestSpecSessionIDIsNeverZero(t *testing.T) {
	for i := 0; i < 500; i++ {
		cred, err := newSessionCred()
		if err != nil {
			t.Fatal(err)
		}
		if cred.sessionID == 0 {
			t.Fatal("session id 0 is reserved for \"the server did not report one\"")
		}
		if cred.sessionID > 19 {
			t.Fatalf("session id %d out of range", cred.sessionID)
		}
	}
}

// Tab. 80: "the value that was set can also be read back for checking". Reading
// it back before submitting proves the command in the session is the one that
// was requested.
func TestSpecParameterReadbackMismatchAbortsTheCommand(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.RawValues[setCtrlItem(2)] = []uint64{uint64(CtrlStart), 0, 0} // server holds something else

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, ErrParameterNotAccepted) {
		t.Fatalf("err = %v, want it to wrap ErrParameterNotAccepted", res[0].Err)
	}
	if f.Wrote(sessionSubmitItem(2, SessionCtrl)) {
		t.Error("a session holding the wrong value was submitted")
	}
}

// 3.4.1 / Tab. 79: SessionRequest carries session id, user id and private key,
// in that order; SessionSubmit carries private key and public key; the Set…
// items carry value, private key and public key.
func TestSpecWrittenArrayLayout(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	const userID = uint64(169592065) // the user id from the data sheet's example

	if _, err := New(f).Stop(context.Background(), userID, false, true, 2); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	byName := map[string][]uint32{}
	for _, w := range f.Writes {
		byName[w.ItemName] = w.Value
	}
	request := byName[sessionRequestItem(2, SessionCtrl)]
	if len(request) != 3 {
		t.Fatalf("SessionRequest has %d elements, want 3", len(request))
	}
	if request[0] == 0 || request[0] > 19 {
		t.Errorf("SessionRequest[0] (session id) = %d", request[0])
	}
	if uint64(request[1]) != userID {
		t.Errorf("SessionRequest[1] (user id) = %d, want %d", request[1], userID)
	}
	if request[2] == 0 {
		t.Error("SessionRequest[2] (private key) must not be zero")
	}

	setCtrl := byName[setCtrlItem(2)]
	if len(setCtrl) != 3 {
		t.Fatalf("SetCtrl has %d elements, want 3", len(setCtrl))
	}
	if setCtrl[0] != uint32(CtrlStop60) {
		t.Errorf("SetCtrl[0] (control value) = %d, want %d", setCtrl[0], CtrlStop60)
	}
	if setCtrl[1] != request[2] {
		t.Errorf("SetCtrl[1] (private key) = %d, want the session's %d", setCtrl[1], request[2])
	}
	if uint64(setCtrl[2]) != f.PubKey {
		t.Errorf("SetCtrl[2] (public key) = %d, want %d", setCtrl[2], f.PubKey)
	}

	submit := byName[sessionSubmitItem(2, SessionCtrl)]
	if len(submit) != 2 {
		t.Fatalf("SessionSubmit has %d elements, want 2", len(submit))
	}
	if submit[0] != request[2] || uint64(submit[1]) != f.PubKey {
		t.Errorf("SessionSubmit = %v, want [privateKey publicKey]", submit)
	}
}
