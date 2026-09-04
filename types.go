package energontrol

import "fmt"

// Outcome describes what happened to a single plant. It exists because a bool
// cannot distinguish "I sent the command" from "no command was needed" from
// "the command is not possible" — a distinction a control loop must be able to
// make.
type Outcome int

const (
	// OutcomeCommanded means the value was written and the plant's control
	// session confirmed it.
	OutcomeCommanded Outcome = iota
	// OutcomeAlreadyInState means the plant was already in the requested state,
	// so no session was opened and nothing was written.
	OutcomeAlreadyInState
	// OutcomeNotPermitted means the requested state cannot be reached, because
	// the plant is under Enercon control or its state is unknown. Err says which.
	OutcomeNotPermitted
	// OutcomeFailed means the command was attempted but did not complete. Err
	// carries the reason.
	OutcomeFailed
)

func (o Outcome) String() string {
	switch o {
	case OutcomeCommanded:
		return "commanded"
	case OutcomeAlreadyInState:
		return "already in state"
	case OutcomeNotPermitted:
		return "not permitted"
	case OutcomeFailed:
		return "failed"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// PlantResult is the per-plant result of a command. Every result carries its own
// plant number, so results never have to be matched to the input by position.
type PlantResult struct {
	PlantNo uint8
	Outcome Outcome
	// Err is nil for OutcomeCommanded and OutcomeAlreadyInState. For the other
	// outcomes it wraps one of this package's sentinel errors.
	Err error
}

// InRequestedState reports whether the plant is now in the state the caller
// asked for — either because the command was executed or because the plant was
// already there.
//
// Use this instead of "err == nil" when deciding whether a setpoint has been
// reached. It is deliberately false for OutcomeNotPermitted, including for a
// plant whose state is unknown because of a communication error.
func (r PlantResult) InRequestedState() bool {
	return r.Err == nil && (r.Outcome == OutcomeCommanded || r.Outcome == OutcomeAlreadyInState)
}

func (r PlantResult) String() string {
	if r.Err != nil {
		return fmt.Sprintf("plant %d: %s: %v", r.PlantNo, r.Outcome, r.Err)
	}
	return fmt.Sprintf("plant %d: %s", r.PlantNo, r.Outcome)
}

// Results is a list of per-plant results.
type Results []PlantResult

// Err returns a single error joining every failed plant, or nil if all plants
// reached the requested state. Use it for the common "did this work at all?"
// check; inspect the individual results for per-plant handling.
func (rs Results) Err() error {
	var errs []error
	for _, r := range rs {
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("plant %d: %w", r.PlantNo, r.Err))
		}
	}
	return joinErrors(errs)
}

// InRequestedState reports whether every plant reached the requested state.
func (rs Results) InRequestedState() bool {
	for _, r := range rs {
		if !r.InRequestedState() {
			return false
		}
	}
	return len(rs) > 0
}

// PlantState is the control state of one plant.
type PlantState struct {
	PlantNo uint8
	Ctrl    CtrlValue
}

// RbhState is the rotor blade heating status word of one plant. Decode it with
// RbhStatusStrings.
type RbhState struct {
	PlantNo uint8
	Status  uint64
}

// IceDetState is the ice detection status word of one plant. It says which
// system detected ice. Decode it with IceDetStatusStrings.
type IceDetState struct {
	PlantNo uint8
	Status  uint64
}

// ControlAndRbhValue describes a command that combines a control value, a
// heating value and an ice warning lamp value. Enercon transmits them in one
// session, so any combination of the three costs a single session per plant.
//
// Unlike v1 this struct carries no per-plant action slices: which plants need a
// command is derived internally, per plant, and never indexed by position.
type ControlAndRbhValue struct {
	// SetCtrlValue enables the CtrlValue part of the command.
	SetCtrlValue bool
	// CtrlValue is the control value to write. It must satisfy Writable.
	CtrlValue CtrlValue
	// SetRbhValue enables the RbhValue part of the command.
	SetRbhValue bool
	// RbhValue is the heating value to write. It must satisfy Writable.
	RbhValue RbhValue
	// SetIceDetValue enables the IceDetValue part of the command.
	SetIceDetValue bool
	// IceDetValue switches the ice warning lamp. It must satisfy Writable.
	IceDetValue IceDetValue
	// ForceExplicitCommand requests the exact CtrlValue even when the plant is
	// already in a different stop state. With it false, any stop state satisfies
	// a stop request. In v1 this was hardwired to false here while Stop exposed
	// it; it is now explicit in both places.
	ForceExplicitCommand bool
}

// TurbineInfo lists the plants of a park and which functions each one offers.
type TurbineInfo struct {
	ParkNo  uint64
	PlantNo []uint8
	Ctrl    map[uint8]bool
	Rbh     map[uint8]bool
	Reset   map[uint8]bool
	Para    map[uint8]bool
	IceDet  map[uint8]bool
}
