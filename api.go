package energontrol

import "context"

// The functions below are convenience wrappers for callers that do not need to
// configure a Client. Each one builds a Client with default settings; use New
// directly to attach a logger, change the session polling budget, enable the
// staleness check or install a session release function.

// Start runs the given plants. See Client.Start.
func Start(ctx context.Context, t Transport, userID uint64, plants ...uint8) (Results, error) {
	return New(t).Start(ctx, userID, plants...)
}

// Stop stops the given plants. See Client.Stop.
func Stop(ctx context.Context, t Transport, userID uint64, fullStop, forceExplicitCommand bool,
	plants ...uint8) (Results, error) {
	return New(t).Stop(ctx, userID, fullStop, forceExplicitCommand, plants...)
}

// SetCtrl sends an arbitrary documented control value. See Client.SetCtrl.
func SetCtrl(ctx context.Context, t Transport, userID uint64, value CtrlValue,
	forceExplicitCommand bool, plants ...uint8) (Results, error) {
	return New(t).SetCtrl(ctx, userID, value, forceExplicitCommand, plants...)
}

// SetRbh sends an arbitrary documented heating value. See Client.SetRbh.
func SetRbh(ctx context.Context, t Transport, userID uint64, value RbhValue,
	plants ...uint8) (Results, error) {
	return New(t).SetRbh(ctx, userID, value, plants...)
}

// IceDetOn switches the ice warning lamp on. See Client.IceDetOn.
func IceDetOn(ctx context.Context, t Transport, userID uint64, plants ...uint8) (Results, error) {
	return New(t).IceDetOn(ctx, userID, plants...)
}

// IceDetOff switches the ice warning lamp off. See Client.IceDetOff.
func IceDetOff(ctx context.Context, t Transport, userID uint64, plants ...uint8) (Results, error) {
	return New(t).IceDetOff(ctx, userID, plants...)
}

// SetIceDet writes an ice detection value. See Client.SetIceDet.
func SetIceDet(ctx context.Context, t Transport, userID uint64, value IceDetValue,
	plants ...uint8) (Results, error) {
	return New(t).SetIceDet(ctx, userID, value, plants...)
}

// Reset acknowledges faults on the given plants. See Client.Reset.
func Reset(ctx context.Context, t Transport, userID uint64, plants ...uint8) (Results, error) {
	return New(t).Reset(ctx, userID, plants...)
}

// RbhOn switches the rotor blade heating on. See Client.RbhOn.
func RbhOn(ctx context.Context, t Transport, userID uint64, plants ...uint8) (Results, error) {
	return New(t).RbhOn(ctx, userID, plants...)
}

// RbhAutoOff suppresses automatic heating operation. See Client.RbhAutoOff.
func RbhAutoOff(ctx context.Context, t Transport, userID uint64, plants ...uint8) (Results, error) {
	return New(t).RbhAutoOff(ctx, userID, plants...)
}

// RbhStandard returns the heating to automatic control. See Client.RbhStandard.
func RbhStandard(ctx context.Context, t Transport, userID uint64, plants ...uint8) (Results, error) {
	return New(t).RbhStandard(ctx, userID, plants...)
}

// ControlAndRbh sets a control and a heating value in one session.
// See Client.ControlAndRbh.
func ControlAndRbh(ctx context.Context, t Transport, userID uint64, values ControlAndRbhValue,
	plants ...uint8) (Results, error) {
	return New(t).ControlAndRbh(ctx, userID, values, plants...)
}

// Turbines lists the plants of the park. See Client.Turbines.
func Turbines(ctx context.Context, t Transport) (TurbineInfo, error) {
	return New(t).Turbines(ctx)
}

// ParkNoMatch reports whether the server serves the given park.
// See Client.ParkNoMatch.
func ParkNoMatch(ctx context.Context, t Transport, parkNo uint64, checkAvailable bool) (bool, error) {
	return New(t).ParkNoMatch(ctx, parkNo, checkAvailable)
}

// PlantCtrlState reads the control state of the given plants.
// See Client.PlantCtrlState.
func PlantCtrlState(ctx context.Context, t Transport, plants ...uint8) (PlantStates, error) {
	return New(t).PlantCtrlState(ctx, plants...)
}

// PlantRbhState reads the heating status of the given plants.
// See Client.PlantRbhState.
func PlantRbhState(ctx context.Context, t Transport, plants ...uint8) (RbhStates, error) {
	return New(t).PlantRbhState(ctx, plants...)
}

// PlantIceDetState reads the ice detection status of the given plants.
// See Client.PlantIceDetState.
func PlantIceDetState(ctx context.Context, t Transport, plants ...uint8) (IceDetStates, error) {
	return New(t).PlantIceDetState(ctx, plants...)
}

// ServerAvailable reports whether the OPC server is reachable and running.
// See Client.ServerAvailable.
func ServerAvailable(ctx context.Context, t Transport) error {
	return New(t).ServerAvailable(ctx)
}
