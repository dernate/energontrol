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
//   - Success is reported only for plants where a value was actually written,
//     the server confirmed the write, and the session was verified. Reaching the
//     final session state is not by itself evidence that anything was written.
//   - A check that cannot be carried out is not a check that passed. If the
//     session id or a written value cannot be read back, the plant fails with
//     ErrSessionUnverified rather than being reported as commanded on the
//     strength of a log warning nobody reads.
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
//     this; there is no documented way to abort one earlier. It is the ceiling
//     on everything a command waits for: every wait inside a session is clamped
//     to the time left of it, so the per-transition polling budget cannot add
//     up past the point where the session still exists. A command that runs
//     into it reports ErrSessionExpired. WithSessionLifetime overrides the
//     value for an installation that documents a different one.
//   - The default polling budget is one second per state transition — not per
//     command, of which there are four. Raise it with WithSessionPolling if a
//     SCADA needs longer; values above the session lifetime are clamped to it.
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
// # Plant numbers
//
// A plant number is a uint8 throughout this package, and that is a deliberate
// choice rather than a narrow type nobody thought about. An Enercon wind park
// holds on the order of 20 to 30 turbines; 255 leaves an order of magnitude of
// headroom over the largest park this package is meant to serve. The narrow
// type keeps the plant number small enough to be a map key and a struct field
// everywhere without conversions, and makes an accidental negative or absurd
// value impossible to express.
//
// The limit is nevertheless never allowed to hide a turbine. If a browse of
// Loc/Wec reports a plant node this package cannot address — a number above
// 255, or a name that does not follow the Loc/Wec/Plant<n> scheme — it is
// reported in TurbineInfo.Unsupported and logged, not dropped. A plant missing
// from a park listing is never commanded and never monitored, so the caller has
// to be able to see that it happened. If a park ever does exceed the range, that
// shows up as an entry in Unsupported rather than as a turbine that quietly does
// not exist.
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
// A Client is safe for concurrent use by multiple goroutines. Commands are
// serialised per plant inside the Client: the Enercon control session is a
// single plant-wide resource, and two overlapping sessions overwrite each
// other's keys. A command for a plant another goroutine is currently commanding
// waits for it, in plant-number order, so overlapping plant sets cannot
// deadlock. Read-only calls are never blocked.
//
// The serialisation is per Client and therefore covers callers that share one,
// which is how a Client is meant to be used — it owns one park. It cannot cover
// a second process. There the session id check is the only defence, and its
// reach is limited: the id is drawn from 1 to 19, so two clients competing for
// one plant draw the same id about once in nineteen attempts. Commands for one
// park belong in one process.
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
// Exactly one outcome is tolerated: a server that does not report the session id
// at all. The id is never drawn as zero, so a zero read-back means "the server
// does not expose it" and is logged rather than treated as a mismatch.
//
// Everything else that prevents a check from being carried out — a faulted item,
// a missing item, unusable quality, a stale value, an unexpected type — fails
// the plant with ErrSessionUnverified. An unverifiable session is not a verified
// one, and a package whose default logger discards everything cannot report such
// a gap through a warning. WithLenientVerification restores the tolerant
// behaviour for a server that genuinely cannot answer these reads.
//
// The same rule covers the write itself: an item missing from the WriteResponse
// is unconfirmed, and an unconfirmed write is not a write. This matters most for
// Reset, which the data sheet gives no readable parameter for, so the write
// confirmation is its only evidence.
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
//	if errors.Is(err, energontrol.ErrSessionUnverified) {
//	    // the command was not sent because the session could not be checked
//	}
//	if errors.Is(err, energontrol.ErrSessionExpired) {
//	    // the session ran out of its 60 s; a fresh attempt can work
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
// ("switch heating on manually"), but the server rejects a bare 8. The heating
// is switched on with 10, that is 8+2, so no constant for 8 exists.
//
// # Transport security
//
// Enercon notes about the session mechanism: "this mechanism only serves to
// regulate and identify access by trusted communication partners. It is not a
// security mechanism such as VPN or SSL encryption."
//
// The session id and the keys are therefore not credentials, and OPC XML-DA over
// plain HTTP carries the user id in clear text. Anyone who can reach the SCADA
// endpoint can command the park. Put the endpoint behind a VPN or TLS and
// restrict who can route to it; this package cannot make that decision for a
// caller, and does not pretend the session mechanism substitutes for it.
package energontrol
