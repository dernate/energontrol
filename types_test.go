package energontrol

// The result types a caller reads a command's outcome from.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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

func TestResultsHelpers(t *testing.T) {
	boom := errors.New("boom")
	rs := Results{
		{PlantNo: 1, Outcome: OutcomeCommanded},
		{PlantNo: 2, Outcome: OutcomeAlreadyInState},
	}
	if !rs.InRequestedState() || rs.Err() != nil {
		t.Errorf("all-good results: InRequestedState=%t err=%v", rs.InRequestedState(), rs.Err())
	}
	rs = append(rs, PlantResult{PlantNo: 3, Outcome: OutcomeFailed, Err: boom})
	if rs.InRequestedState() {
		t.Error("a failed plant must make InRequestedState false")
	}
	if err := rs.Err(); !errors.Is(err, boom) {
		t.Errorf("Err() = %v, want it to wrap boom", err)
	}
	if Results(nil).InRequestedState() {
		t.Error("an empty result set is not a fulfilled command")
	}
	// A not-permitted plant carries an error and is not in the requested state.
	np := PlantResult{PlantNo: 4, Outcome: OutcomeNotPermitted, Err: ErrPlantCommunication}
	if np.InRequestedState() {
		t.Error("not-permitted must not count as in-state")
	}
}

// The state slices carry their own aggregate helpers, so a caller who wants the
// old all-or-nothing behaviour has one call for it.
func TestStateSliceHelpers(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90)
	f.Ctrl[5] = uint64(CtrlStart)
	f.ItemFault[ctrlItem(5)] = "E_UNKNOWN_ITEM_NAME"

	states, err := New(f).PlantCtrlState(context.Background(), 2, 5)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("got %d states for 2 plants", len(states))
	}
	// The readable plant is still readable.
	got, ok := states.Get(2)
	if !ok || got.Err != nil || got.Ctrl != CtrlStop90 {
		t.Errorf("plant 2 = %v (ok=%t), want %s", got, ok, CtrlStop90)
	}
	// The unreadable one carries its reason rather than a state.
	got, ok = states.Get(5)
	if !ok || got.Err == nil {
		t.Errorf("plant 5 = %v (ok=%t), want an error", got, ok)
	}
	if _, ok := states.Get(9); ok {
		t.Error("Get returned a plant that was never requested")
	}
	if states.Err() == nil {
		t.Error("Err() should surface the unreadable plant")
	}

	// And with every plant readable, Err is nil.
	f2 := newFakeOPC()
	f2.Ctrl[2] = uint64(CtrlStop90)
	clean, err := New(f2).PlantCtrlState(context.Background(), 2)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if clean.Err() != nil {
		t.Errorf("Err() = %v, want nil", clean.Err())
	}
}
