package energontrol

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Each test in this file pins down one of the defects the v1 audit found. The
// name says which one.

const testUser = uint64(1234)

// K1: ControlAndRbh built its per-plant action flags over the unfiltered plant
// list and the session procedure then indexed them with the position in the
// filtered list. As soon as one plant needed no command, the remaining plants
// were checked against the wrong flag, their SetRbh write was skipped, and the
// session still ran to completion and reported success.
func TestControlAndRbhWritesForEveryCommandedPlant(t *testing.T) {
	f := newFakeOPC()
	// Value 10 means "suppress the automatic system and heat manually", so the
	// state that already satisfies it carries both bits.
	f.Rbh[2] = RbhInstalled | RbhAutoOffWEA | RbhManualOnSCADA // already on, needs no command
	f.Rbh[5] = RbhInstalled | RbhAutoDeicingAllowed            // off, needs the command
	f.Rbh[7] = RbhInstalled | RbhAutoDeicingAllowed            // off, needs the command

	res, err := New(f).ControlAndRbh(context.Background(), testUser,
		ControlAndRbhValue{SetRbhValue: true, RbhValue: RbhSetManualOn}, 2, 5, 7)
	if err != nil {
		t.Fatalf("ControlAndRbh: %v", err)
	}
	if got, want := res[0].Outcome, OutcomeAlreadyInState; got != want {
		t.Errorf("plant 2: outcome = %v, want %v", got, want)
	}
	for _, i := range []int{1, 2} {
		if res[i].Outcome != OutcomeCommanded {
			t.Errorf("plant %d: outcome = %v (%v), want %v",
				res[i].PlantNo, res[i].Outcome, res[i].Err, OutcomeCommanded)
		}
	}
	for _, plant := range []uint8{5, 7} {
		if !f.Wrote(setRbhItem(plant)) {
			t.Errorf("plant %d reported as commanded but SetRbh was never written; writes: %v",
				plant, f.WrittenNames())
		}
	}
	if f.Wrote(setRbhItem(2)) {
		t.Error("plant 2 needed no command but SetRbh was written for it")
	}
}

// K1, second half: success must follow from a value having been written, not
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

// K2: Start reported started=true and err=nil for plants that Enercon had
// stopped with higher rights, and for plants whose state was unknown because of
// a communication error.
func TestStartDoesNotClaimSuccessForUncommandablePlants(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[3] = uint64(CtrlStopEnercon)
	f.Ctrl[7] = uint64(CtrlCommError)
	f.Ctrl[9] = uint64(CtrlStart)

	res, err := New(f).Start(context.Background(), testUser, 3, 7, 9)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cases := []struct {
		idx     int
		outcome Outcome
		want    error
	}{
		{0, OutcomeNotPermitted, ErrPlantUnderEnerconControl},
		{1, OutcomeNotPermitted, ErrPlantCommunication},
	}
	for _, tc := range cases {
		r := res[tc.idx]
		if r.Outcome != tc.outcome {
			t.Errorf("plant %d: outcome = %v, want %v", r.PlantNo, r.Outcome, tc.outcome)
		}
		if !errors.Is(r.Err, tc.want) {
			t.Errorf("plant %d: err = %v, want it to wrap %v", r.PlantNo, r.Err, tc.want)
		}
		if r.InRequestedState() {
			t.Errorf("plant %d: InRequestedState() = true, but the plant is not running", r.PlantNo)
		}
	}
	if res[2].Outcome != OutcomeAlreadyInState || !res[2].InRequestedState() {
		t.Errorf("plant 9: %v, want already-in-state", res[2])
	}
	if len(f.WrittenNames()) != 0 {
		t.Errorf("no session should have been opened, but writes happened: %v", f.WrittenNames())
	}
}

// K2: a plant in CtrlCommError must never be reported as stopped either — its
// state is unknown, and "unknown" is not "stopped".
func TestStopDoesNotClaimStoppedOnCommunicationError(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[4] = uint64(CtrlCommError)

	res, err := New(f).Stop(context.Background(), testUser, false, false, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].InRequestedState() {
		t.Fatal("a plant with a communication error was reported as stopped")
	}
	if !errors.Is(res[0].Err, ErrPlantCommunication) {
		t.Errorf("err = %v, want it to wrap ErrPlantCommunication", res[0].Err)
	}
}

// K2: a plant Enercon already stopped does satisfy a plain stop request — the
// caller's goal is met — but not a forced request for a specific stop state.
func TestStopAcceptsEnerconStopUnlessForced(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[4] = uint64(CtrlStopEnercon)

	res, err := New(f).Stop(context.Background(), testUser, true, false, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeAlreadyInState {
		t.Errorf("unforced stop: outcome = %v, want already-in-state", res[0].Outcome)
	}

	res, err = New(f).Stop(context.Background(), testUser, true, true, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeNotPermitted {
		t.Errorf("forced stop: outcome = %v, want not-permitted", res[0].Outcome)
	}
	if len(f.WrittenNames()) != 0 {
		t.Errorf("forced stop on an Enercon-controlled plant must not open a session; writes: %v",
			f.WrittenNames())
	}
}

// K3: the value type of an OPC item depends on the xsi:type the server chose.
// v1 asserted it with .(uint64) and .(uint16) and panicked the calling process
// when a server disagreed.
func TestUnexpectedValueTypesDoNotPanic(t *testing.T) {
	for _, typ := range []string{"uint16", "uint32", "int", "float64", "string", "nil"} {
		t.Run(typ, func(t *testing.T) {
			f := newFakeOPC()
			f.Ctrl[2] = uint64(CtrlStart)
			f.ValueType[ctrlItem(2)] = typ
			f.ValueType[sessionStateItem(2, SessionCtrl)] = typ

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic instead of an error: %v", r)
				}
			}()
			res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
			switch typ {
			case "string", "nil":
				// Not convertible: must surface as an error, never as a state.
				if err == nil && res[0].Err == nil {
					t.Fatal("an unusable value was accepted")
				}
				if err == nil && !errors.Is(res[0].Err, ErrUnexpectedType) {
					t.Errorf("err = %v, want it to wrap ErrUnexpectedType", res[0].Err)
				}
			default:
				if err != nil {
					t.Fatalf("numeric type %s rejected: %v", typ, err)
				}
				if res[0].Outcome != OutcomeCommanded {
					t.Errorf("numeric type %s: outcome = %v (%v)", typ, res[0].Outcome, res[0].Err)
				}
			}
		})
	}
}

// K4: v1 ignored the quality field, so a value the server had explicitly marked
// as unusable was used as a process value.
func TestBadQualityIsRejected(t *testing.T) {
	for _, q := range []string{"bad", "uncertain", "badConfigurationError"} {
		t.Run(q, func(t *testing.T) {
			f := newFakeOPC()
			f.Ctrl[2] = uint64(CtrlStart)
			f.Quality[ctrlItem(2)] = q

			res, err := New(f).Start(context.Background(), testUser, 2)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if res[0].InRequestedState() {
				t.Fatalf("a value with quality %q was accepted as the plant state", q)
			}
			if !errors.Is(res[0].Err, ErrBadQuality) {
				t.Errorf("err = %v, want it to wrap ErrBadQuality", res[0].Err)
			}
		})
	}
	t.Run("goodLocalOverride is usable", func(t *testing.T) {
		f := newFakeOPC()
		f.Ctrl[2] = uint64(CtrlStart)
		f.Quality[ctrlItem(2)] = "goodLocalOverride"
		res, err := New(f).Start(context.Background(), testUser, 2)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !res[0].InRequestedState() {
			t.Errorf("good quality rejected: %v", res[0])
		}
	})
}

// K4: an item the server flags with a ResultID must not be read as a state.
func TestItemFaultIsRejected(t *testing.T) {
	f := newFakeOPC()
	f.ItemFault[ctrlItem(2)] = "E_UNKNOWN_ITEM_NAME"

	res, err := New(f).Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !errors.Is(res[0].Err, ErrItemFault) {
		t.Fatalf("err = %v, want it to wrap ErrItemFault", res[0].Err)
	}
}

// K5: a response that omits an item left v1 with the zero value — plant 0,
// CtrlState 0 — and CtrlState 0 means "running".
func TestMissingItemIsAnErrorNotARunningPlant(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90)
	f.Ctrl[5] = uint64(CtrlStop90)
	f.Omit[ctrlItem(5)] = true

	res, err := New(f).Start(context.Background(), testUser, 2, 5)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !errors.Is(res[1].Err, ErrItemMissing) {
		t.Fatalf("plant 5: err = %v, want it to wrap ErrItemMissing", res[1].Err)
	}
	if res[1].InRequestedState() {
		t.Error("a plant whose state was never returned was reported as running")
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Errorf("plant 2 should still be commanded, got %v (%v)", res[0].Outcome, res[0].Err)
	}
}

// K5: OPC XML-DA does not promise that a response lists items in request order.
// Correlation is by ClientItemHandle, so the order must not matter.
func TestResponseOrderDoesNotMatter(t *testing.T) {
	f := newFakeOPC()
	f.Reverse = true
	f.Ctrl[2] = uint64(CtrlStart)  // running
	f.Ctrl[5] = uint64(CtrlStop90) // stopped
	f.Ctrl[7] = uint64(CtrlStop60) // stopped

	states, err := New(f).PlantCtrlState(context.Background(), 2, 5, 7)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	want := map[uint8]CtrlValue{2: CtrlStart, 5: CtrlStop90, 7: CtrlStop60}
	for _, s := range states {
		if s.Ctrl != want[s.PlantNo] {
			t.Errorf("plant %d: state %s, want %s", s.PlantNo, s.Ctrl, want[s.PlantNo])
		}
	}
}

// K5: without a handle and without an item name a response cannot be matched to
// the request. Guessing by position could command the wrong turbine, so it is
// refused.
func TestUncorrelatableResponseIsRefused(t *testing.T) {
	f := newFakeOPC()
	f.NoHandles = true
	f.Ctrl[2] = uint64(CtrlStart)
	f.Ctrl[5] = uint64(CtrlStart)

	// With no handle, the item name still allows correlation.
	if _, err := New(f).PlantCtrlState(context.Background(), 2, 5); err != nil {
		t.Fatalf("correlation by ItemName failed: %v", err)
	}

	// With neither, the response is refused rather than matched by position.
	blind := newFakeOPC()
	blind.NoHandles = true
	blind.StripNames = true
	blind.Ctrl[2] = uint64(CtrlStart)
	if _, err := New(blind).PlantCtrlState(context.Background(), 2); !errors.Is(err, ErrUncorrelatable) {
		t.Fatalf("err = %v, want it to wrap ErrUncorrelatable", err)
	}
}

// H2: v1 answered (false, nil) for a server that was reachable but not running,
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

// H1: a session that was reserved but could not be completed must not be
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

// H1: with a release function configured, an unfinished session is handed to it.
func TestSessionReleaseHookIsCalled(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved

	var released []uint8
	c := New(f,
		WithSessionPolling(time.Millisecond, 10*time.Millisecond),
		WithSessionRelease(func(_ context.Context, _ OpcClient, plant uint8, kind SessionKind,
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

// H1: a session that completed normally is not treated as left open.
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

// H3: ControlAndRbh passed the caller's values straight through to the plant
// controller, including values reserved for the system.
func TestReservedValuesAreRejected(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	ctx := context.Background()

	for _, v := range []CtrlValue{CtrlStopEnercon, CtrlStop60Enercon, CtrlCommError, CtrlValue(42)} {
		_, err := New(f).ControlAndRbh(ctx, testUser,
			ControlAndRbhValue{SetCtrlValue: true, CtrlValue: v}, 2)
		if !errors.Is(err, ErrInvalidValue) {
			t.Errorf("CtrlValue %d: err = %v, want it to wrap ErrInvalidValue", v, err)
		}
	}
	if _, err := New(f).ControlAndRbh(ctx, testUser,
		ControlAndRbhValue{SetRbhValue: true, RbhValue: RbhValue(7)}, 2); !errors.Is(err, ErrInvalidValue) {
		t.Errorf("RbhValue 7: err = %v, want it to wrap ErrInvalidValue", err)
	}
	if len(f.WrittenNames()) != 0 {
		t.Errorf("a rejected value must not reach the plant; writes: %v", f.WrittenNames())
	}
}

// H3: a command that requests nothing is an error, not a success for every plant.
func TestEmptyCommandIsRejected(t *testing.T) {
	f := newFakeOPC()
	_, err := New(f).ControlAndRbh(context.Background(), testUser, ControlAndRbhValue{}, 2, 5)
	if !errors.Is(err, ErrNothingRequested) {
		t.Fatalf("err = %v, want it to wrap ErrNothingRequested", err)
	}
}

// H9: every step of the session procedure batches all plants into one request.
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

// Duplicate plants would open two competing sessions on the same turbine.
func TestPlantListValidation(t *testing.T) {
	f := newFakeOPC()
	ctx := context.Background()
	if _, err := New(f).Start(ctx, testUser); !errors.Is(err, ErrNoPlants) {
		t.Errorf("empty list: err = %v, want ErrNoPlants", err)
	}
	if _, err := New(f).Start(ctx, testUser, 2, 5, 2); !errors.Is(err, ErrDuplicatePlant) {
		t.Errorf("duplicate: err = %v, want ErrDuplicatePlant", err)
	}
}

// The heating of a plant that has none cannot be commanded, and must not be
// reported as if it had been.
func TestRbhUnavailableIsNotPermitted(t *testing.T) {
	f := newFakeOPC()
	f.Rbh[2] = 0 // no bits set: bit 15 clear, so no heating installed
	f.Rbh[5] = RbhNotInstalledValue

	res, err := New(f).RbhOn(context.Background(), testUser, 2, 5)
	if err != nil {
		t.Fatalf("RbhOn: %v", err)
	}
	for _, r := range res {
		if r.Outcome != OutcomeNotPermitted || !errors.Is(r.Err, ErrRbhUnavailable) {
			t.Errorf("plant %d: %v, want not-permitted with ErrRbhUnavailable", r.PlantNo, r)
		}
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

// One plant failing must not take the rest of the park with it.
func TestOnePlantFailureDoesNotFailTheBatch(t *testing.T) {
	f := newFakeOPC()
	for _, p := range []uint8{2, 5, 7} {
		f.Ctrl[p] = uint64(CtrlStart)
	}
	f.ItemFault[ctrlItem(5)] = "E_UNKNOWN_ITEM_NAME"

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2, 5, 7)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded || res[2].Outcome != OutcomeCommanded {
		t.Errorf("healthy plants were dragged down: %v", res)
	}
	if res[1].Err == nil {
		t.Error("the faulted plant was not reported")
	}
	if res.Err() == nil {
		t.Error("Results.Err() should surface the failed plant")
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

// Every command must yield exactly one result per requested plant, in order.
func TestResultsCoverEveryPlantInOrder(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[9] = uint64(CtrlStart)       // will be commanded
	f.Ctrl[3] = uint64(CtrlStop90)      // already stopped
	f.Ctrl[6] = uint64(CtrlStopEnercon) // not permitted
	f.Omit[ctrlItem(1)] = true          // unreadable

	plants := []uint8{9, 3, 6, 1}
	res, err := New(f).Stop(context.Background(), testUser, true, true, plants...)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(res) != len(plants) {
		t.Fatalf("got %d results for %d plants", len(res), len(plants))
	}
	for i, p := range plants {
		if res[i].PlantNo != p {
			t.Errorf("result %d is for plant %d, want %d", i, res[i].PlantNo, p)
		}
	}
	want := []Outcome{OutcomeCommanded, OutcomeAlreadyInState, OutcomeNotPermitted, OutcomeFailed}
	for i, w := range want {
		if res[i].Outcome != w {
			t.Errorf("plant %d: outcome = %v, want %v (%v)", res[i].PlantNo, res[i].Outcome, w, res[i].Err)
		}
	}
}

// A stale value must not drive a control decision once the check is enabled.
func TestStaleValueIsRejectedWhenEnabled(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Timestamps = true

	c := New(f, WithMaxStateAge(time.Second))
	c.now = func() time.Time { return time.Now().Add(time.Hour) }

	res, err := c.Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !errors.Is(res[0].Err, ErrStaleValue) {
		t.Fatalf("err = %v, want it to wrap ErrStaleValue", res[0].Err)
	}
}
