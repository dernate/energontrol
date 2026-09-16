package energontrol

// The commands and the decisions they take before any session is opened: which
// plants already satisfy a request, which cannot be commanded at all, and how
// the per-plant results are assembled.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCtrlSatisfied(t *testing.T) {
	cases := []struct {
		current CtrlValue
		want    CtrlValue
		force   bool
		ok      bool
	}{
		// Start is satisfied only by a running plant.
		{CtrlStart, CtrlStart, false, true},
		{CtrlStop60, CtrlStart, false, false},
		{CtrlStopEnercon, CtrlStart, false, false},
		{CtrlCommError, CtrlStart, false, false},
		// An unforced stop is satisfied by a state at least as stopped as the
		// one asked for. The same state always satisfies itself.
		{CtrlStop60, CtrlStop60, false, true},
		{CtrlStop90, CtrlStop90, false, true},
		// A deeper stop satisfies a shallower request: commanding it would open
		// the blades from 90° back to 60°.
		{CtrlStop90, CtrlStop60, false, true},
		// A shallower stop does not satisfy a deeper request. This is the case
		// that used to be reported as already-in-state while the plant stayed
		// at 60°.
		{CtrlStop60, CtrlStop90, false, false},
		// A stop Enercon made ranks with the angle it names, which is what lets
		// it be tolerated without 60° passing for 90°.
		{CtrlStopEnercon, CtrlStop60, false, true},
		{CtrlStopEnercon, CtrlStop90, false, true},
		{CtrlStop60Enercon, CtrlStop60, false, true},
		{CtrlStop60Enercon, CtrlStop90, false, false},
		{CtrlStart, CtrlStop90, false, false},
		// The gradient and species-protection stops end in the angle they name,
		// so a deeper one still satisfies a shallower request.
		{CtrlStop90, CtrlGradientStop60, false, true},
		{CtrlStop60, CtrlGradientStop90, false, false},
		// At the same angle, though, a different command is a different
		// operating mode and the plant can be commanded, so it is sent.
		{CtrlStop60, CtrlStopSpeciesProtection60, false, false},
		{CtrlStopSpeciesProtection60, CtrlStop60, false, false},
		{CtrlStopSpeciesProtection90, CtrlStop90, false, false},
		{CtrlGradientStop60, CtrlStopSpeciesProtection60, false, false},
		{CtrlStopSpeciesProtection90, CtrlStop60, false, true},
		// Neither a communication error nor a rejected value says where the
		// blades are, so neither satisfies any stop.
		{CtrlCommError, CtrlStop90, false, false},
		{CtrlCommError, CtrlStop60, false, false},
		{CtrlValueRejected, CtrlStop60, false, false},
		// The data sheet documents no resulting state for these two, so nothing
		// satisfies them and the command is always sent.
		{CtrlStop90, CtrlStopIceDetection, false, false},
		{CtrlStop90, CtrlStopShadowFlicker, false, false},
		// A forced stop wants exactly that state.
		{CtrlStop60, CtrlStop60, true, true},
		{CtrlStop60, CtrlStop90, true, false},
		{CtrlStop90, CtrlStop60, true, false},
		{CtrlStopEnercon, CtrlStop90, true, false},
		{CtrlStop90, CtrlGradientStop90, true, false},
		{CtrlStop90, CtrlStopIceDetection, true, false},
	}
	for _, tc := range cases {
		if got := ctrlSatisfied(tc.current, tc.want, tc.force); got != tc.ok {
			t.Errorf("ctrlSatisfied(%s, %s, force=%t) = %t, want %t",
				tc.current, tc.want, tc.force, got, tc.ok)
		}
	}
}

// The three heating rules, checked against the original Enercon expressions
// (St & 508) && !(St & 68608) for "on" and ((St&8) ^ (St&2)) && !(St&8) for
// "auto off".
func TestRbhSatisfied(t *testing.T) {
	cases := []struct {
		name   string
		status uint64
		want   RbhValue
		ok     bool
	}{
		{"automatic allowed is standard", RbhInstalled | RbhAutoDeicingAllowed, RbhSetStandard, true},
		{"automatic allowed is not auto-off", RbhInstalled | RbhAutoDeicingAllowed, RbhSetAutoOff, false},
		{"automatic allowed is not on", RbhInstalled | RbhAutoDeicingAllowed, RbhSetManualOn, false},

		{"suppressed is not standard", RbhInstalled | RbhAutoOffWEA, RbhSetStandard, false},
		{"suppressed is auto-off", RbhInstalled | RbhAutoOffWEA, RbhSetAutoOff, true},
		{"suppressed is not on", RbhInstalled | RbhAutoOffWEA, RbhSetManualOn, false},

		{"manual on is not standard", RbhInstalled | RbhAutoOffWEA | RbhManualOnSCADA, RbhSetStandard, false},
		{"manual on is not auto-off", RbhInstalled | RbhAutoOffWEA | RbhManualOnSCADA, RbhSetAutoOff, false},
		{"manual on is on", RbhInstalled | RbhAutoOffWEA | RbhManualOnSCADA, RbhSetManualOn, true},

		// RbhOn asks for "the heating runs", not for a particular bit, so a
		// plant already heating under automatic control satisfies it.
		{"automatic heating is on", RbhInstalled | RbhAutoDeicingAllowed | RbhHeatingInOperationSCADA, RbhSetManualOn, true},
		{"automatic heating is standard", RbhInstalled | RbhAutoDeicingAllowed | RbhHeatingInOperationSCADA, RbhSetStandard, true},

		{"a fault blocks on", RbhInstalled | RbhAutoOffWEA | RbhManualOnSCADA | RbhFault, RbhSetManualOn, false},
		{"missing supply blocks on", RbhInstalled | RbhAutoOffWEA | RbhManualOnSCADA | RbhNoSupplyPowerAvailable, RbhSetManualOn, false},
		{"not installed blocks on", RbhNotInstalledValue, RbhSetManualOn, false},
		{"not installed blocks standard", 0, RbhSetStandard, false},

		// The preset-duration command is a one-shot action with no status bit,
		// so no status can already satisfy it.
		{"preset duration is never satisfied", RbhInstalled | RbhHeatingWhenStoppedSCADA, RbhSetPresetDuration, false},
	}
	for _, tc := range cases {
		if got := rbhSatisfied(tc.status, tc.want); got != tc.ok {
			t.Errorf("%s: rbhSatisfied(%d, %s) = %t, want %t", tc.name, tc.status, tc.want, got, tc.ok)
		}
	}
}

// The simplified auto-off rule must agree with v1's literal translation of the
// original C expression for every possible combination of the two bits.
func TestRbhAutoOffMatchesOriginalExpression(t *testing.T) {
	original := func(a uint64) bool {
		return (a&RbhManualOnSCADA != 0) != ((a&RbhAutoOffWEA) != 0) && (a&RbhManualOnSCADA) == 0
	}
	for a := uint64(0); a < 64; a++ {
		// The installation bit is a separate question in v2, so set it here.
		status := a | RbhInstalled
		if got, want := rbhSatisfied(status, RbhSetAutoOff), original(a); got != want {
			t.Errorf("status %d: simplified = %t, original = %t", a, got, want)
		}
	}
}

func TestRbhStateError(t *testing.T) {
	if err := rbhStateError(0); !errors.Is(err, ErrRbhUnavailable) {
		t.Errorf("no access: %v", err)
	}
	if err := rbhStateError(RbhNotInstalledValue); !errors.Is(err, ErrRbhUnavailable) {
		t.Errorf("not installed: %v", err)
	}
	if err := rbhStateError(RbhInstalled | RbhAutoDeicingAllowed); err != nil {
		t.Errorf("installed heating reported as unavailable: %v", err)
	}
}

func TestValidatePlants(t *testing.T) {
	if err := validatePlants(nil); !errors.Is(err, ErrNoPlants) {
		t.Errorf("nil: %v", err)
	}
	if err := validatePlants([]uint8{1, 2, 3}); err != nil {
		t.Errorf("valid list rejected: %v", err)
	}
	if err := validatePlants([]uint8{1, 2, 1}); !errors.Is(err, ErrDuplicatePlant) {
		t.Errorf("duplicate: %v", err)
	}
}

// The same for a control command, where the omitted item is one of several
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

// An out-of-range user id is an argument error. It must be rejected before
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

// newResultSet kept the variadic slice the caller passed in, which for
// client.Start(ctx, id, plants...) is the caller's own backing array. Nothing
// wrote to it, but the order the results are reported in was read from it after
// the command had finished — so a caller reusing the slice got results
// attributed to plants it never asked about. lockPlants already copies for
// exactly this reason.
func TestResultSetDoesNotAliasTheCallersSlice(t *testing.T) {
	plants := []uint8{2, 5}
	rs := newResultSet(plants)
	rs.set(PlantResult{PlantNo: 2, Outcome: OutcomeAlreadyInState})
	rs.set(PlantResult{PlantNo: 5, Outcome: OutcomeAlreadyInState})

	plants[0] = 9 // the caller reuses its slice

	out := rs.list()
	if len(out) != 2 {
		t.Fatalf("results = %v, want two", out)
	}
	if out[0].PlantNo != 2 || out[1].PlantNo != 5 {
		t.Errorf("results = %v, want plants 2 and 5: the caller's slice was aliased", out)
	}
	for _, r := range out {
		if r.Err != nil {
			t.Errorf("plant %d: %v, want the recorded result", r.PlantNo, r.Err)
		}
	}
}

// ControlAndRbh built its per-plant action flags over the unfiltered plant
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

// Start reported started=true and err=nil for plants that Enercon had
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

// A plant in CtrlCommError must never be reported as stopped either — its
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

// A plant Enercon already stopped does satisfy a plain stop request — the
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

// ControlAndRbh passed the caller's values straight through to the plant
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

// A command that requests nothing is an error, not a success for every plant.
func TestEmptyCommandIsRejected(t *testing.T) {
	f := newFakeOPC()
	_, err := New(f).ControlAndRbh(context.Background(), testUser, ControlAndRbhValue{}, 2, 5)
	if !errors.Is(err, ErrNothingRequested) {
		t.Fatalf("err = %v, want it to wrap ErrNothingRequested", err)
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

// The preset-duration command has no status bit of its own, so it is always
// sent rather than skipped as "already in state".
func TestSpecPresetDurationIsAlwaysSent(t *testing.T) {
	f := newFakeOPC()
	f.Rbh[2] = RbhInstalled | RbhHeatingWhenStoppedSCADA

	res, err := New(f).ControlAndRbh(context.Background(), testUser,
		ControlAndRbhValue{SetRbhValue: true, RbhValue: RbhSetPresetDuration}, 2)
	if err != nil {
		t.Fatalf("ControlAndRbh: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded", res[0])
	}
	if !f.Wrote(setRbhItem(2)) {
		t.Error("SetRbh was not written")
	}
}

// The control values beyond start and the two stops must be reachable through
// the public API, not only through ControlAndRbh.
func TestSpecSetCtrlSendsTheDocumentedValues(t *testing.T) {
	for _, value := range []CtrlValue{
		CtrlGradientStop60, CtrlGradientStop90,
		CtrlStopIceDetection, CtrlStopShadowFlicker,
		CtrlStopSpeciesProtection60, CtrlStopSpeciesProtection90,
	} {
		f := newFakeOPC()
		f.Ctrl[2] = uint64(CtrlStart)
		res, err := New(f).SetCtrl(context.Background(), testUser, value, true, 2)
		if err != nil {
			t.Errorf("%s: %v", value, err)
			continue
		}
		if res[0].Outcome != OutcomeCommanded {
			t.Errorf("%s: result = %v, want commanded", value, res[0])
			continue
		}
		var written []uint32
		for _, w := range f.Writes {
			if w.ItemName == setCtrlItem(2) {
				written = w.Value
			}
		}
		if len(written) == 0 || written[0] != uint32(value) {
			t.Errorf("%s: SetCtrl written as %v", value, written)
		}
	}
}

// Tab. 80: SetIceDet switches the ice warning lamp — 0 off, 8 on — and the
// plant reports the SCADA control signal in its IceDet status.
func TestSpecIceDetLamp(t *testing.T) {
	f := newFakeOPC()
	f.IceDet[2] = 0
	f.IceDet[5] = IceDetExternalSCADA // already on

	res, err := New(f).IceDetOn(context.Background(), testUser, 2, 5)
	if err != nil {
		t.Fatalf("IceDetOn: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Errorf("plant 2: %v, want commanded", res[0])
	}
	if res[1].Outcome != OutcomeAlreadyInState {
		t.Errorf("plant 5: %v, want already in state", res[1])
	}
	if !f.Wrote(setIceDetItem(2)) {
		t.Errorf("SetIceDet not written for plant 2; writes: %v", f.WrittenNames())
	}
	if f.Wrote(setIceDetItem(5)) {
		t.Error("SetIceDet written for a plant whose lamp is already on")
	}
	if f.IceDet[2]&IceDetExternalSCADA == 0 {
		t.Error("the plant does not report the SCADA control signal after the command")
	}

	// A second command needs the previous session to have ended; on a real
	// plant that is the session end timeout.
	f.EndSessions()

	// And back off again.
	if _, err := New(f).IceDetOff(context.Background(), testUser, 2); err != nil {
		t.Fatalf("IceDetOff: %v", err)
	}
	if f.IceDet[2]&IceDetExternalSCADA != 0 {
		t.Error("the SCADA control signal is still set after switching the lamp off")
	}
}

func TestSpecIceDetValueValidation(t *testing.T) {
	f := newFakeOPC()
	for _, v := range []IceDetValue{1, 2, 4, 16, 9} {
		if v.Writable() {
			t.Errorf("%d is not a documented SetIceDet value", v)
		}
		if _, err := New(f).SetIceDet(context.Background(), testUser, v, 2); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("%d: err = %v, want it to wrap ErrInvalidValue", v, err)
		}
	}
	if len(f.WrittenNames()) != 0 {
		t.Errorf("a rejected value must not reach the plant; writes: %v", f.WrittenNames())
	}
}

// A stop for ice detection and the warning lamp belong together, and Enercon
// transmits them in one session.
func TestSpecCombinedCtrlRbhIceDetCommand(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Rbh[2] = RbhInstalled | RbhAutoDeicingAllowed
	f.IceDet[2] = 0

	res, err := New(f).ControlAndRbh(context.Background(), testUser, ControlAndRbhValue{
		SetCtrlValue:   true,
		CtrlValue:      CtrlStopIceDetection,
		SetRbhValue:    true,
		RbhValue:       RbhSetManualOn,
		SetIceDetValue: true,
		IceDetValue:    IceDetLampOn,
	}, 2)
	if err != nil {
		t.Fatalf("ControlAndRbh: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded", res[0])
	}
	for _, item := range []string{setCtrlItem(2), setRbhItem(2), setIceDetItem(2)} {
		if !f.Wrote(item) {
			t.Errorf("%s was not written; writes: %v", item, f.WrittenNames())
		}
	}
	// One session, so one SessionRequest and one SessionSubmit.
	if f.WriteCalls > 3 {
		t.Errorf("%d write requests, want the three values carried in one session", f.WriteCalls)
	}
}

// The Enercon server rejects a bare 8 for the heating; RbhOn writes 10.
func TestSpecRbhOnWritesTen(t *testing.T) {
	f := newFakeOPC()
	f.Rbh[2] = RbhInstalled | RbhAutoDeicingAllowed

	if _, err := New(f).RbhOn(context.Background(), testUser, 2); err != nil {
		t.Fatalf("RbhOn: %v", err)
	}
	for _, w := range f.Writes {
		if w.ItemName == setRbhItem(2) {
			if w.Value[0] != uint32(RbhSetManualOn) {
				t.Errorf("SetRbh written as %d, want %d", w.Value[0], RbhSetManualOn)
			}
			return
		}
	}
	t.Fatalf("SetRbh was not written; writes: %v", f.WrittenNames())
}

// RbhOn asks for a running heating, not for a particular bit: a plant already
// heating under automatic control needs no command.
func TestSpecRbhOnAcceptsAutomaticHeating(t *testing.T) {
	f := newFakeOPC()
	f.Rbh[2] = RbhInstalled | RbhAutoDeicingAllowed | RbhHeatingInOperationSCADA

	res, err := New(f).RbhOn(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("RbhOn: %v", err)
	}
	if res[0].Outcome != OutcomeAlreadyInState {
		t.Errorf("result = %v, want already in state", res[0])
	}
	if f.Wrote(setRbhItem(2)) {
		t.Error("a command was sent to a plant that is already heating")
	}
}

// A plant already stopped at 60° has to be brought to 90° when a full stop is
// requested. An unforced stop tolerates a state the caller did not ask for
// because Enercon may have stopped the plant itself — but 60° is not 90°, and
// reporting a 60° plant as already in the requested state is a wrong yes: the
// blades stay where they are and InRequestedState says the caller got what they
// asked for.
func TestFullStopDeepensAShallowStop(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[4] = uint64(CtrlStop60)

	res, err := New(f).Stop(context.Background(), testUser, true, false, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Errorf("outcome = %v (%v), want %v", res[0].Outcome, res[0].Err, OutcomeCommanded)
	}
	if !f.Wrote(setCtrlItem(4)) {
		t.Errorf("no SetCtrl was written, so the plant stays at 60°; writes: %v", f.WrittenNames())
	}
	if got := CtrlValue(f.Ctrl[4]); got != CtrlStop90 {
		t.Errorf("plant ends at %s, want %s", got, CtrlStop90)
	}
}

// The other direction stays tolerant: a plant at 90° is more stopped than a 60°
// stop asks for, so the request is already met. Commanding it would open the
// blades from 90° back to 60°, which is not what a caller asking for a stop
// wants.
func TestPartialStopAcceptsADeeperStop(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[4] = uint64(CtrlStop90)

	res, err := New(f).Stop(context.Background(), testUser, false, false, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeAlreadyInState {
		t.Errorf("outcome = %v (%v), want %v", res[0].Outcome, res[0].Err, OutcomeAlreadyInState)
	}
	if len(f.WrittenNames()) != 0 {
		t.Errorf("a plant at 90° must not be opened back to 60°; writes: %v", f.WrittenNames())
	}
}

// A plant Enercon stopped at 60° cannot be taken to 90° by a client at all, so
// the honest answer is that the command is not permitted — not that the plant is
// already where the caller wanted it.
func TestFullStopOnAnEnerconStopAt60IsNotPermitted(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[4] = uint64(CtrlStop60Enercon)

	res, err := New(f).Stop(context.Background(), testUser, true, false, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeNotPermitted {
		t.Errorf("outcome = %v, want %v", res[0].Outcome, OutcomeNotPermitted)
	}
	if !errors.Is(res[0].Err, ErrPlantUnderEnerconControl) {
		t.Errorf("err = %v, want it to wrap ErrPlantUnderEnerconControl", res[0].Err)
	}
	if res[0].InRequestedState() {
		t.Error("a plant stopped at 60° by Enercon is not in a requested 90° stop")
	}
	if len(f.WrittenNames()) != 0 {
		t.Errorf("must not open a session; writes: %v", f.WrittenNames())
	}
}

// A plant reporting a Ctrl value this package does not know is refused, and the
// refusal has to name the plant state. It used to wrap ErrSessionState, whose
// message reads "unexpected session state" — which sends an operator looking at
// session timeouts and reservation delays instead of at the plant. The state
// that surfaced this on a real park was 137, which is not in the data sheet this
// package was written against.
func TestAnUnknownPlantStateIsReportedAsSuch(t *testing.T) {
	for _, state := range []CtrlValue{CtrlValue(137), CtrlValue(9), CtrlValue(200)} {
		f := newFakeOPC()
		f.Ctrl[4] = uint64(state)

		res, err := New(f).Start(context.Background(), testUser, 4)
		if err != nil {
			t.Fatalf("state %d: Start: %v", uint64(state), err)
		}
		if res[0].Outcome != OutcomeNotPermitted {
			t.Errorf("state %d: outcome = %v, want %v", uint64(state), res[0].Outcome,
				OutcomeNotPermitted)
		}
		if !errors.Is(res[0].Err, ErrPlantStateUnknown) {
			t.Errorf("state %d: err = %v, want it to wrap ErrPlantStateUnknown",
				uint64(state), res[0].Err)
		}
		if errors.Is(res[0].Err, ErrSessionState) {
			t.Errorf("state %d: err = %v, but an unknown plant state is not a session problem",
				uint64(state), res[0].Err)
		}
		// The raw value has to be in the message: it is the only thing that can
		// be looked up in the data sheet for the controller type.
		if !strings.Contains(res[0].Err.Error(), fmt.Sprintf("%d", uint64(state))) {
			t.Errorf("state %d: err = %q does not name the value", uint64(state), res[0].Err)
		}
		if res[0].InRequestedState() {
			t.Errorf("state %d: an unknown state is never the requested one", uint64(state))
		}
		if len(f.WrittenNames()) != 0 {
			t.Errorf("state %d: must not open a session; writes: %v",
				uint64(state), f.WrittenNames())
		}
	}
}

// What a real park does, which the data sheet's wording did not suggest: after
// SetCtrl 7 the plant reports 7 — the command value itself — and stays there.
// It does not report the 60° blade angle that command produces.
//
// Two things followed from assuming otherwise, both seen on a live run: the
// species-protection stop was never recognised as having taken effect, and the
// plant could afterwards not be started, because a state the package believed
// impossible was treated as unknown.
func TestAPlantReportingTheCommandValueIsHandled(t *testing.T) {
	reported := []CtrlValue{
		CtrlGradientStop60, CtrlGradientStop90,
		CtrlStopIceDetection, CtrlStopShadowFlicker,
		CtrlStopSpeciesProtection60, CtrlStopSpeciesProtection90,
	}

	t.Run("it can be started again", func(t *testing.T) {
		for _, state := range reported {
			f := newFakeOPC()
			f.Ctrl[4] = uint64(state)

			res, err := New(f).Start(context.Background(), testUser, 4)
			if err != nil {
				t.Fatalf("%s: Start: %v", state, err)
			}
			if res[0].Outcome != OutcomeCommanded {
				t.Errorf("%s: outcome = %v (%v), want %v",
					state, res[0].Outcome, res[0].Err, OutcomeCommanded)
			}
			if !f.Wrote(setCtrlItem(4)) {
				t.Errorf("%s: no SetCtrl was written; writes: %v", state, f.WrittenNames())
			}
		}
	})

	t.Run("it counts as stopped", func(t *testing.T) {
		for _, state := range reported {
			if !state.Stopped() {
				t.Errorf("%s is a stop, so a plant reporting it is stopped", state)
			}
		}
	})

	t.Run("it satisfies a shallower stop request", func(t *testing.T) {
		// A plant standing at 90° because of a species-protection stop fulfils
		// an unforced request for a 60° stop: commanding it would open the
		// blades from 90° back to 60°.
		f := newFakeOPC()
		f.Ctrl[4] = uint64(CtrlStopSpeciesProtection90)

		res, err := New(f).Stop(context.Background(), testUser, false, false, 4)
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if res[0].Outcome != OutcomeAlreadyInState {
			t.Errorf("outcome = %v (%v), want %v", res[0].Outcome, res[0].Err,
				OutcomeAlreadyInState)
		}
		// A request for the deeper stop is sent.
		f = newFakeOPC()
		f.Ctrl[4] = uint64(CtrlStopSpeciesProtection60)
		res, err = New(f).Stop(context.Background(), testUser, true, false, 4)
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if res[0].Outcome != OutcomeCommanded {
			t.Errorf("full stop: outcome = %v (%v), want %v", res[0].Outcome, res[0].Err,
				OutcomeCommanded)
		}
	})

	t.Run("a forced command sees it as reached", func(t *testing.T) {
		for _, want := range reported {
			if !want.Reached(want) {
				t.Errorf("a plant reporting %s has not reached %s", want, want)
			}
		}
		// And nothing else does: Ctrl carries the value that was set, so a
		// plant reporting anything other than the command has not carried it
		// out, however its blades happen to stand.
		for _, other := range []CtrlValue{CtrlStop60, CtrlStop90, CtrlStart, CtrlStopEnercon} {
			if other.Reached(CtrlStopSpeciesProtection60) {
				t.Errorf("a plant reporting %s has not carried out a species-protection stop",
					other)
			}
		}
	})
}

// Ctrl carries the value that was set, never a blade angle. The package first
// assumed the opposite, then — on the first evidence of an echo — accepted both
// readings. Accepting both was still wrong, and not only redundant: a forced
// request for a command that shares a blade angle with the plant's current state
// was reported as already satisfied and never sent.
//
// A plant standing at Stop90 has not carried out a gradient stop at 90°. The
// blades are at the same angle, the command is a different one, and a forced
// request asks for that command.
func TestACommandIsNotReachedByAnotherWithTheSameAngle(t *testing.T) {
	pairs := []struct{ current, want CtrlValue }{
		{CtrlStop90, CtrlGradientStop90},
		{CtrlStop90, CtrlStopSpeciesProtection90},
		{CtrlStop60, CtrlGradientStop60},
		{CtrlStop60, CtrlStopSpeciesProtection60},
		{CtrlGradientStop60, CtrlStopSpeciesProtection60},
	}
	for _, p := range pairs {
		if p.current.Reached(p.want) {
			t.Errorf("a plant reporting %s has not carried out %s", p.current, p.want)
		}
		if ctrlSatisfied(p.current, p.want, true) {
			t.Errorf("a forced %s on a plant reporting %s must be sent", p.want, p.current)
		}
	}

	// End to end: the command has to reach the plant.
	f := newFakeOPC()
	f.Ctrl[4] = uint64(CtrlStop90)
	res, err := New(f).SetCtrl(context.Background(), testUser, CtrlGradientStop90, true, 4)
	if err != nil {
		t.Fatalf("SetCtrl: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Errorf("outcome = %v (%v), want %v", res[0].Outcome, res[0].Err, OutcomeCommanded)
	}
	if !f.Wrote(setCtrlItem(4)) {
		t.Errorf("SetCtrl was never written; writes: %v", f.WrittenNames())
	}

	// Unforced it is sent too: the same blade angle by a different command is a
	// different operating mode, and the plant can be commanded. Only a *deeper*
	// stop satisfies a request, because commanding that would open the blades.
	f = newFakeOPC()
	f.Ctrl[4] = uint64(CtrlStop90)
	res, err = New(f).SetCtrl(context.Background(), testUser, CtrlGradientStop90, false, 4)
	if err != nil {
		t.Fatalf("SetCtrl: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Errorf("unforced: outcome = %v, want %v", res[0].Outcome, OutcomeCommanded)
	}

	f = newFakeOPC()
	f.Ctrl[4] = uint64(CtrlStop90)
	res, err = New(f).SetCtrl(context.Background(), testUser, CtrlGradientStop60, false, 4)
	if err != nil {
		t.Fatalf("SetCtrl: %v", err)
	}
	if res[0].Outcome != OutcomeAlreadyInState {
		t.Errorf("deeper stop: outcome = %v, want %v", res[0].Outcome, OutcomeAlreadyInState)
	}
}

// A plant stopped for species protection at 60° is not "already in" a plain 60°
// stop. The blades stand at the same angle, but it is a different command and a
// different operating mode, the plant can be commanded, and the operator asked
// for the plain stop. Reporting it as already in state left the plant under
// species protection and wrote nothing.
//
// The tolerance an unforced stop exists for is narrower than "same blade angle":
// do not open the blades of a plant that is more stopped than asked for, and do
// not fight a stop Enercon made with higher rights.
func TestADifferentStopAtTheSameAngleIsNotAlreadyInState(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[4] = uint64(CtrlStopSpeciesProtection60)

	res, err := New(f).Stop(context.Background(), testUser, false, false, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Errorf("outcome = %v (%v), want %v", res[0].Outcome, res[0].Err, OutcomeCommanded)
	}
	if !f.Wrote(setCtrlItem(4)) {
		t.Errorf("no SetCtrl was written, so the plant stays under species protection; writes: %v",
			f.WrittenNames())
	}
	if got := CtrlValue(f.Ctrl[4]); got != CtrlStop60 {
		t.Errorf("plant ends at %s, want %s", got, CtrlStop60)
	}
}

// Whatever ctrlSatisfied calls satisfied has to be a state from which sending
// the command would be wrong or impossible — not merely one that looks close
// enough. Otherwise the library reports a setpoint as reached while the plant
// sits in a state nobody asked for, which is what a live run hit: "already in
// state" for a plant under species protection, followed by a minute of waiting
// for a stop that was never sent.
func TestWhatSatisfiesARequestIsWrongOrImpossibleToCommand(t *testing.T) {
	reported := []CtrlValue{
		CtrlStart, CtrlStop60, CtrlStop90,
		CtrlGradientStop60, CtrlGradientStop90,
		CtrlStopIceDetection, CtrlStopShadowFlicker,
		CtrlStopSpeciesProtection60, CtrlStopSpeciesProtection90,
		CtrlStop60Enercon, CtrlStopEnercon, CtrlValueRejected, CtrlCommError,
	}
	for _, want := range reported {
		if !want.Writable() {
			continue // not a command a client may send
		}
		for _, current := range reported {
			if !ctrlSatisfied(current, want, false) {
				continue
			}
			switch {
			case current.Reached(want):
				// The command was carried out.
			case !current.Commandable():
				// Enercon holds it; a client cannot command it away.
				if current.stopDepth() < want.stopDepth() {
					t.Errorf("%s satisfies %s although it is a shallower stop",
						current, want)
				}
			case current.stopDepth() > want.stopDepth():
				// Deeper: commanding would open the blades back up.
			default:
				t.Errorf("%s satisfies a request for %s, but commanding it would be "+
					"neither wrong nor impossible — so the command has to be sent",
					current, want)
			}
		}
	}
}
