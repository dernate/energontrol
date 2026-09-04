package energontrol

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The tests in this file reconcile the implementation with the ENERCON SCADA
// PDI-OPC technical data sheet, sections 3.4 and 4.1.

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

// Tab. 80: a plant reports the resulting blade angle, never the command value.
// A forced request for a gradient stop at 60° is therefore satisfied by Ctrl 1.
func TestSpecExpectedCtrlAfterCommand(t *testing.T) {
	cases := []struct {
		cmd      CtrlValue
		expected CtrlValue
		known    bool
	}{
		{CtrlStart, CtrlStart, true},
		{CtrlStop60, CtrlStop60, true},
		{CtrlGradientStop60, CtrlStop60, true},
		{CtrlStopSpeciesProtection60, CtrlStop60, true},
		{CtrlStop90, CtrlStop90, true},
		{CtrlGradientStop90, CtrlStop90, true},
		{CtrlStopSpeciesProtection90, CtrlStop90, true},
		// The data sheet names no blade angle for these two.
		{CtrlStopIceDetection, 0, false},
		{CtrlStopShadowFlicker, 0, false},
	}
	for _, tc := range cases {
		got, ok := tc.cmd.expectedCtrlAfter()
		if ok != tc.known || (ok && got != tc.expected) {
			t.Errorf("%s: expectedCtrlAfter = (%s, %t), want (%s, %t)",
				tc.cmd, got, ok, tc.expected, tc.known)
		}
		if tc.known && !ctrlSatisfied(tc.expected, tc.cmd, true) {
			t.Errorf("%s: a plant reporting %s should satisfy a forced request", tc.cmd, tc.expected)
		}
		if !tc.known && ctrlSatisfied(CtrlStop60, tc.cmd, true) {
			t.Errorf("%s: no state may be claimed to satisfy a forced request", tc.cmd)
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
	f.ReadBack[setCtrlItem(2)] = []uint64{uint64(CtrlStart), 0, 0} // server holds something else

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

// Abb. 34: the item names of the Ctrl node, verbatim.
func TestSpecCtrlNodeItemNames(t *testing.T) {
	want := []string{
		"Loc/Wec/Plant1/Ctrl/SessionState",
		"Loc/Wec/Plant1/Ctrl/SessionTimeOut",
		"Loc/Wec/Plant1/Ctrl/SessionPubKey",
		"Loc/Wec/Plant1/Ctrl/SessionRequest",
		"Loc/Wec/Plant1/Ctrl/SessionSubmit",
		"Loc/Wec/Plant1/Ctrl/SetCtrl",
		"Loc/Wec/Plant1/Ctrl/Ctrl",
		"Loc/Wec/Plant1/Ctrl/SetRbh",
		"Loc/Wec/Plant1/Ctrl/Rbh",
	}
	got := []string{
		sessionStateItem(1, SessionCtrl),
		sessionTimeoutItem(1, SessionCtrl),
		sessionPubKeyItem(1, SessionCtrl),
		sessionRequestItem(1, SessionCtrl),
		sessionSubmitItem(1, SessionCtrl),
		setCtrlItem(1),
		ctrlItem(1),
		setRbhItem(1),
		rbhItem(1),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item name = %q, want %q", got[i], want[i])
		}
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
