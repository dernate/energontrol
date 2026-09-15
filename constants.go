package energontrol

import (
	"fmt"
	"sort"
	"strings"
)

// Availability of the individual control values depends on the plant's
// controller type; the data sheet marks the affected values with footnotes,
// reproduced on the constants below. This package does not check controller
// types — a plant that does not support a value answers with CtrlValueRejected,
// which surfaces as ErrCtrlValueRejected. Knowing which plants support which
// command is the caller's responsibility.

// CtrlValue is the value of Loc/Wec/Plant<n>/Ctrl/Ctrl.
//
// It is a distinct type on purpose: in v1 these values lived in a
// map[string]uint64, where a typo produced the zero value — and the zero value
// is CtrlStart. Looking up a misspelled "Stop" therefore started a turbine.
// With a named type, the same mistake is a compile error.
type CtrlValue uint64

const (
	// CtrlStart runs the plant. Written to SetCtrl and read back from Ctrl.
	CtrlStart CtrlValue = 0
	// CtrlStop60 stops the plant with a 60° blade angle.
	//
	// Data sheet footnote 1: not available for plants with controller type
	// EP5-CS-03.
	CtrlStop60 CtrlValue = 1
	// CtrlStop90 stops the plant with a 90° blade angle (full stop).
	CtrlStop90 CtrlValue = 2
	// CtrlGradientStop60 is a gradient stop with a 60° blade angle.
	//
	// Data sheet footnote 1: not available for plants with controller type
	// EP5-CS-03.
	CtrlGradientStop60 CtrlValue = 3
	// CtrlGradientStop90 is a gradient stop with a 90° blade angle.
	CtrlGradientStop90 CtrlValue = 4
	// CtrlStopIceDetection stops the plant for ice detection.
	CtrlStopIceDetection CtrlValue = 5
	// CtrlStopShadowFlicker stops the plant for shadow flicker mitigation.
	CtrlStopShadowFlicker CtrlValue = 6
	// CtrlStopSpeciesProtection60 stops the plant for species protection with a
	// 60° blade angle.
	//
	// Data sheet footnote 2: not available for plants with controller type
	// EP5-CS-03, except for the E-160 EP5 E3.
	CtrlStopSpeciesProtection60 CtrlValue = 7
	// CtrlStopSpeciesProtection90 stops the plant for species protection with a
	// 90° blade angle.
	CtrlStopSpeciesProtection90 CtrlValue = 8

	// The values below are reported by Ctrl and must not be written. The values
	// above are both written and reported: Ctrl carries the value that was set,
	// so after SetCtrl 7 the plant reads 7 and holds it.

	// CtrlValueRejected means the plant rejected the written control value.
	// Enercon lists it among the feedback codes of Ctrl.
	CtrlValueRejected CtrlValue = 121
	// CtrlStop60Enercon means the plant was stopped at 60° with higher
	// priority. It cannot be started by a client.
	//
	// Data sheet footnote 2: not available for plants with controller type
	// EP5-CS-03, except for the E-160 EP5 E3.
	CtrlStop60Enercon CtrlValue = 129
	// CtrlStopEnercon means the plant was stopped at 90° with higher priority.
	// It cannot be started by a client.
	CtrlStopEnercon CtrlValue = 130
	// CtrlCommError means the value has not been initialised: the SCADA cannot
	// reach the plant. The real state of the plant is unknown.
	CtrlCommError CtrlValue = 255
)

var ctrlNames = map[CtrlValue]string{
	CtrlStart:                   "Start",
	CtrlStop60:                  "Stop60",
	CtrlStop90:                  "Stop90",
	CtrlGradientStop60:          "GradientStop60",
	CtrlGradientStop90:          "GradientStop90",
	CtrlStopIceDetection:        "StopIceDetection",
	CtrlStopShadowFlicker:       "StopShadowFlicker",
	CtrlStopSpeciesProtection60: "StopSpeciesProtection60",
	CtrlStopSpeciesProtection90: "StopSpeciesProtection90",
	CtrlValueRejected:           "ValueRejected",
	CtrlStop60Enercon:           "Stop60Enercon",
	CtrlStopEnercon:             "StopEnercon",
	CtrlCommError:               "CommunicationError",
}

func (v CtrlValue) String() string {
	if s, ok := ctrlNames[v]; ok {
		return s
	}
	return fmt.Sprintf("CtrlValue(%d)", uint64(v))
}

// Writable reports whether a client may write this value to SetCtrl. Enercon
// defines the values 0 to 8; everything above is a status a plant reports, not
// a command a client may send.
func (v CtrlValue) Writable() bool {
	return v <= CtrlStopSpeciesProtection90
}

// Stopped reports whether the plant reports a stopped state, including the
// states reserved for Enercon and the command values a plant echoes back.
//
// It is false for CtrlCommError: an uninitialised value means the SCADA cannot
// reach the plant, so the plant is not known to be stopped. It is false for
// CtrlValueRejected for the same reason, and for any value this package does not
// know.
func (v CtrlValue) Stopped() bool {
	switch v {
	case CtrlStop60, CtrlStop90,
		CtrlGradientStop60, CtrlGradientStop90,
		CtrlStopIceDetection, CtrlStopShadowFlicker,
		CtrlStopSpeciesProtection60, CtrlStopSpeciesProtection90,
		CtrlStop60Enercon, CtrlStopEnercon:
		return true
	default:
		return false
	}
}

// stopDepth ranks how far a state has the plant shut down — running, stopped at
// 60°, stopped at 90° — so that "at least as stopped as requested" is a
// comparison rather than a set membership test.
//
// The two states Enercon reserves for itself rank with the angle they name:
// CtrlStop60Enercon is a 60° stop, CtrlStopEnercon a 90° one. That is what lets
// an unforced stop tolerate them without also letting a 60° stop pass for a 90°
// one.
//
// A state that carries no rank ranks below running, so it satisfies no stop
// request at all: CtrlValueRejected and CtrlCommError say nothing about where the
// blades are, and neither do the two stops whose name does not name an angle —
// a plant stopped for ice detection is stopped, but not at a known angle.
//
// The same ranking is applied to a requested command value, which works because
// a command and the state it produces carry the same angle.
func (v CtrlValue) stopDepth() int {
	switch v {
	case CtrlStart:
		return 0
	case CtrlStop60, CtrlGradientStop60, CtrlStopSpeciesProtection60, CtrlStop60Enercon:
		return 1
	case CtrlStop90, CtrlGradientStop90, CtrlStopSpeciesProtection90, CtrlStopEnercon:
		return 2
	default:
		return -1
	}
}

// Commandable reports whether a client can still change this state.
//
// The data sheet says "only operating states with the values 0-2 can be
// changed", which reads as though a client-initiated gradient or
// species-protection stop could never be undone. A real park shows otherwise: it
// reports the command value itself, and a plant reporting 7 can be started
// again. The sentence describes the three states that table lists, not a
// lock-out, so every state a client can have caused is commandable.
//
// The states reserved for Enercon are not, and neither is a rejected value, a
// communication error, or a value this package does not know: for those the
// command is refused before a session is opened.
func (v CtrlValue) Commandable() bool {
	switch v {
	case CtrlStart, CtrlStop60, CtrlStop90,
		CtrlGradientStop60, CtrlGradientStop90,
		CtrlStopIceDetection, CtrlStopShadowFlicker,
		CtrlStopSpeciesProtection60, CtrlStopSpeciesProtection90:
		return true
	default:
		return false
	}
}

// Reached reports whether a plant reporting state v has carried out the command
// value want.
//
// Ctrl carries the value that was set. A plant stopped for species protection at
// 60° reports 7, not the 60° blade angle that stop produces, and it holds 7 — no
// blade angle has ever been observed in that item. So this is exact equality,
// and it is a named method rather than a bare comparison because the rule is not
// obvious from the data sheet, which describes Ctrl as an operating state, and
// because a monitoring loop needs to ask exactly this.
//
// It is deliberately not satisfied by a different command that leaves the blades
// at the same angle: a plant reporting CtrlStop90 has not carried out a gradient
// stop at 90°. Treating the two as interchangeable made a forced request for the
// gradient stop report success without sending anything.
//
// For the looser question "is this plant stopped at all", use Stopped; for "is
// it at least as stopped as I asked", an unforced Stop already answers it.
func (v CtrlValue) Reached(want CtrlValue) bool {
	return v == want
}

// stateError returns the error that describes why a plant in this state cannot
// be commanded, or nil if it can.
func (v CtrlValue) stateError() error {
	switch v {
	case CtrlStop60Enercon, CtrlStopEnercon:
		return fmt.Errorf("%w (state %s)", ErrPlantUnderEnerconControl, v)
	case CtrlCommError:
		return fmt.Errorf("%w (state %s)", ErrPlantCommunication, v)
	case CtrlValueRejected:
		return fmt.Errorf("%w (state %s)", ErrCtrlValueRejected, v)
	default:
		if !v.Commandable() {
			return fmt.Errorf("%w (state %s)", ErrPlantStateUnknown, v)
		}
		return nil
	}
}

// RbhValue is the value written to Loc/Wec/Plant<n>/Ctrl/SetRbh.
type RbhValue uint64

const (
	// RbhSetStandard neither suppresses automatic operation nor switches the
	// heating on manually.
	RbhSetStandard RbhValue = 0
	// RbhSetAutoOff suppresses automatic operation of the heating.
	RbhSetAutoOff RbhValue = 2
	// RbhSetManualOn suppresses automatic operation and switches the heating on
	// manually. This is the value to use for "switch the heating on".
	//
	// The data sheet also lists 8 ("switch heating on manually", without
	// suppressing automatic operation), but the server rejects a bare 8 — the
	// heating is switched on with 10, that is 8+2. There is therefore no
	// constant for 8.
	RbhSetManualOn RbhValue = 10
	// RbhSetPresetDuration switches the heating on for the preset heating
	// duration.
	//
	// Enercon: only possible if the plant has stopped and ice has been detected.
	// This is a one-shot action with no status bit of its own, so it is always
	// sent — it can never be "already in state".
	RbhSetPresetDuration RbhValue = 128
)

var rbhNames = map[RbhValue]string{
	RbhSetStandard:       "Standard",
	RbhSetAutoOff:        "AutoOff",
	RbhSetManualOn:       "ManualOn",
	RbhSetPresetDuration: "PresetDuration",
}

func (v RbhValue) String() string {
	if s, ok := rbhNames[v]; ok {
		return s
	}
	return fmt.Sprintf("RbhValue(%d)", uint64(v))
}

// Writable reports whether a client may write this value to SetRbh.
func (v RbhValue) Writable() bool {
	switch v {
	case RbhSetStandard, RbhSetAutoOff, RbhSetManualOn, RbhSetPresetDuration:
		return true
	default:
		return false
	}
}

// IceDetValue is the value written to Loc/Wec/Plant<n>/Ctrl/SetIceDet. It
// switches the ice warning lamp.
type IceDetValue uint64

const (
	// IceDetLampOff switches the ice warning lamp off.
	IceDetLampOff IceDetValue = 0
	// IceDetLampOn switches the ice warning lamp on. The plant then reports
	// IceDetExternalSCADA in its IceDet status.
	IceDetLampOn IceDetValue = 8
)

func (v IceDetValue) String() string {
	switch v {
	case IceDetLampOff:
		return "LampOff"
	case IceDetLampOn:
		return "LampOn"
	default:
		return fmt.Sprintf("IceDetValue(%d)", uint64(v))
	}
}

// Writable reports whether a client may write this value to SetIceDet.
func (v IceDetValue) Writable() bool {
	return v == IceDetLampOff || v == IceDetLampOn
}

// Ice detection status bits, as read from Loc/Wec/Plant<n>/Ctrl/IceDet. The
// status says which system detected the ice.
const (
	IceDetPowerCurve     = uint64(1)  // detection by the power curve method
	IceDetPreventive     = uint64(2)  // preventive detection
	IceDetExternalSensor = uint64(4)  // external ice detection sensor (Labko)
	IceDetExternalSCADA  = uint64(8)  // external control signal from SCADA (third party)
	IceDetParkDetection  = uint64(16) // park-wide ice detection
)

var iceDetStatusNames = map[uint64]string{
	IceDetPowerCurve:     "Detection by the power curve method",
	IceDetPreventive:     "Preventive detection",
	IceDetExternalSensor: "External ice detection sensor (Labko)",
	IceDetExternalSCADA:  "External control signal from SCADA",
	IceDetParkDetection:  "Park-wide ice detection",
}

// IceDetStatusStrings decodes an ice detection status word into the systems that
// report ice, in ascending bit order.
func IceDetStatusStrings(status uint64) []string {
	if status == 0 {
		return []string{"No ice detected"}
	}
	bits := make([]uint64, 0, len(iceDetStatusNames))
	for mask := range iceDetStatusNames {
		if status&mask != 0 {
			bits = append(bits, mask)
		}
	}
	sort.Slice(bits, func(i, j int) bool { return bits[i] < bits[j] })
	out := make([]string, 0, len(bits))
	for _, b := range bits {
		out = append(out, iceDetStatusNames[b])
	}
	return out
}

// IceDetStatusString renders an ice detection status word as a single line.
func IceDetStatusString(status uint64) string {
	return strings.Join(IceDetStatusStrings(status), ", ")
}

// SessionState is the value of Loc/Wec/Plant<n>/{Ctrl,Reset}/SessionState.
type SessionState uint16

const (
	SessionFree               SessionState = 0
	SessionReserved           SessionState = 1
	SessionParameterInput     SessionState = 2
	SessionWaitLoop           SessionState = 3
	SessionWaitEnd            SessionState = 4
	SessionBlocked            SessionState = 5
	SessionOccupied           SessionState = 108
	SessionAccessDenied       SessionState = 109
	SessionValueError         SessionState = 121
	SessionIncorrectUserID    SessionState = 174
	SessionInsufficientRights SessionState = 175
)

var sessionNames = map[SessionState]string{
	SessionFree:               "session free",
	SessionReserved:           "session reserved",
	SessionParameterInput:     "parameter input",
	SessionWaitLoop:           "waiting time in loop mode",
	SessionWaitEnd:            "waiting time session end",
	SessionBlocked:            "session blocked (global reservation)",
	SessionOccupied:           "occupied",
	SessionAccessDenied:       "access denied",
	SessionValueError:         "value error",
	SessionIncorrectUserID:    "incorrect user id",
	SessionInsufficientRights: "insufficient rights",
}

// satisfies reports whether an observed session state fulfils a wait for want.
//
// After a submit the session may pass through SessionWaitLoop (3, "waiting time
// in loop mode") on its way to SessionWaitEnd (4). Reaching 3 already proves the
// submit was accepted, because only a submit moves a session out of parameter
// input, so it counts as the end of the procedure rather than as a failure to
// reach state 4 in time.
func (s SessionState) satisfies(want SessionState) bool {
	if s == want {
		return true
	}
	return want == SessionWaitEnd && s == SessionWaitLoop
}

func (s SessionState) String() string {
	if name, ok := sessionNames[s]; ok {
		return fmt.Sprintf("%q", name)
	}
	return fmt.Sprintf("unknown state %d", uint16(s))
}

// Rotor blade heating status bits, as read from Loc/Wec/Plant<n>/Ctrl/Rbh.
// Enercon transmits this status as a 16-bit word in which every bit carries one
// statement; the parameter numbers are the plant parameters the bit refers to.
const (
	RbhAutoDeicingAllowed        = uint64(1 << 0)  // Bit 0: automatic heating allowed (parameter 1314)
	RbhAutoOffWEA                = uint64(1 << 1)  // Bit 1: automatic operation suppressed (SCADA)
	RbhManualOnWEA               = uint64(1 << 2)  // Bit 2: heating manually on (inside the plant)
	RbhManualOnSCADA             = uint64(1 << 3)  // Bit 3: heating manually on (SCADA)
	RbhAutoDeicingWhenStopped    = uint64(1 << 4)  // Bit 4: automatic heating while the plant is stopped
	RbhAutoDeicingInOperation    = uint64(1 << 5)  // Bit 5: automatic heating in operation
	RbhHeatingPreventiveAuto     = uint64(1 << 6)  // Bit 6: heating switched on preventively (by automation)
	RbhHeatingWhenStoppedSCADA   = uint64(1 << 7)  // Bit 7: heating while the plant is stopped (SCADA)
	RbhHeatingInOperationSCADA   = uint64(1 << 8)  // Bit 8: heating while the plant is running (SCADA)
	RbhNoSupplyPowerAvailable    = uint64(1 << 10) // Bit 10: no supply power available
	RbhFault                     = uint64(1 << 11) // Bit 11: heating fault
	RbhDeicingAllowedInOperation = uint64(1 << 12) // Bit 12: heating in operation allowed (parameter 1315)
	RbhPreventiveHeaterAllowed   = uint64(1 << 13) // Bit 13: preventive heating allowed (parameter 1318)
	RbhInstalled                 = uint64(1 << 15) // Bit 15: heating installed
)

// Bits 9 and 14 are documented as unused.

// RbhNotInstalledValue is the whole-word value Enercon documents for a plant
// without rotor blade heating. The technical data sheet prints 65636; 1<<16
// (65536) is the value that follows from the 16-bit status word. Both lie
// outside the documented status word and both have the "installed" bit clear,
// so RbhIsInstalled recognises either — as well as a plain 0.
const RbhNotInstalledValue = uint64(65636)

// RbhIsInstalled reports whether a status word describes a plant that has rotor
// blade heating at all. It is the bit 15 test, which also covers the whole-word
// "not installed" values, since those have bit 15 clear.
func RbhIsInstalled(status uint64) bool {
	return status&RbhInstalled != 0
}

// rbhRunningMask covers every bit that means the heater is currently on. It is
// the Go form of the original (St & 508) test.
const rbhRunningMask = RbhManualOnWEA | RbhManualOnSCADA |
	RbhAutoDeicingWhenStopped | RbhAutoDeicingInOperation | RbhHeatingPreventiveAuto |
	RbhHeatingWhenStoppedSCADA | RbhHeatingInOperationSCADA

// rbhFaultMask covers the bits that prevent the heater from running. Whether the
// heating exists at all is a separate question, answered by RbhIsInstalled.
const rbhFaultMask = RbhNoSupplyPowerAvailable | RbhFault

var rbhStatusNames = map[uint64]string{
	RbhAutoDeicingAllowed:        "Automatic heating allowed",
	RbhAutoOffWEA:                "Automatic operation of the heating suppressed",
	RbhManualOnWEA:               "Heating manually on (inside the plant)",
	RbhManualOnSCADA:             "Heating manually on (SCADA)",
	RbhAutoDeicingWhenStopped:    "Automatic heating while the plant is stopped",
	RbhAutoDeicingInOperation:    "Automatic heating in operation",
	RbhHeatingPreventiveAuto:     "Heating switched on preventively (by automation)",
	RbhHeatingWhenStoppedSCADA:   "Heating while the plant is stopped (SCADA)",
	RbhHeatingInOperationSCADA:   "Heating while the plant is running (SCADA)",
	RbhNoSupplyPowerAvailable:    "No supply power available",
	RbhFault:                     "Heating fault",
	RbhDeicingAllowedInOperation: "Heating in operation allowed",
	RbhPreventiveHeaterAllowed:   "Preventive heating allowed",
	RbhInstalled:                 "Heating installed",
}

// RbhStatusStrings decodes a rotor blade heating status word into its set bits,
// in ascending bit order. A status of 0 means the heating cannot be accessed.
func RbhStatusStrings(status uint64) []string {
	if !RbhIsInstalled(status) {
		return []string{"No rotor blade heating installed"}
	}
	bits := make([]uint64, 0, len(rbhStatusNames))
	for mask := range rbhStatusNames {
		if status&mask != 0 {
			bits = append(bits, mask)
		}
	}
	sort.Slice(bits, func(i, j int) bool { return bits[i] < bits[j] })
	out := make([]string, 0, len(bits))
	for _, b := range bits {
		out = append(out, rbhStatusNames[b])
	}
	return out
}

// RbhStatusString renders a rotor blade heating status word as a single line.
func RbhStatusString(status uint64) string {
	return strings.Join(RbhStatusStrings(status), ", ")
}
