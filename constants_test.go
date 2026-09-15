package energontrol

// The typed value sets and status words, reconciled against the ENERCON SCADA
// PDI-OPC technical data sheet.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

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

func TestCtrlValueClassification(t *testing.T) {
	if !CtrlStop60Enercon.Stopped() || !CtrlStopEnercon.Stopped() {
		t.Error("an Enercon stop is a stop state")
	}
	if CtrlCommError.Stopped() {
		t.Error("a communication error must not count as stopped")
	}
	for _, v := range []CtrlValue{CtrlStart, CtrlStop60, CtrlStop90} {
		if !v.Writable() {
			t.Errorf("%s should be writable", v)
		}
		if v.stateError() != nil {
			t.Errorf("%s should be commandable, got %v", v, v.stateError())
		}
	}
	for _, v := range []CtrlValue{CtrlStop60Enercon, CtrlStopEnercon, CtrlCommError, CtrlValue(77)} {
		if v.Writable() {
			t.Errorf("%s must not be writable by a client", v)
		}
		if v.stateError() == nil {
			t.Errorf("%s should not be commandable", v)
		}
	}
	if !errors.Is(CtrlStopEnercon.stateError(), ErrPlantUnderEnerconControl) {
		t.Error("StopEnercon should map to ErrPlantUnderEnerconControl")
	}
	if !errors.Is(CtrlCommError.stateError(), ErrPlantCommunication) {
		t.Error("CommError should map to ErrPlantCommunication")
	}
}

func TestRbhStatusStrings(t *testing.T) {
	for _, status := range []uint64{0, RbhNotInstalledValue, 1 << 16} {
		if got := RbhStatusStrings(status); len(got) != 1 {
			t.Errorf("status %d = %v, want the single \"not installed\" entry", status, got)
		}
	}
	got := RbhStatusStrings(RbhAutoOffWEA | RbhManualOnSCADA | RbhInstalled)
	want := []string{
		"Automatic operation of the heating suppressed",
		"Heating manually on (SCADA)",
		"Heating installed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RbhStatusStrings = %v, want %v (ascending bit order)", got, want)
	}
}

func TestSessionStateString(t *testing.T) {
	cases := map[SessionState]string{
		SessionFree:               `"session free"`,
		SessionInsufficientRights: `"insufficient rights"`,
		SessionState(234):         "unknown state 234",
	}
	for state, want := range cases {
		if got := state.String(); got != want {
			t.Errorf("SessionState(%d).String() = %q, want %q", state, got, want)
		}
	}
}

// Tab. 80: SetCtrl accepts the control values 0 to 8. v1 and the first v2 draft
// allowed only 0, 1 and 2, so gradient stops and the stops for ice detection,
// shadow flicker and species protection could not be sent at all.
func TestSpecWritableCtrlValues(t *testing.T) {
	writable := []CtrlValue{
		CtrlStart, CtrlStop60, CtrlStop90,
		CtrlGradientStop60, CtrlGradientStop90,
		CtrlStopIceDetection, CtrlStopShadowFlicker,
		CtrlStopSpeciesProtection60, CtrlStopSpeciesProtection90,
	}
	for _, v := range writable {
		if !v.Writable() {
			t.Errorf("%s (%d) is a documented SetCtrl value but is not writable", v, v)
		}
	}
	// Ctrl reports these; a client must not write them.
	for _, v := range []CtrlValue{CtrlValueRejected, CtrlStop60Enercon, CtrlStopEnercon, CtrlCommError} {
		if v.Writable() {
			t.Errorf("%s (%d) is a status, not a command, and must not be writable", v, v)
		}
	}
}

// Ctrl carries the value that was set — a plant reports the command it was
// given and holds it, never a blade angle. The blade angle each command produces
// is still what ranks a stop's depth, which is a different question and stays in
// stopDepth.
func TestSpecStateAfterCommand(t *testing.T) {
	cases := []struct {
		cmd   CtrlValue
		depth int
	}{
		{CtrlStart, 0},
		{CtrlStop60, 1},
		{CtrlGradientStop60, 1},
		{CtrlStopSpeciesProtection60, 1},
		{CtrlStop90, 2},
		{CtrlGradientStop90, 2},
		{CtrlStopSpeciesProtection90, 2},
		// These two name no blade angle, so they rank nowhere and nothing counts
		// as already satisfying them.
		{CtrlStopIceDetection, -1},
		{CtrlStopShadowFlicker, -1},
	}
	for _, tc := range cases {
		if got := tc.cmd.stopDepth(); got != tc.depth {
			t.Errorf("%s: stopDepth = %d, want %d", tc.cmd, got, tc.depth)
		}
		// The command value reported back is what counts as carried out.
		if !tc.cmd.Reached(tc.cmd) {
			t.Errorf("%s: a plant reporting the command value has carried it out", tc.cmd)
		}
		if !ctrlSatisfied(tc.cmd, tc.cmd, true) {
			t.Errorf("%s: the reported command value should satisfy a forced request", tc.cmd)
		}
		// Nothing else does, not even a command that ends at the same angle.
		for _, other := range cases {
			if other.cmd == tc.cmd {
				continue
			}
			if other.cmd.Reached(tc.cmd) {
				t.Errorf("a plant reporting %s has not carried out %s", other.cmd, tc.cmd)
			}
		}
		if tc.cmd != CtrlStart && CtrlStart.Reached(tc.cmd) {
			t.Errorf("%s: a running plant has not carried out a stop", tc.cmd)
		}
	}
}

// 3.4.3: Ctrl also reports 121 for a rejected control value.
func TestSpecCtrlValueRejected(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlValueRejected)

	res, err := New(f).Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res[0].Outcome != OutcomeNotPermitted || !errors.Is(res[0].Err, ErrCtrlValueRejected) {
		t.Fatalf("result = %v, want not-permitted with ErrCtrlValueRejected", res[0])
	}
}

// Tab. 80: SetRbH accepts 0, 2, 8, 10 and 128. v1 knew 128 but not 8; the first
// v2 draft dropped 128.
func TestSpecWritableRbhValues(t *testing.T) {
	for _, v := range []RbhValue{RbhSetStandard, RbhSetAutoOff, RbhSetManualOn, RbhSetPresetDuration} {
		if !v.Writable() {
			t.Errorf("%s (%d) is a documented SetRbH value but is not writable", v, v)
		}
	}
	// 8 is listed in the data sheet but rejected by the server: the heating is
	// switched on with 10, that is 8+2.
	for _, v := range []RbhValue{1, 3, 7, 8, 9, 11, 127, 129} {
		if v.Writable() {
			t.Errorf("%d must not be writable", v)
		}
	}
}

// Tab. 80: bit 15 marks an installed heating, and a whole-word value marks a
// plant without one. The data sheet prints 65636; 65536 follows from the 16-bit
// status word. Both have bit 15 clear, and so does a status of 0.
func TestSpecHeatingInstalledDetection(t *testing.T) {
	notInstalled := []uint64{0, RbhNotInstalledValue, 1 << 16}
	for _, status := range notInstalled {
		if RbhIsInstalled(status) {
			t.Errorf("status %d should read as no heating installed", status)
		}
		if !errors.Is(rbhStateError(status), ErrRbhUnavailable) {
			t.Errorf("status %d should be reported as unavailable", status)
		}
	}
	if !RbhIsInstalled(RbhInstalled | RbhAutoDeicingAllowed) {
		t.Error("bit 15 set should read as installed")
	}
}

// Tab. 79: the session states, verbatim.
func TestSpecSessionStateValues(t *testing.T) {
	want := map[SessionState]uint16{
		SessionFree: 0, SessionReserved: 1, SessionParameterInput: 2,
		SessionWaitLoop: 3, SessionWaitEnd: 4, SessionBlocked: 5,
		SessionOccupied: 108, SessionAccessDenied: 109, SessionValueError: 121,
		SessionIncorrectUserID: 174, SessionInsufficientRights: 175,
	}
	for state, value := range want {
		if uint16(state) != value {
			t.Errorf("session state %v = %d, want %d", state, uint16(state), value)
		}
	}
}

// Tab. 80: the IceDet status says which system detected ice.
func TestSpecIceDetStatusStrings(t *testing.T) {
	if got := IceDetStatusStrings(0); len(got) != 1 {
		t.Errorf("no ice = %v, want a single entry", got)
	}
	got := IceDetStatusStrings(IceDetPowerCurve | IceDetExternalSCADA | IceDetParkDetection)
	want := []string{
		"Detection by the power curve method",
		"External control signal from SCADA",
		"Park-wide ice detection",
	}
	if len(got) != len(want) {
		t.Fatalf("IceDetStatusStrings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IceDetStatusStrings = %v, want %v (ascending bit order)", got, want)
		}
	}
}

// The stop ranking is what decides whether an unforced stop request is already
// met, so the mapping itself is pinned: the two states Enercon reserves rank
// with the angle they name, and a state that says nothing about the blades ranks
// below running so that it can satisfy nothing.
func TestStopDepthRanksByBladeAngle(t *testing.T) {
	cases := map[CtrlValue]int{
		CtrlStart:         0,
		CtrlStop60:        1,
		CtrlStop60Enercon: 1,
		CtrlStop90:        2,
		CtrlStopEnercon:   2,
		CtrlCommError:     -1,
		CtrlValueRejected: -1,
		// A real park reports the command value itself, so those rank by the
		// blade angle their name carries.
		CtrlGradientStop60:          1,
		CtrlGradientStop90:          2,
		CtrlStopSpeciesProtection60: 1,
		CtrlStopSpeciesProtection90: 2,
		// The two stops whose name carries no angle: stopped, but not at a known
		// angle, so they satisfy no request for a particular one.
		CtrlStopIceDetection:  -1,
		CtrlStopShadowFlicker: -1,
		CtrlValue(42):         -1,
	}
	for v, want := range cases {
		if got := v.stopDepth(); got != want {
			t.Errorf("%s.stopDepth() = %d, want %d", v, got, want)
		}
	}
	if CtrlCommError.stopDepth() >= CtrlStart.stopDepth() {
		t.Error("an unknown state must rank below running, or it could satisfy a request")
	}
}
