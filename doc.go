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
// Four rules follow from the fact that a wrong "yes" is more dangerous than a
// "no":
//
//   - A plant whose state cannot be established is never reported as being in
//     the requested state. A missing item, a faulted item, an item whose quality
//     is not good, and a plant in CtrlCommError all mean the state is unknown.
//   - A plant that cannot be commanded yields OutcomeNotPermitted with a reason,
//     not a quiet success. Plants stopped by Enercon with higher rights are the
//     usual case; a plant reporting a control state outside the documented set
//     is another, and is refused rather than guessed at.
//   - Success is reported only for plants where a value was actually written,
//     the server confirmed the write, and the session was verified. Reaching the
//     final session state is not by itself evidence that anything was written.
//   - A check that cannot be carried out is not a check that passed. If the
//     session id or a written value cannot be read back, the plant fails with
//     ErrSessionUnverified rather than being reported as commanded on the
//     strength of a log warning nobody reads.
//   - A state satisfies a request only where sending the command would be wrong
//     or impossible: a deeper stop, whose blades commanding would open back up,
//     or a stop Enercon holds the plant in. A plant idling at 60° is commanded
//     on to 90°, and a plant stopped at 60° by a different command is commanded
//     to the plain stop, rather than either being reported as already stopped.
//
// # What a plant reports in Ctrl
//
// Ctrl carries the value that was set. After SetCtrl 7 the plant reports 7 and
// holds it; no blade angle is ever reported there. The data sheet describes Ctrl
// as an operating state, which this package first read as "the plant answers
// with the blade angle a command produces, 1 or 2, and never with the command
// value" — that reading is wrong.
//
// Two things follow. CtrlValue.Reached is exact equality against the command
// value, so a plant reporting CtrlStop90 has not carried out a gradient stop at
// 90°, however its blades stand. And every state a client can have caused (0 to
// 8) is commandable, so a plant stopped for species protection can be started
// again.
//
// The blade angle a command produces is still what ranks how deep a stop is,
// which is the separate question an unforced Stop asks.
//
// The rule has a counterpart that matters just as much for a control loop:
// OutcomeFailed does not mean nothing was written. If the procedure fails after
// the session submit was written and confirmed, the server has the value and
// has committed it, and only the confirmation that the session wound down is
// missing. Such a plant carries ErrOutcomeUncertain in addition to the reason
// it failed. Read the plant's state before retrying — a Stop is protected by
// the 360 s delay before a new reservation, a Start (0 s) and a Reset are not:
//
//	if errors.Is(r.Err, energontrol.ErrOutcomeUncertain) {
//	    // the command may have taken effect; do not simply send it again
//	}
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
//     on everything a command waits for once a session exists: every wait
//     inside a session is clamped to the time left of it, so neither the
//     per-transition polling budget nor the minimum attempt count can push a
//     command past the point where the session still exists. A command that
//     runs into it reports ErrSessionExpired. WithSessionLifetime overrides the
//     value for an installation that documents a different one.
//   - The default polling budget is five seconds per state transition — not per
//     command, of which there are four. It is a wall-clock deadline and every
//     attempt is a SOAP round trip, so a slow SCADA gets fewer attempts out of
//     the same budget; a minimum of four is made regardless. Change it with
//     WithSessionPolling; values above the session lifetime are clamped to it.
//   - The wait for a free session happens before any reservation exists and is
//     therefore not bounded by the session lifetime. WithCommandTimeout bounds
//     a command as a whole, which is the figure a scheduler can reason about.
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
// # The transport
//
// The OPC server is reached through the Transport port. The transport for an
// Enercon SCADA is in the opcxmlda subpackage:
//
//	server := &gopcxmlda.Server{Url: u, LocaleID: "en-us", Timeout: 10 * time.Second}
//	client := energontrol.New(opcxmlda.New(server))
//
// The port speaks item names and unsigned integers. Nothing about SOAP, XML or
// a particular OPC client library reaches past it, and the adapter sits in a
// package of its own so that this one does not import that library at all —
// which is what makes the boundary a compile error rather than a code-review
// item. The port's predecessor was built from the library's own types, and an
// audit found it leaking.
//
// Past the port, three rules live in exactly one place: correlating a response
// with its request, following a paged browse to its end, and separating what a
// server said about the request from what it said about one item. See Transport
// for the contract an implementation has to keep.
//
// # Correlating responses
//
// Response items are matched to request items by ClientItemHandle, and by
// ItemName where no handle is returned. Position is never used: OPC XML-DA does
// not guarantee that a response lists items in request order, and a positional
// mismatch would apply a command to the wrong turbine. A response that carries
// neither is rejected with ErrUncorrelatable, and so is one whose handle and
// item name name different items — the handle is authoritative, but a
// contradiction is a server fault, and trusting it anyway is the same mistake
// positional matching was rejected for.
//
// # Errors about one item, and errors about the request
//
// A read or a write returns one result per item, and a problem with a single
// item — a fault code, unusable quality, a value of an unexpected type, an item
// the server did not answer for — is reported in that item's result. One
// unreadable plant does not blind a caller to the rest of the park, which
// matters because reading the park is the documented way to monitor it.
//
// This is the one place where an OPC XML-DA detail leaks into a safety
// property, so it is worth stating: a conformant server reports an item fault
// in the item's ResultID attribute and, since ReturnErrorText defaults to true,
// adds an Errors element carrying the localised text for it. A client library
// that reports that element as a failure of the whole request turns one bad
// item name into a failed read for a whole park — and inside a command into a
// failed batch with every reserved session left open for the server's 60 s
// timeout. The transport shipped here therefore classifies such an error as
// item-level and keeps the response, while a transport failure, a SOAP fault
// and an Errors element that no returned item accounts for all remain failures
// of the request.
//
// The boundary of the per-item handling is worth stating, because it is a
// property of the stack rather than of this package. All of the above concerns
// a response the transport could parse, in which the server reported a problem
// with a particular item. A response that cannot be parsed at all — a value
// element without the xsi:type the schema requires, say — fails as a whole:
// gopcxmlda is deliberately fail-fast about decoding, and that is the right
// call, because a reply that malformed says nothing trustworthy about any of
// its items. Salvaging the ones that happened to parse would mean deciding, on
// a guess, that the rest of the document is still to be believed.
//
// # The address space
//
// The item names follow the ENERCON technical data sheet: the park number at
// Loc/LocNo, the plants below Loc/Wec as Loc/Wec/Plant<n>, and the Ctrl and
// Reset branches below each of those. WithItemRoot points the client at a
// different root for installations that do not expose their park under "Loc";
// everything below the root, the "/" included, follows the data sheet and is
// not configurable.
//
// A plant node is only taken into the listing if this package would address it
// under the name the server itself gave it. A server that names its nodes
// Plant007 reports plant 7, but a command would go to Loc/Wec/Plant7, an item
// such a server does not have — so the node is reported in
// TurbineInfo.Unsupported instead of being listed as though it were usable.
//
// # Requesting fresh values
//
// A server may answer a read from its cache, and a control decision taken on a
// stale state is a decision taken on the wrong state. WithMaxStateAge guards
// against that from both ends.
//
// Every read carries the age as the MaxAge attribute OPC XML-DA defines for it,
// on the request's item list, which obliges the server to fetch a fresh value
// from the device rather than serve one out of its cache. That prevents a stale
// value. The item timestamp is then checked against the same limit, which
// detects one that arrives regardless — from a server that ignores the
// attribute, say. The second half is why the check is strict about an item that
// carries no timestamp at all: an age that cannot be established is not an age
// within the limit.
//
// The option is off by default, and with it off no MaxAge is sent at all. The
// specification reads a MaxAge of 0 as a demand for the most accurate data
// available, so sending it on behalf of a caller who never asked would turn
// every read into a device read.
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
//	if errors.Is(err, energontrol.ErrOutcomeUncertain) {
//	    // the command was submitted; it may have taken effect
//	}
//
// # Relationship to v1
//
// v2 is a breaking release that came out of an audit of v1. The API differences
// are listed in CHANGELOG.md; the safety-relevant ones are that commands now
// return []PlantResult instead of ([]bool, []error), that the OPC server is
// reached through the Transport port rather than used directly, and that
// control values are typed constants rather than entries in a string-keyed map
// — in v1 a misspelled key silently yielded 0, which is "start".
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
