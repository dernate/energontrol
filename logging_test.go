package energontrol

import (
	"bytes"
	"context"
	"log/slog"
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

// A plant node the package cannot address is logged as well as reported, so it
// shows up in an operator's log and not only in a struct field nobody reads.
func TestUnsupportedPlantNodeIsLogged(t *testing.T) {
	f := parkFake()
	f.ExtraBranches = []string{"Loc/Wec/Plant300"}

	rec, logger := newRecordingLogger()
	if _, err := New(f, WithLogger(logger)).Turbines(context.Background()); err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	if got := rec.text(); !strings.Contains(got, "Plant300") {
		t.Errorf("the unaddressable plant was not logged:\n%s", got)
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

// The single-line renderers are what a caller puts in its own log; they are
// exercised only by the live tests otherwise.
func TestStatusStringRenderers(t *testing.T) {
	if got := RbhStatusString(RbhInstalled | RbhAutoOffWEA | RbhManualOnSCADA); !strings.Contains(
		got, "Heating manually on (SCADA)") {
		t.Errorf("RbhStatusString = %q", got)
	}
	if got := RbhStatusString(0); got != "No rotor blade heating installed" {
		t.Errorf("RbhStatusString(0) = %q", got)
	}
	if got := IceDetStatusString(IceDetPowerCurve | IceDetExternalSCADA); !strings.Contains(
		got, "power curve") {
		t.Errorf("IceDetStatusString = %q", got)
	}
	if got := IceDetStatusString(0); got != "No ice detected" {
		t.Errorf("IceDetStatusString(0) = %q", got)
	}
	// The state types render themselves for a log line, including the unknown
	// case, which must not print a state it does not have.
	unknown := PlantState{PlantNo: 4, Ctrl: CtrlStart, Err: ErrItemMissing}
	if got := unknown.String(); !strings.Contains(got, "unknown") || strings.Contains(got, "Start") {
		t.Errorf("PlantState.String() = %q, want it to report the unknown state only", got)
	}
	known := PlantState{PlantNo: 4, Ctrl: CtrlStop90}
	if got := known.String(); !strings.Contains(got, "Stop90") {
		t.Errorf("PlantState.String() = %q", got)
	}
	if got := (RbhState{PlantNo: 1, Err: ErrItemMissing}).String(); !strings.Contains(got, "unknown") {
		t.Errorf("RbhState.String() = %q", got)
	}
	if got := (IceDetState{PlantNo: 1, Status: IceDetPreventive}).String(); !strings.Contains(
		got, "Preventive") {
		t.Errorf("IceDetState.String() = %q", got)
	}
}

// Outcome and the error types have to name themselves in a log line too.
func TestOutcomeAndErrorRendering(t *testing.T) {
	for outcome, want := range map[Outcome]string{
		OutcomeCommanded:      "commanded",
		OutcomeAlreadyInState: "already in state",
		OutcomeNotPermitted:   "not permitted",
		OutcomeFailed:         "failed",
		Outcome(99):           "Outcome(99)",
	} {
		if got := outcome.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", int(outcome), got, want)
		}
	}
	if got := (&SessionStateError{PlantNo: 3, Want: SessionFree, Got: SessionOccupied}).Error(); !strings.Contains(
		got, "plant 3") || !strings.Contains(got, "occupied") {
		t.Errorf("SessionStateError.Error() = %q", got)
	}
	// An ItemError without a detail must not print an empty bracket.
	if got := (&ItemError{ItemName: "X", Reason: ErrItemMissing}).Error(); strings.Contains(got, "()") {
		t.Errorf("ItemError.Error() = %q", got)
	}
	if got := reasonText(nil); got != "unknown reason" {
		t.Errorf("reasonText(nil) = %q", got)
	}
	// A reason that is not one of this package's sentinels keeps its own text.
	if got := reasonText(errTransport); got != errTransport.Error() {
		t.Errorf("reasonText = %q, want %q", got, errTransport.Error())
	}
	if got := CtrlValue(77).String(); got != "CtrlValue(77)" {
		t.Errorf("CtrlValue(77).String() = %q", got)
	}
	if got := RbhValue(77).String(); got != "RbhValue(77)" {
		t.Errorf("RbhValue(77).String() = %q", got)
	}
	if got := IceDetValue(77).String(); got != "IceDetValue(77)" {
		t.Errorf("IceDetValue(77).String() = %q", got)
	}
}
