package energontrol

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestToUint64(t *testing.T) {
	ok := []struct {
		in   any
		want uint64
	}{
		{uint64(130), 130}, {uint32(2), 2}, {uint16(175), 175}, {uint8(1), 1}, {uint(5), 5},
		{int(0), 0}, {int64(255), 255}, {int32(4), 4}, {int16(2), 2}, {int8(1), 1},
		{float64(129), 129}, {float32(2), 2},
	}
	for _, tc := range ok {
		got, err := toUint64(tc.in)
		if err != nil {
			t.Errorf("toUint64(%T %v): %v", tc.in, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("toUint64(%T %v) = %d, want %d", tc.in, tc.in, got, tc.want)
		}
	}
	bad := []any{nil, "1", int(-1), int64(-5), float64(1.5), float64(-1), []uint64{1}, struct{}{}}
	for _, in := range bad {
		if got, err := toUint64(in); err == nil {
			t.Errorf("toUint64(%T %v) = %d, want an error", in, in, got)
		}
	}
}

func TestQualityUsable(t *testing.T) {
	usable := []string{"", "good", "goodLocalOverride"}
	for _, q := range usable {
		if !qualityUsable(q) {
			t.Errorf("qualityUsable(%q) = false, want true", q)
		}
	}
	unusable := []string{"bad", "badConfigurationError", "uncertain", "uncertainLastUsableValue"}
	for _, q := range unusable {
		if qualityUsable(q) {
			t.Errorf("qualityUsable(%q) = true, want false", q)
		}
	}
}

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
		// An unforced stop accepts any stop state, including Enercon's.
		{CtrlStop60, CtrlStop90, false, true},
		{CtrlStop90, CtrlStop60, false, true},
		{CtrlStop60Enercon, CtrlStop90, false, true},
		{CtrlStopEnercon, CtrlStop60, false, true},
		{CtrlStart, CtrlStop90, false, false},
		// A communication error is never a stop state: the plant is not known
		// to be stopped.
		{CtrlCommError, CtrlStop90, false, false},
		{CtrlCommError, CtrlStop60, false, false},
		// A forced stop wants exactly that state.
		{CtrlStop60, CtrlStop60, true, true},
		{CtrlStop60, CtrlStop90, true, false},
		{CtrlStopEnercon, CtrlStop90, true, false},
	}
	for _, tc := range cases {
		if got := ctrlSatisfied(tc.current, tc.want, tc.force); got != tc.ok {
			t.Errorf("ctrlSatisfied(%s, %s, force=%t) = %t, want %t",
				tc.current, tc.want, tc.force, got, tc.ok)
		}
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

func TestSessionStateErrorClassification(t *testing.T) {
	cases := []struct {
		got  SessionState
		want error
	}{
		{SessionOccupied, ErrSessionOccupied},
		{SessionBlocked, ErrSessionBlocked},
		{SessionAccessDenied, ErrAccessDenied},
		{SessionInsufficientRights, ErrInsufficientRights},
		{SessionIncorrectUserID, ErrIncorrectUserID},
		{SessionValueError, ErrSessionValue},
		{SessionReserved, ErrSessionState},
		{SessionState(234), ErrSessionState},
	}
	for _, tc := range cases {
		err := &SessionStateError{PlantNo: 3, Want: SessionFree, Got: tc.got}
		if !errors.Is(err, tc.want) {
			t.Errorf("state %d: err = %v, want it to wrap %v", tc.got, err, tc.want)
		}
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

func TestSessionCredentialsAreDistinct(t *testing.T) {
	seen := make(map[uint16]int)
	for i := 0; i < 200; i++ {
		cred, err := newSessionCred()
		if err != nil {
			t.Fatalf("newSessionCred: %v", err)
		}
		if cred.privateKey == 0 {
			t.Fatal("private key must not be zero")
		}
		if cred.sessionID >= 20 {
			t.Fatalf("session id %d out of range", cred.sessionID)
		}
		seen[cred.privateKey]++
	}
	// v1 reseeded math/rand from the clock on every call, which produced runs of
	// identical keys. Anything close to that shows up as a dominant value here.
	for key, n := range seen {
		if n > 10 {
			t.Errorf("private key %d drawn %d times out of 200", key, n)
		}
	}
}

func TestItemErrorMessage(t *testing.T) {
	err := &ItemError{ItemName: ctrlItem(4), Reason: ErrBadQuality, Detail: "quality=bad"}
	want := fmt.Sprintf("energontrol: item %q: item quality is not good (quality=bad)", ctrlItem(4))
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrBadQuality) {
		t.Error("ItemError should unwrap to its reason")
	}
}

func TestItemNamesMatchTheSCADAAddressSpace(t *testing.T) {
	cases := map[string]string{
		ctrlItem(7):                         "Loc/Wec/Plant7/Ctrl/Ctrl",
		rbhItem(7):                          "Loc/Wec/Plant7/Ctrl/Rbh",
		setCtrlItem(7):                      "Loc/Wec/Plant7/Ctrl/SetCtrl",
		setRbhItem(7):                       "Loc/Wec/Plant7/Ctrl/SetRbh",
		setResetItem(7):                     "Loc/Wec/Plant7/Reset/SetReset",
		sessionStateItem(7, SessionCtrl):    "Loc/Wec/Plant7/Ctrl/SessionState",
		sessionRequestItem(7, SessionReset): "Loc/Wec/Plant7/Reset/SessionRequest",
		sessionPubKeyItem(7, SessionCtrl):   "Loc/Wec/Plant7/Ctrl/SessionPubKey",
		sessionSubmitItem(7, SessionReset):  "Loc/Wec/Plant7/Reset/SessionSubmit",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("item name = %q, want %q", got, want)
		}
	}
}

// The OPC XML-DA option names are XML attribute names and therefore case
// sensitive. v1 sent "returnItemName", which a server ignores.
func TestRequestOptionsUseSpecCasing(t *testing.T) {
	for _, opts := range []map[string]interface{}{readOptions(), writeOptions()} {
		for key := range opts {
			if key[0] < 'A' || key[0] > 'Z' {
				t.Errorf("option %q does not use the casing from the specification", key)
			}
		}
	}
	if _, ok := readOptions()[optReturnItemName]; !ok {
		t.Error("reads must request ItemName so responses can be correlated")
	}
}
