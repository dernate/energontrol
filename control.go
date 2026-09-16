package energontrol

import (
	"context"
	"fmt"
	"math"
	"slices"
)

// PlantCtrlState reads the control state of the given plants.
//
// It returns one entry per requested plant, in request order. A plant whose
// state could not be established carries that reason in its Err and its Ctrl
// must not be used: a missing item, a faulted item, an item whose quality is not
// good and a stale item all mean the state of that plant is unknown, and an
// unknown state must not be mistaken for "running".
//
// The returned error is reserved for failures of the whole request — transport,
// a server that is not running, a response that cannot be correlated. One
// unreadable plant does not blind the caller to the others: this call is the
// documented way to monitor a park, and a park is exactly what would be lost.
// Use PlantStates.Err to fail on any unreadable plant.
func (c *Client) PlantCtrlState(ctx context.Context, plants ...uint8) (PlantStates, error) {
	if err := validatePlants(plants); err != nil {
		return nil, err
	}
	states, readErrs, err := c.readCtrlStates(ctx, plants)
	if err != nil {
		return nil, err
	}
	out := make(PlantStates, 0, len(plants))
	for _, p := range plants {
		out = append(out, PlantState{PlantNo: p, Ctrl: states[p], Err: readErrs[p]})
	}
	return out, nil
}

// PlantIceDetState reads the ice detection status of the given plants. Decode a
// status word with IceDetStatusStrings.
//
// Like PlantCtrlState it returns one entry per requested plant and reports a
// plant that could not be read in that entry's Err.
func (c *Client) PlantIceDetState(ctx context.Context, plants ...uint8) (IceDetStates, error) {
	if err := validatePlants(plants); err != nil {
		return nil, err
	}
	states, readErrs, err := c.readIceDetStates(ctx, plants)
	if err != nil {
		return nil, err
	}
	out := make(IceDetStates, 0, len(plants))
	for _, p := range plants {
		out = append(out, IceDetState{PlantNo: p, Status: states[p], Err: readErrs[p]})
	}
	return out, nil
}

// PlantRbhState reads the rotor blade heating status word of the given plants.
// Decode a status word with RbhStatusStrings.
//
// Like PlantCtrlState it returns one entry per requested plant and reports a
// plant that could not be read in that entry's Err.
func (c *Client) PlantRbhState(ctx context.Context, plants ...uint8) (RbhStates, error) {
	if err := validatePlants(plants); err != nil {
		return nil, err
	}
	states, readErrs, err := c.readRbhStates(ctx, plants)
	if err != nil {
		return nil, err
	}
	out := make(RbhStates, 0, len(plants))
	for _, p := range plants {
		out = append(out, RbhState{PlantNo: p, Status: states[p], Err: readErrs[p]})
	}
	return out, nil
}

func (c *Client) readCtrlStates(ctx context.Context, plants []uint8) (
	map[uint8]CtrlValue, map[uint8]error, error) {
	names := make([]string, len(plants))
	owner := make(map[string]uint8, len(plants))
	for i, p := range plants {
		names[i] = c.items.ctrl(p)
		owner[names[i]] = p
	}
	values, err := c.readValues(ctx, names)
	if err != nil {
		return nil, nil, err
	}
	states := make(map[uint8]CtrlValue, len(plants))
	errs := make(map[uint8]error)
	for name, v := range values {
		if v.Err != nil {
			errs[owner[name]] = v.Err
			continue
		}
		states[owner[name]] = CtrlValue(v.Value)
	}
	return states, errs, nil
}

func (c *Client) readIceDetStates(ctx context.Context, plants []uint8) (
	map[uint8]uint64, map[uint8]error, error) {
	return c.readPlantWords(ctx, plants, c.items.iceDet)
}

func (c *Client) readRbhStates(ctx context.Context, plants []uint8) (
	map[uint8]uint64, map[uint8]error, error) {
	return c.readPlantWords(ctx, plants, c.items.rbh)
}

// readPlantWords reads one scalar item per plant in a single request. The
// per-plant errors are returned separately so one unreadable plant does not fail
// a command for the whole park.
func (c *Client) readPlantWords(ctx context.Context, plants []uint8,
	item func(uint8) string) (map[uint8]uint64, map[uint8]error, error) {
	names := make([]string, len(plants))
	owner := make(map[string]uint8, len(plants))
	for i, p := range plants {
		names[i] = item(p)
		owner[names[i]] = p
	}
	values, err := c.readValues(ctx, names)
	if err != nil {
		return nil, nil, err
	}
	states := make(map[uint8]uint64, len(plants))
	errs := make(map[uint8]error)
	for name, v := range values {
		if v.Err != nil {
			errs[owner[name]] = v.Err
			continue
		}
		states[owner[name]] = v.Value
	}
	return states, errs, nil
}

// Start runs the given plants.
//
// A plant that is already running yields OutcomeAlreadyInState. A plant that
// Enercon stopped with higher rights, or whose state is unknown because of a
// communication error, yields OutcomeNotPermitted with ErrPlantUnderEnerconControl
// or ErrPlantCommunication — v1 reported those plants as successfully started.
func (c *Client) Start(ctx context.Context, userID uint64, plants ...uint8) (Results, error) {
	return c.setCtrl(ctx, userID, CtrlStart, false, plants)
}

// Stop stops the given plants. fullStop selects a 90° stop over a 60° stop.
//
// With forceExplicitCommand false, the request is also satisfied by a *deeper*
// stop — a plant at 90° satisfies a request for 60°, since commanding it would
// open the blades back up — and by a stop Enercon holds the plant in at least
// as deep as the one asked for. Nothing else: a plant idling at 60° is commanded
// on to 90° when fullStop is set, and a plant stopped at 60° by some other
// command is commanded to the plain stop. With forceExplicitCommand true the
// plant must have carried out exactly this command; see CtrlValue.Reached.
//
// A plant in CtrlCommError is never reported as stopped: its state is unknown.
func (c *Client) Stop(ctx context.Context, userID uint64, fullStop, forceExplicitCommand bool,
	plants ...uint8) (Results, error) {
	want := CtrlStop60
	if fullStop {
		want = CtrlStop90
	}
	return c.setCtrl(ctx, userID, want, forceExplicitCommand, plants)
}

func (c *Client) setCtrl(ctx context.Context, userID uint64, want CtrlValue, force bool,
	plants []uint8) (Results, error) {
	if err := validatePlants(plants); err != nil {
		return nil, err
	}
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if !want.Writable() {
		return nil, fmt.Errorf("%w: Ctrl value %s", ErrInvalidValue, want)
	}
	ctx, cancel := c.withCommandTimeout(ctx)
	defer cancel()
	unlock, err := c.lockPlants(ctx, plants)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := c.ServerAvailable(ctx); err != nil {
		return nil, err
	}
	states, readErrs, err := c.readCtrlStates(ctx, plants)
	if err != nil {
		return nil, err
	}
	rs := newResultSet(plants)
	var cmds []plantCommand
	for _, p := range plants {
		if e := readErrs[p]; e != nil {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeFailed, Err: e})
			continue
		}
		current := states[p]
		if ctrlSatisfied(current, want, force) {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeAlreadyInState})
			continue
		}
		if e := current.stateError(); e != nil {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeNotPermitted, Err: e})
			continue
		}
		cmds = append(cmds, plantCommand{PlantNo: p, SetCtrl: true, CtrlValue: want})
	}
	rs.merge(c.controlProcedure(ctx, userID, cmds))
	return rs.list(), nil
}

// ctrlSatisfied reports whether a plant in state current already fulfils a
// request for want — that is, whether sending the command would be wrong or
// impossible rather than merely redundant.
//
// A forced request is satisfied only by the command having been carried out,
// which Reached decides: Ctrl carries the value that was set.
//
// An unforced request adds the two cases the tolerance exists for, and no
// others:
//
//   - The plant is a *deeper* stop than the one asked for. Commanding it would
//     open the blades back up, which is not what a caller asking for a stop
//     wants.
//   - Enercon holds the plant with higher rights and it is at least as stopped
//     as asked. A client cannot command it away in any case.
//
// A stop at the same blade angle by a *different* command does not satisfy the
// request: a plant stopped for species protection at 60° is not in a plain 60°
// stop, the two are different operating modes, and the plant can be commanded.
// Nor does a state that says nothing about where the blades are, or a command
// that names no blade angle — stop for ice detection, stop for shadow flicker —
// which is why those are always sent.
func ctrlSatisfied(current, want CtrlValue, force bool) bool {
	if current.Reached(want) {
		return true
	}
	if force || want == CtrlStart {
		return false
	}
	wantDepth := want.stopDepth()
	if wantDepth < 0 {
		return false
	}
	if current == CtrlStop60Enercon || current == CtrlStopEnercon {
		return current.stopDepth() >= wantDepth
	}
	return current.stopDepth() > wantDepth
}

// SetCtrl sends an arbitrary documented control value.
//
// Start and Stop cover the three values most callers need; this is the way to
// reach the rest of the set Enercon defines for SetCtrl — the gradient stops and
// the stops for ice detection, shadow flicker and species protection.
//
// forceExplicitCommand has the same meaning as in Stop: with it false, a stop
// request is also satisfied by a deeper stop and by a stop Enercon holds the
// plant in; with it true the plant must report exactly this command. The stops
// for ice detection and shadow flicker name no blade angle, so nothing satisfies
// them and they are always sent.
func (c *Client) SetCtrl(ctx context.Context, userID uint64, value CtrlValue,
	forceExplicitCommand bool, plants ...uint8) (Results, error) {
	return c.setCtrl(ctx, userID, value, forceExplicitCommand, plants)
}

// SetRbh sends an arbitrary documented rotor blade heating value, including
// RbhSetPresetDuration, which the named methods do not cover.
func (c *Client) SetRbh(ctx context.Context, userID uint64, value RbhValue,
	plants ...uint8) (Results, error) {
	return c.setRbh(ctx, userID, value, plants)
}

// RbhOn switches the rotor blade heating on from SCADA, by writing 10 —
// "suppress automatic operation and switch the heating on manually". The bare
// value 8 the data sheet also lists is rejected by the server.
//
// A plant whose heating is already running yields OutcomeAlreadyInState,
// whatever switched it on: the request is "the heating runs", not "this client's
// manual-on bit is set".
func (c *Client) RbhOn(ctx context.Context, userID uint64, plants ...uint8) (Results, error) {
	return c.setRbh(ctx, userID, RbhSetManualOn, plants)
}

// RbhAutoOff suppresses automatic operation of the rotor blade heating.
func (c *Client) RbhAutoOff(ctx context.Context, userID uint64, plants ...uint8) (Results, error) {
	return c.setRbh(ctx, userID, RbhSetAutoOff, plants)
}

// RbhStandard hands the rotor blade heating back to the automatic system.
func (c *Client) RbhStandard(ctx context.Context, userID uint64, plants ...uint8) (Results, error) {
	return c.setRbh(ctx, userID, RbhSetStandard, plants)
}

func (c *Client) setRbh(ctx context.Context, userID uint64, want RbhValue, plants []uint8) (Results, error) {
	if err := validatePlants(plants); err != nil {
		return nil, err
	}
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if !want.Writable() {
		return nil, fmt.Errorf("%w: Rbh value %s", ErrInvalidValue, want)
	}
	ctx, cancel := c.withCommandTimeout(ctx)
	defer cancel()
	unlock, err := c.lockPlants(ctx, plants)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := c.ServerAvailable(ctx); err != nil {
		return nil, err
	}
	states, readErrs, err := c.readRbhStates(ctx, plants)
	if err != nil {
		return nil, err
	}
	rs := newResultSet(plants)
	var cmds []plantCommand
	for _, p := range plants {
		if e := readErrs[p]; e != nil {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeFailed, Err: e})
			continue
		}
		status := states[p]
		if e := rbhStateError(status); e != nil {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeNotPermitted, Err: e})
			continue
		}
		if rbhSatisfied(status, want) {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeAlreadyInState})
			continue
		}
		cmds = append(cmds, plantCommand{PlantNo: p, SetRbh: true, RbhValue: want})
	}
	rs.merge(c.controlProcedure(ctx, userID, cmds))
	return rs.list(), nil
}

// rbhSatisfied reports whether a heating status word already fulfils a request.
//
// The three cases are the original Enercon rules; the AutoOff case is the
// simplified but equivalent form of v1's ((a&8) ^ (a&2)) && !(a&8).
func rbhSatisfied(status uint64, want RbhValue) bool {
	if !RbhIsInstalled(status) {
		return false
	}
	switch want {
	case RbhSetStandard:
		// Neither suppress the automatic system nor heat manually. With the
		// "automatic suppressed" bit clear, the heating is back under automatic
		// control.
		return status&RbhAutoOffWEA == 0
	case RbhSetAutoOff:
		// Automatic suppressed, and not manually switched on from SCADA.
		return status&RbhAutoOffWEA != 0 && status&RbhManualOnSCADA == 0
	case RbhSetManualOn:
		// The request is "the heating runs", not "the manual-on bit is set", so
		// a plant already heating satisfies it whatever switched it on. Any bit
		// that means "heating running", and no bit that prevents it. This is the
		// original Enercon rule, (St & 508) && !(St & 68608).
		return status&rbhRunningMask != 0 && status&rbhFaultMask == 0
	default:
		// RbhSetPresetDuration switches the heating on for a preset duration.
		// It is a one-shot action with no status bit of its own, so it can
		// never be "already in state" and is always sent.
		return false
	}
}

// rbhStateError reports why the heating of a plant cannot be commanded.
//
// Enercon marks an installed heating with bit 15 of the status word and
// documents a whole-word value for a plant without one. Both cases, and a status
// word of 0, come out of the same test.
func rbhStateError(status uint64) error {
	if !RbhIsInstalled(status) {
		return fmt.Errorf("%w (status word %d)", ErrRbhUnavailable, status)
	}
	return nil
}

// IceDetOn switches the ice warning lamp on.
func (c *Client) IceDetOn(ctx context.Context, userID uint64, plants ...uint8) (Results, error) {
	return c.setIceDet(ctx, userID, IceDetLampOn, plants)
}

// IceDetOff switches the ice warning lamp off.
func (c *Client) IceDetOff(ctx context.Context, userID uint64, plants ...uint8) (Results, error) {
	return c.setIceDet(ctx, userID, IceDetLampOff, plants)
}

// SetIceDet writes an ice detection value. See IceDetOn and IceDetOff.
func (c *Client) SetIceDet(ctx context.Context, userID uint64, value IceDetValue,
	plants ...uint8) (Results, error) {
	return c.setIceDet(ctx, userID, value, plants)
}

func (c *Client) setIceDet(ctx context.Context, userID uint64, want IceDetValue,
	plants []uint8) (Results, error) {
	if err := validatePlants(plants); err != nil {
		return nil, err
	}
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if !want.Writable() {
		return nil, fmt.Errorf("%w: IceDet value %s", ErrInvalidValue, want)
	}
	ctx, cancel := c.withCommandTimeout(ctx)
	defer cancel()
	unlock, err := c.lockPlants(ctx, plants)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := c.ServerAvailable(ctx); err != nil {
		return nil, err
	}
	states, readErrs, err := c.readIceDetStates(ctx, plants)
	if err != nil {
		return nil, err
	}
	rs := newResultSet(plants)
	var cmds []plantCommand
	for _, p := range plants {
		if e := readErrs[p]; e != nil {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeFailed, Err: e})
			continue
		}
		if iceDetSatisfied(states[p], want) {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeAlreadyInState})
			continue
		}
		cmds = append(cmds, plantCommand{PlantNo: p, SetIceDet: true, IceDetValue: want})
	}
	rs.merge(c.controlProcedure(ctx, userID, cmds))
	return rs.list(), nil
}

// iceDetSatisfied reports whether the ice detection status already reflects the
// requested lamp state.
//
// Switching the lamp on from SCADA is what the plant reports as
// IceDetExternalSCADA, so that bit is the read-back of this command. Other
// detection systems set their own bits and are not affected by it.
func iceDetSatisfied(status uint64, want IceDetValue) bool {
	on := status&IceDetExternalSCADA != 0
	return on == (want == IceDetLampOn)
}

// ControlAndRbh sets a control value and a heating value in one session.
//
// If any requested part of the command cannot be carried out for a plant, that
// plant yields OutcomeNotPermitted and nothing is written for it. The command is
// treated as one unit on purpose: silently executing half of it is the kind of
// partial success this API is built to avoid.
func (c *Client) ControlAndRbh(ctx context.Context, userID uint64, values ControlAndRbhValue,
	plants ...uint8) (Results, error) {
	if err := validatePlants(plants); err != nil {
		return nil, err
	}
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if !values.SetCtrlValue && !values.SetRbhValue && !values.SetIceDetValue {
		return nil, ErrNothingRequested
	}
	if values.SetCtrlValue && !values.CtrlValue.Writable() {
		return nil, fmt.Errorf("%w: Ctrl value %s", ErrInvalidValue, values.CtrlValue)
	}
	if values.SetRbhValue && !values.RbhValue.Writable() {
		return nil, fmt.Errorf("%w: Rbh value %s", ErrInvalidValue, values.RbhValue)
	}
	if values.SetIceDetValue && !values.IceDetValue.Writable() {
		return nil, fmt.Errorf("%w: IceDet value %s", ErrInvalidValue, values.IceDetValue)
	}
	ctx, cancel := c.withCommandTimeout(ctx)
	defer cancel()
	unlock, lockErr := c.lockPlants(ctx, plants)
	if lockErr != nil {
		return nil, lockErr
	}
	defer unlock()
	if err := c.ServerAvailable(ctx); err != nil {
		return nil, err
	}

	var (
		ctrlStates   map[uint8]CtrlValue
		ctrlReadErrs map[uint8]error
		rbhStates    map[uint8]uint64
		rbhReadErrs  map[uint8]error
		iceStates    map[uint8]uint64
		iceReadErrs  map[uint8]error
		err          error
	)
	if values.SetCtrlValue {
		if ctrlStates, ctrlReadErrs, err = c.readCtrlStates(ctx, plants); err != nil {
			return nil, err
		}
	}
	if values.SetRbhValue {
		if rbhStates, rbhReadErrs, err = c.readRbhStates(ctx, plants); err != nil {
			return nil, err
		}
	}
	if values.SetIceDetValue {
		if iceStates, iceReadErrs, err = c.readIceDetStates(ctx, plants); err != nil {
			return nil, err
		}
	}

	rs := newResultSet(plants)
	var cmds []plantCommand
	for _, p := range plants {
		if e := firstErr(ctrlReadErrs[p], rbhReadErrs[p], iceReadErrs[p]); e != nil {
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeFailed, Err: e})
			continue
		}
		cmd := plantCommand{PlantNo: p}
		var notPermitted error
		if values.SetCtrlValue {
			current := ctrlStates[p]
			stateErr := current.stateError()
			switch {
			case ctrlSatisfied(current, values.CtrlValue, values.ForceExplicitCommand):
				// nothing to do for the Ctrl part
			case stateErr != nil:
				notPermitted = stateErr
			default:
				cmd.SetCtrl = true
				cmd.CtrlValue = values.CtrlValue
			}
		}
		if notPermitted == nil && values.SetRbhValue {
			status := rbhStates[p]
			rbhErr := rbhStateError(status)
			switch {
			case rbhErr != nil:
				notPermitted = rbhErr
			case rbhSatisfied(status, values.RbhValue):
				// nothing to do for the Rbh part
			default:
				cmd.SetRbh = true
				cmd.RbhValue = values.RbhValue
			}
		}
		if notPermitted == nil && values.SetIceDetValue {
			if !iceDetSatisfied(iceStates[p], values.IceDetValue) {
				cmd.SetIceDet = true
				cmd.IceDetValue = values.IceDetValue
			}
		}
		switch {
		case notPermitted != nil:
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeNotPermitted, Err: notPermitted})
		case !cmd.SetCtrl && !cmd.SetRbh && !cmd.SetIceDet:
			rs.set(PlantResult{PlantNo: p, Outcome: OutcomeAlreadyInState})
		default:
			cmds = append(cmds, cmd)
		}
	}
	rs.merge(c.controlProcedure(ctx, userID, cmds))
	return rs.list(), nil
}

// Reset acknowledges faults on the given plants.
//
// Unlike a control command a reset has no target state to compare against, so
// every plant is sent through the session procedure.
func (c *Client) Reset(ctx context.Context, userID uint64, plants ...uint8) (Results, error) {
	if err := validatePlants(plants); err != nil {
		return nil, err
	}
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	ctx, cancel := c.withCommandTimeout(ctx)
	defer cancel()
	unlock, err := c.lockPlants(ctx, plants)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := c.ServerAvailable(ctx); err != nil {
		return nil, err
	}
	cmds := make([]plantCommand, 0, len(plants))
	for _, p := range plants {
		cmds = append(cmds, plantCommand{PlantNo: p})
	}
	rs := newResultSet(plants)
	rs.merge(c.resetProcedure(ctx, userID, cmds))
	return rs.list(), nil
}

// resultSet collects per-plant results and returns them in request order,
// regardless of which stage produced them.
type resultSet struct {
	order []uint8
	by    map[uint8]PlantResult
}

// newResultSet copies the plant list. It is the caller's variadic slice —
// client.Start(ctx, id, plants...) passes its own backing array — and the order
// results are reported in is read from it after the command has finished, so a
// caller reusing the slice would otherwise get results attributed to plants it
// never asked about. lockPlants copies for the same reason.
func newResultSet(plants []uint8) *resultSet {
	return &resultSet{order: slices.Clone(plants), by: make(map[uint8]PlantResult, len(plants))}
}

func (r *resultSet) set(res PlantResult) { r.by[res.PlantNo] = res }

func (r *resultSet) merge(results Results) {
	for _, res := range results {
		r.by[res.PlantNo] = res
	}
}

func (r *resultSet) list() Results {
	out := make(Results, 0, len(r.order))
	for _, p := range r.order {
		res, ok := r.by[p]
		if !ok {
			res = PlantResult{
				PlantNo: p,
				Outcome: OutcomeFailed,
				Err:     fmt.Errorf("energontrol: plant %d: no result was produced", p),
			}
		}
		out = append(out, res)
	}
	return out
}

// validateUserID rejects a user id that does not fit into the long word Enercon
// defines for it.
//
// It is checked at every public entry point, before anything is sent: the id is
// a property of the call, identical for every plant, so a value out of range is
// an argument error and not a per-plant failure. Reporting it as one — which an
// earlier draft did, after three reads had already gone to the SCADA — hid a
// configuration mistake inside a plant result that a caller checking err would
// never see.
func validateUserID(userID uint64) error {
	if userID > math.MaxUint32 {
		return fmt.Errorf("%w: %d", ErrInvalidUserID, userID)
	}
	return nil
}

// validatePlants rejects an empty list and duplicates. Two entries for one plant
// would open two competing sessions on the same turbine.
func validatePlants(plants []uint8) error {
	if len(plants) == 0 {
		return ErrNoPlants
	}
	seen := make(map[uint8]struct{}, len(plants))
	for _, p := range plants {
		if _, dup := seen[p]; dup {
			return fmt.Errorf("%w: plant %d listed more than once", ErrDuplicatePlant, p)
		}
		seen[p] = struct{}{}
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
