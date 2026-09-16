package energontrol

import (
	"errors"
	"fmt"
)

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
	return errors.Join(errs...)
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
//
// Err is non-nil when the state of this plant could not be established. Ctrl is
// then meaningless and must not be used as a process value — Ctrl 0 means
// "running", which is the last thing an unknown state should be read as. One
// unreadable plant does not invalidate the others, so a reading call returns
// one entry per requested plant and reports the failures here.
type PlantState struct {
	PlantNo uint8
	Ctrl    CtrlValue
	Err     error
}

func (s PlantState) String() string {
	if s.Err != nil {
		return fmt.Sprintf("plant %d: unknown: %v", s.PlantNo, s.Err)
	}
	return fmt.Sprintf("plant %d: %s", s.PlantNo, s.Ctrl)
}

// RbhState is the rotor blade heating status word of one plant. Decode it with
// RbhStatusStrings.
//
// Err is non-nil when the status of this plant could not be read; Status is then
// meaningless.
type RbhState struct {
	PlantNo uint8
	Status  uint64
	Err     error
}

func (s RbhState) String() string {
	if s.Err != nil {
		return fmt.Sprintf("plant %d: unknown: %v", s.PlantNo, s.Err)
	}
	return fmt.Sprintf("plant %d: %s", s.PlantNo, RbhStatusString(s.Status))
}

// IceDetState is the ice detection status word of one plant. It says which
// system detected ice. Decode it with IceDetStatusStrings.
//
// Err is non-nil when the status of this plant could not be read; Status is then
// meaningless.
type IceDetState struct {
	PlantNo uint8
	Status  uint64
	Err     error
}

func (s IceDetState) String() string {
	if s.Err != nil {
		return fmt.Sprintf("plant %d: unknown: %v", s.PlantNo, s.Err)
	}
	return fmt.Sprintf("plant %d: %s", s.PlantNo, IceDetStatusString(s.Status))
}

// plantScoped is what the per-plant state types have in common: each one knows
// which plant it describes and whether that plant could be read.
type plantScoped interface {
	plant() uint8
	err() error
}

func (s PlantState) plant() uint8  { return s.PlantNo }
func (s PlantState) err() error    { return s.Err }
func (s RbhState) plant() uint8    { return s.PlantNo }
func (s RbhState) err() error      { return s.Err }
func (s IceDetState) plant() uint8 { return s.PlantNo }
func (s IceDetState) err() error   { return s.Err }

// statesErr joins the per-plant errors of a state slice into one error.
func statesErr[T plantScoped](states []T) error {
	var errs []error
	for _, s := range states {
		if err := s.err(); err != nil {
			errs = append(errs, fmt.Errorf("plant %d: %w", s.plant(), err))
		}
	}
	return errors.Join(errs...)
}

// statesGet returns the entry for one plant, and whether it was requested.
func statesGet[T plantScoped](states []T, plant uint8) (T, bool) {
	for _, s := range states {
		if s.plant() == plant {
			return s, true
		}
	}
	var zero T
	return zero, false
}

// PlantStates is the result of reading the control state of several plants.
type PlantStates []PlantState

// Err returns a single error joining every plant whose state could not be
// established, or nil if every state was read. Use it where an unreadable plant
// should fail the whole call — it is the all-or-nothing answer these calls used
// to give unconditionally.
func (ss PlantStates) Err() error { return statesErr(ss) }

// Get returns the entry for one plant, and whether it was part of the request.
func (ss PlantStates) Get(plant uint8) (PlantState, bool) { return statesGet(ss, plant) }

// RbhStates is the result of reading the heating status of several plants.
type RbhStates []RbhState

// Err returns a single error joining every plant whose status could not be read.
func (ss RbhStates) Err() error { return statesErr(ss) }

// Get returns the entry for one plant, and whether it was part of the request.
func (ss RbhStates) Get(plant uint8) (RbhState, bool) { return statesGet(ss, plant) }

// IceDetStates is the result of reading the ice detection status of several
// plants.
type IceDetStates []IceDetState

// Err returns a single error joining every plant whose status could not be read.
func (ss IceDetStates) Err() error { return statesErr(ss) }

// Get returns the entry for one plant, and whether it was part of the request.
func (ss IceDetStates) Get(plant uint8) (IceDetState, bool) { return statesGet(ss, plant) }

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
	// already in a different stop state. With it false, a stop request is also
	// satisfied by a deeper stop and by a stop Enercon holds the plant in, and by
	// nothing else. In v1 this was hardwired to false here while Stop exposed it;
	// it is now explicit in both places.
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

	// Unsupported lists the item names below Loc/Wec that look like a plant but
	// could not be taken into PlantNo — a number above 255, which the uint8
	// plant number cannot represent, or a name that does not follow the
	// Loc/Wec/Plant<n> scheme.
	//
	// It exists so a plant can never disappear from a park listing without a
	// trace: a plant that is not in PlantNo is never commanded and never
	// monitored, and a caller has to be able to see that this happened. In
	// practice it stays empty — an Enercon park holds 20 to 30 turbines, well
	// inside the range (see the package documentation on plant numbers) — which
	// is exactly why the case needs reporting rather than trust: nobody would
	// notice a silent drop in a situation nobody expects.
	Unsupported []string
}
