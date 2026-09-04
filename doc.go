// Package energontrol sends control commands to Enercon wind turbines over
// OPC XML-DA.
//
// It wraps the Enercon control session protocol: read the current state, decide
// which plants actually need a command, reserve a session per plant, write the
// value, submit it, and confirm that the session reached its final state.
//
// # Wind turbines are critical infrastructure
//
// Every command in this package moves a real machine. Test against a test park,
// and read the safety contract below before using the results of a command to
// drive anything.
//
// # The safety contract
//
// A command returns one PlantResult per plant. Use PlantResult.InRequestedState
// — not "Err == nil" — to decide whether a setpoint has been reached:
//
//	res, err := client.Stop(ctx, userID, true, false, 2, 4)
//	if err != nil {
//	    return err // the command could not be attempted at all
//	}
//	for _, r := range res {
//	    if !r.InRequestedState() {
//	        log.Printf("plant %d is not stopped: %v", r.PlantNo, r.Err)
//	    }
//	}
//
// Three rules follow from the fact that a wrong "yes" is more dangerous than a
// "no":
//
//   - A plant whose state cannot be established is never reported as being in
//     the requested state. A missing item, a faulted item, an item whose quality
//     is not good, and a plant in CtrlCommError all mean the state is unknown.
//   - A plant that cannot be commanded yields OutcomeNotPermitted with a reason,
//     not a quiet success. Plants stopped by Enercon with higher rights are the
//     usual case.
//   - Success is reported only for plants where a value was actually written and
//     the session confirmed it. Reaching the final session state is not by
//     itself evidence that anything was written.
//
// # A command is not a confirmation that the plant moved
//
// OutcomeCommanded means the session was reserved, this client's session id was
// verified, the value was written, read back, and submitted, and the session
// reached its final state. It does not mean the turbine has reached the state
// yet. Enercon is explicit about this: "the status resulting from a start and
// stop of the wind turbine must be monitored. Monitoring the status is the
// responsibility of the customer or the operator." Poll PlantCtrlState until the
// plant reports the expected state.
//
// # Timing
//
// The Enercon technical data sheet gives these timings for control access to a
// single plant. They constrain how a caller may schedule commands:
//
//   - Session timeout: 60 s. A session that cannot be completed expires after
//     this; there is no documented way to abort one earlier.
//     The default polling budget for a state transition is one second, well
//     inside this; raise it with WithSessionPolling if a SCADA needs longer.
//   - Extension timeout after setting a value: 60 s.
//   - Delay before a new reservation after a stop: 360 s. A plant that was just
//     stopped answers "occupied" for six minutes. Do not build a control loop
//     that re-sends a stop faster than that.
//   - Delay before a new reservation after a start: 0 s.
//   - Error timeout after three wrong entries: 180 s.
//   - Wrong user id: 300 s.
//
// The last two are why this package never retries a write: three rejected keys
// lock the plant out for three minutes, and a wrong user id for five. Errors
// wrapping ErrInsufficientRights or ErrIncorrectUserID must not be retried.
//
// # Correlating responses
//
// Response items are matched to request items by ClientItemHandle, and by
// ItemName where no handle is returned. Position is never used: OPC XML-DA does
// not guarantee that a response lists items in request order, and a positional
// mismatch would apply a command to the wrong turbine. A response that carries
// neither is rejected with ErrUncorrelatable.
//
// # Concurrency
//
// A Client is safe for concurrent use by multiple goroutines, as long as no two
// goroutines command the same plant at the same time. The Enercon control
// session is a single plant-wide resource; two overlapping sessions overwrite
// each other's keys and the outcome is undefined. Serialise per plant, or route
// all commands for a park through one goroutine.
//
// # Verifying the session
//
// The session id written to SessionRequest is the only means by which a client
// can tell its own reservation from another client's, and the Enercon session
// schema requires the client to check it — after the reservation and again after
// entering the parameters. This package does both, and additionally reads the
// written value back before submitting, so a session is only ever submitted when
// it is this client's and holds the requested command.
//
// A server that does not report the session id at all is tolerated: the id is
// never drawn as zero, so a zero read-back is treated as "cannot verify" and
// logged, not as a mismatch.
//
// # Sessions that cannot be completed
//
// If a session is reserved but the procedure cannot finish, the plant's result
// carries ErrSessionLeftOpen and the session expires on the server's own
// timeout of 60 s, during which further commands for that plant fail as
// occupied. Enercon documents no way to abort a session earlier;
// WithSessionRelease exists for installations whose documentation does.
//
// # Errors
//
// Every error wraps one of the package's sentinel errors, so a control loop can
// tell a retryable condition from a permanent one:
//
//	if errors.Is(err, energontrol.ErrSessionOccupied) {
//	    // another client holds the session; retrying later can work
//	}
//	if errors.Is(err, energontrol.ErrInsufficientRights) {
//	    // the user id lacks the rights; retrying will not help
//	}
//
// # Relationship to v1
//
// v2 is a breaking release that came out of an audit of v1. The API differences
// are listed in CHANGELOG.md; the safety-relevant ones are that commands now
// return []PlantResult instead of ([]bool, []error), that the OPC client is
// passed as an interface (&server rather than server), and that control values
// are typed constants rather than entries in a string-keyed map — in v1 a
// misspelled key silently yielded 0, which is "start".
//
// # Reference
//
// Behaviour follows the ENERCON technical data sheet "ENERCON SCADA PDI-OPC",
// section 3.4 (transferring setpoints, session schema) and section 4.1 (session
// items, control values, heating and ice detection items, timings).
//
// Which control values a plant accepts depends on its controller type; the data
// sheet marks the affected values with footnotes, reproduced on the constants.
// This package does not check controller types — a plant that does not support a
// value answers with CtrlValueRejected, surfacing as ErrCtrlValueRejected.
//
// One deviation from the data sheet is deliberate: it lists 8 as a SetRbH value
// ("switch heating on manually"), but the server rejects a bare 8. The heating is
// switched on with 10, that is 8+2, so no constant for 8 exists. Enercon notes about the
// session mechanism: "this mechanism only serves to regulate and identify access
// by trusted communication partners. It is not a security mechanism such as VPN
// or SSL encryption." Protect the network path accordingly.
package energontrol
