# energontrol

Control Enercon wind turbines from Go, over OPC XML-DA, using
[gopcxmlda](https://github.com/dernate/gopcxmlda).

> **Wind turbines are critical infrastructure.** Be careful when interacting with
> them and test only in suitable test environments. No liability is assumed for
> any consequences of using this source code — **use at your own risk**.

## Install

```sh
go get github.com/dernate/energontrol/v2
```

`v2` is a breaking release. See [CHANGELOG.md](CHANGELOG.md) for the migration
from v1 and for the defects that motivated it.

## Getting started

```go
package main

import (
	"context"
	"log"
	"net/url"
	"time"

	"github.com/dernate/energontrol/v2"
	"github.com/dernate/energontrol/v2/opcxmlda"
	"github.com/dernate/gopcxmlda"
)

func main() {
	opcURL, err := url.Parse("http://your-opc-server:8080/DA")
	if err != nil {
		log.Fatal(err)
	}
	// Note the pointer: gopcxmlda.Server carries an http.Client and a mutex and
	// must not be copied.
	server := &gopcxmlda.Server{
		Url:      opcURL,
		LocaleID: "en-us",
		Timeout:  10 * time.Second, // a time.Duration — plain 10 means 10ns
	}

	// The OPC server is reached through the Transport port; opcxmlda is the
	// transport for an Enercon SCADA. See *The transport* below.
	client := energontrol.New(opcxmlda.New(server))

	const userID = 1234
	res, err := client.Stop(context.Background(), userID, true, false, 2, 4)
	if err != nil {
		log.Fatalf("the command could not be attempted: %v", err)
	}
	for _, r := range res {
		if r.InRequestedState() {
			log.Printf("plant %d: %s", r.PlantNo, r.Outcome)
		} else {
			log.Printf("plant %d is NOT stopped: %v", r.PlantNo, r.Err)
		}
	}
}
```

Each command is also available as a package-level function that builds a client
with default settings: `energontrol.Stop(ctx, transport, userID, true, false, 2, 4)`.

## Reading a result

A command returns one `PlantResult` per plant, in the order the plants were
given. Each result carries its own plant number, so results never have to be
matched by position.

| Outcome | Meaning | `Err` |
| --- | --- | --- |
| `OutcomeCommanded` | The value was written and the session confirmed it. | `nil` |
| `OutcomeAlreadyInState` | The plant was already there; nothing was written. | `nil` |
| `OutcomeNotPermitted` | The requested state cannot be reached. | why |
| `OutcomeFailed` | The command was attempted but did not complete. | why |

Use `PlantResult.InRequestedState()` rather than `Err == nil` to decide whether a
setpoint was reached — it is true for the first two outcomes only.

`OutcomeFailed` does **not** mean nothing was written. If the procedure fails
after the session submit was written and confirmed, the server has committed the
value and only the confirmation that the session wound down is missing. Such a
plant additionally carries `ErrOutcomeUncertain`; read its state before
retrying, because a `Start` (0 s re-reservation delay) and a `Reset` are not
protected by the 360 s delay that follows a `Stop`.

`Results.InRequestedState()` and `Results.Err()` answer the same questions for a
whole batch.

The reading calls follow the same shape. `PlantCtrlState`, `PlantRbhState` and
`PlantIceDetState` return one entry per requested plant, and a plant whose state
could not be established carries the reason in its `Err` — its `Ctrl` or `Status`
is then meaningless, since `Ctrl` 0 means *running*. The returned `error` is
reserved for failures of the whole request. One unreadable turbine does not blind
a caller to the rest of the park, which matters because this is the call the
monitoring loop below is built on. `PlantStates.Err()` gives the all-or-nothing
answer where that is what a caller wants.

### What is deliberately *not* a success

- A plant Enercon stopped with higher rights (`CtrlStop60Enercon`,
  `CtrlStopEnercon`) cannot be started: `OutcomeNotPermitted` with
  `ErrPlantUnderEnerconControl`.
- A plant in `CtrlCommError` has an **unknown** state. It is never reported as
  running and never as stopped: `OutcomeNotPermitted` with
  `ErrPlantCommunication`.
- A plant reporting a `Ctrl` value outside the set this package knows (0 to 8,
  121, 129, 130, 255) is likewise not commanded: `OutcomeNotPermitted` with
  `ErrPlantStateUnknown`, naming the raw value. An unknown state says nothing
  about where the blades are or who holds control, so neither the target-state
  comparison nor the rights check can be carried out. The value 137 has been
  observed on a real park.
- A state whose OPC item is missing, faulted, of bad quality, or (with
  `WithMaxStateAge`) stale is an error, not a state.

### What a plant reports in `Ctrl`

**`Ctrl` carries the value that was set.** After `SetCtrl 7` the plant reports
**7** and holds it; no blade angle is ever reported there. The data sheet
describes `Ctrl` as an operating state, which this package first read as "the
plant answers with the blade angle a command produces, 1 or 2, and never with
the command value" — that reading is wrong.

`CtrlValue.Reached(want)` is therefore exact equality against the command value.
A plant reporting `CtrlStop90` has **not** carried out a gradient stop at 90°,
however its blades stand: the blades are at the same angle, but the command is a
different one, and a forced request asks for that command.

Every state a client can have caused (0 to 8) is commandable, so a plant stopped
for species protection can be started again; only the states reserved for
Enercon (129, 130), a rejected value (121), a communication error (255) and
values outside that set are refused.

The blade angle a command produces is still what ranks how deep a stop is, which
is the separate question an unforced `Stop` asks.

An unforced `Stop` is satisfied by any state at least as stopped as the one
asked for. A plant Enercon already stopped counts, and a plant at 90° satisfies
a request for 60° — commanding it would open the blades back up. A shallower stop
never counts: a plant at 60° is commanded to 90° when `fullStop` is set. A forced
`Stop` requires exactly the requested state.

## Commands

| Method | Purpose |
| --- | --- |
| `Start(ctx, userID, plants...)` | Run the plants. |
| `Stop(ctx, userID, fullStop, forceExplicitCommand, plants...)` | Stop at 90° (`fullStop`) or 60°. With `forceExplicitCommand` the plant must reach exactly that stop state; without it, any state at least as stopped as the one asked for satisfies the request. |
| `SetCtrl(ctx, userID, value, forceExplicitCommand, plants...)` | Send any documented control value, including the gradient stops and the stops for ice detection, shadow flicker and species protection. |
| `SetRbh(ctx, userID, value, plants...)` | Send any documented heating value, including `RbhSetPresetDuration`, which the named methods do not cover. |
| `IceDetOn` / `IceDetOff` / `SetIceDet(ctx, userID, value, plants...)` | Switch the ice warning lamp. |
| `Reset(ctx, userID, plants...)` | Acknowledge faults. |
| `RbhOn` / `RbhAutoOff` / `RbhStandard` | Rotor blade heating: manually on, automatic suppressed, back to automatic. |
| `ControlAndRbh(ctx, userID, values, plants...)` | A control and a heating value in one session. |
| `Turbines(ctx)` | List the plants of the park and which functions each offers. |
| `ParkNo(ctx)` / `ParkNoMatch(ctx, parkNo, checkAvailable)` | Read or verify the park number. |
| `PlantCtrlState` / `PlantRbhState` / `PlantIceDetState` `(ctx, plants...)` | Read states without commanding anything. One entry per plant; a plant that could not be read carries the reason in its `Err`. |
| `ServerAvailable(ctx)` | Returns an error unless the server is reachable **and** running. |

```go
res, err := client.ControlAndRbh(ctx, userID, energontrol.ControlAndRbhValue{
	SetCtrlValue:         true,
	CtrlValue:            energontrol.CtrlStop60,
	SetRbhValue:          true,
	RbhValue:             energontrol.RbhSetManualOn,
	SetIceDetValue:       true,
	IceDetValue:          energontrol.IceDetLampOn,
	ForceExplicitCommand: true,
}, 2, 4)
```

Enercon transmits control value, heating value and ice warning lamp in one
session, so any combination of the three costs a single session per plant.
`ControlAndRbh` treats them as one unit per plant: if any requested part cannot
be carried out, that plant yields `OutcomeNotPermitted` and nothing is written
for it.

## Values

Control and heating values are typed constants, so a typo is a compile error.
The set is the one Enercon documents for `SetCtrl` and `SetRbH`.

| Constant | Value | Meaning |
| --- | --- | --- |
| `CtrlStart` | 0 | Start the plant |
| `CtrlStop60` | 1 | Stop at 60° blade angle |
| `CtrlStop90` | 2 | Stop at 90° blade angle |
| `CtrlGradientStop60` | 3 | Gradient stop at 60° |
| `CtrlGradientStop90` | 4 | Gradient stop at 90° |
| `CtrlStopIceDetection` | 5 | Stop for ice detection |
| `CtrlStopShadowFlicker` | 6 | Stop for shadow flicker |
| `CtrlStopSpeciesProtection60` | 7 | Stop for species protection at 60° |
| `CtrlStopSpeciesProtection90` | 8 | Stop for species protection at 90° |

`Start` and `Stop` cover 0, 1 and 2; use `SetCtrl(ctx, userID, value, force, plants...)`
for the rest.

| Constant | Value | Meaning |
| --- | --- | --- |
| `RbhSetStandard` | 0 | Neither suppress the automatic system nor heat manually |
| `RbhSetAutoOff` | 2 | Suppress automatic operation |
| `RbhSetManualOn` | 10 | Suppress automatic operation **and** heat manually |
| `RbhSetPresetDuration` | 128 | Heat for the preset duration (one-shot; always sent) |

`RbhOn` sends 10, `RbhAutoOff` sends 2, `RbhStandard` sends 0; use
`SetRbh(ctx, userID, value, plants...)` for 128.

The data sheet also lists 8 ("switch heating on manually", leaving automatic
operation enabled), but the server rejects a bare 8 — the heating is switched on
with 10, that is 8+2. There is no constant for 8.

`RbhOn` asks for *"the heating runs"*, not for a particular status bit: a plant
already heating under automatic control yields `OutcomeAlreadyInState`.

| Constant | Value | Meaning |
| --- | --- | --- |
| `IceDetLampOff` | 0 | Switch the ice warning lamp off |
| `IceDetLampOn` | 8 | Switch the ice warning lamp on |

The `IceDet` status read from a plant says *which system* detected ice
(`IceDetPowerCurve`, `IceDetPreventive`, `IceDetExternalSensor`,
`IceDetExternalSCADA`, `IceDetParkDetection`), decoded with
`IceDetStatusStrings`. Switching the lamp on from SCADA is what the plant reports
as `IceDetExternalSCADA`, so that bit is the read-back of the command.

Which control values a plant accepts depends on its controller type — the data
sheet's footnotes are reproduced on the constants (`CtrlStop60`,
`CtrlGradientStop60`, `CtrlStopSpeciesProtection60`, `CtrlStop60Enercon` are not
available on EP5-CS-03, the last one except on the E-160 EP5 E3). This package
does not check controller types: a plant that does not support a value answers
with `Ctrl` 121, which surfaces as `ErrCtrlValueRejected`. Knowing which plants
support which command is the caller's responsibility.

A plant only ever *reports* `Ctrl` values 0, 1, 2, 121 (value rejected), 129,
130 or 255 — never a command value. A forced request for, say, a gradient stop
at 60° is therefore satisfied by a plant reporting `CtrlStop60`. For the two
commands whose name does not state a blade angle (ice detection, shadow
flicker) Enercon documents no resulting state, so nothing counts as
"already in state" and the command is always sent.

Values reserved for the system (`CtrlValueRejected`, `CtrlStop60Enercon`,
`CtrlStopEnercon`, `CtrlCommError`) exist for reading a state but are rejected
with `ErrInvalidValue` if a caller tries to write one.

The heating **status word** read from a plant is a bit field, decoded with
`RbhStatusStrings` / `RbhStatusString`. Its constants (`RbhInstalled`,
`RbhManualOnSCADA`, `RbhFault`, …) are separate from the `RbhSet…` command
values. `RbhIsInstalled(status)` answers whether a plant has heating at all
(bit 15), which also covers the whole-word "not installed" value.

## Layout

```
energontrol/           the library: client, protocol, value types, errors
  opcxmlda/            the OPC XML-DA transport, on top of gopcxmlda
  cmd/scadaprobe/      a diagnostic client to point at a real SCADA
  test/                tests that command real turbines (build tag opc_integration)
```

`energontrol` does not import `gopcxmlda`; only `opcxmlda` does. Every test file
is named after the production file it covers.

## Checking a real SCADA

`cmd/scadaprobe` is an interactive diagnostic and control client. It takes its
configuration from the environment — the same variables the live test suite
uses, so an existing `.env` works as is:

```sh
OPC_URL=http://scada.example:8080/DA \
USERID=1234 PARKNO=4242 ENERGONTROL_TEST_PLANTS=2,4 \
    go run ./cmd/scadaprobe
```

It starts by reading: the park listing, which functions each plant offers, and
the state of the selected plants. Then it verifies the park number against the
server and opens a menu — read the states, diagnose the server, change the plant
selection, toggle `forceExplicitCommand`, or one of the control operations.

**Every command the library can send has a menu entry**, grouped and listed the
way the data sheet is:

| Group | Entries |
| --- | --- |
| Control values (`Ctrl/SetCtrl`) | all nine documented values, in data-sheet order: start (0), stop at 60° (1) and 90° (2), the gradient stops (3, 4), the stops for ice detection (5) and shadow flicker (6), and the species-protection stops at 60° (7) and 90° (8) |
| Rotor blade heating (`Ctrl/SetRbh`) | back to automatic (0), suppress automatic (2), heating on (10), preset duration (128) |
| Ice warning lamp (`Ctrl/SetIceDet`) | off (0), on (8) |
| Fault acknowledgement (`Reset/SetReset`) | reset |
| Several parameters in one session | a control value, a heating value and the lamp together; each part optional |

Every label names the value it writes, so a selection can be checked against the
data sheet without reading the source. The values above 8 are deliberately
absent: those are states a plant reports, not commands a client may send.

`forceExplicitCommand` is a menu toggle rather than a question per command. Off —
the default — a stop request is satisfied by any state at least as stopped as
the one asked for; on, the plant must reach exactly the state the command
produces, and a plant Enercon has stopped is reported as not permitted. The
confirmation screen shows which way it is set for the commands it affects.

**Nothing is written until an operation is confirmed.** Choosing one shows what
would be sent, to which plants, as which user, the state those plants are in
right now, and what the consequences are — and then asks the operator to type
that operation's *name* back. A plain `yes` does not send it; neither does the
menu key. Afterwards the report says what was written, what each plant answered,
and what to do about a failure (which errors must not be retried, which mean the
command may have taken effect anyway).

A command is offered at all only when three things hold together: the server
confirmed `PARKNO`, `ENERGONTROL_TEST_PLANTS` names the plants explicitly, and
`USERID` is set. A command never defaults to the whole park.

The diagnosis answers the questions this library's contracts rest on: whether
the server pages its browse answers, whether it fills item timestamps (so
`WithMaxStateAge` can detect a stale value as well as demand a fresh one),
whether it reports a problem with one item *per item* rather than failing the
whole request, and how long a round trip takes — with a recommended
`WithSessionPolling` budget from the p95, since that budget is a wall-clock
deadline that has to hold several round trips.

Everything goes into `scadaprobe.log` as JSON lines: every menu choice, every
offer and confirmation or abort, every OPC request with the items and values it
carried and what came back, the library's own warnings, and the outcome per
plant. The log is appended to and never truncated. The user id is the one thing
kept out of it — including inside the `SessionRequest` write, where the Enercon
schema puts it between the session id and the private key; it is redacted in
place so the array still reads as the data sheet defines it.

### What it writes

Reading writes nothing — `GetStatus`, `Browse` and `Read` only. A control
operation writes exactly the three items the Enercon session schema defines,
per plant, and nothing else:

| Step | Item | Value |
| --- | --- | --- |
| reserve | `.../{Ctrl,Reset}/SessionRequest` | session id, user id, private key |
| enter | `.../Ctrl/SetCtrl`, `SetRbh`, `SetIceDet`, or `.../Reset/SetReset` | the value, private key, public key |
| submit | `.../{Ctrl,Reset}/SessionSubmit` | private key, public key |

Everything else a command does is a read: the session state four times, the
session id twice, the public key, the value read-back, and the remaining session
timeout if a session had to be left open.

## The transport

The OPC server is reached through the `Transport` port. The transport for an
Enercon SCADA is `opcxmlda.New(server)`, from the subpackage of that name, which
is what nearly every caller wants.

The port speaks item names and unsigned integers — nothing about SOAP, XML or a
particular OPC client library reaches past it. The adapter lives in a package of
its own so that `energontrol` does not import that library at all, which is what
makes the boundary a compile error rather than a code-review item: an interface
cannot leak types it cannot name, and the port's predecessor was built from
those types and did leak.

Past the port, three rules live in exactly one place. An implementation owns
them:

- **Correlating a response with its request.** Every result carries the
  requested name it belongs to, never a name derived from an item's position.
- **Following a paged browse to its end.** A partial listing is never returned
  without an error, because a plant missing from a park listing is never
  commanded and never monitored.
- **Separating an error about the request from an error about one item.** An
  item-level fault belongs in `ItemResult.ResultID` and must not fail the call.

The third one is where an OPC XML-DA detail meets a safety property. A
conformant server reports an item fault in that item's `ResultID` and, since
`ReturnErrorText` defaults to true, adds an `<Errors>` element with the
localised text for it. A client library that reports *that* as a failed request
turns one bad item name into a failed read for a whole park — and inside a
command into a failed batch with every reserved session left open for the
server's 60 s timeout. The shipped transport classifies it as item-level and
keeps the response. A transport failure, a SOAP fault, and an `<Errors>` element
that no returned item accounts for all stay failures of the request.

The boundary is worth knowing, because it belongs to the stack rather than to
this package. All of that concerns a response the transport could parse, in
which the server reported a problem with a particular item. A response that
cannot be parsed at all — a `<Value>` without the `xsi:type` the schema
requires, say — fails as a whole: `gopcxmlda` is deliberately fail-fast about
decoding, and rightly so, since a reply that malformed says nothing trustworthy
about any of its items.

## Errors

Every error wraps a sentinel, so failures can be classified:

```go
switch {
case errors.Is(err, energontrol.ErrSessionOccupied):
	// another client holds the session — retrying later can work
case errors.Is(err, energontrol.ErrInsufficientRights):
	// the user id lacks the rights — retrying will not help
case errors.Is(err, energontrol.ErrServerNotRunning):
	// the SCADA answered but is not running
case errors.Is(err, energontrol.ErrOutcomeUncertain):
	// the command was submitted; it may have taken effect
}
```

`*SessionStateError` (via `errors.As`) additionally reports the plant, the
expected and the observed session state.

## Options

```go
client := energontrol.New(opcxmlda.New(server),
	energontrol.WithLogger(slog.Default()),
	energontrol.WithSessionPolling(100*time.Millisecond, 5*time.Second),
	energontrol.WithCommandTimeout(30*time.Second),
	energontrol.WithMaxStateAge(30*time.Second),
	energontrol.WithSessionRelease(releaseSession),
)
```

`New` panics on a nil transport or an unusable option value — both are
programming errors that should surface on the first run. Where the values come
from a configuration file, use `NewWithOptions`, which returns them as an error
wrapping `ErrInvalidOption`. Options are order-independent: they record what was
asked for, and the clamping against the session lifetime happens once, after all
of them have been applied.

| Option | Effect |
| --- | --- |
| `WithLogger` | Attach an `*slog.Logger`. By default the package logs nothing. |
| `WithSessionPolling` | How often and how long to wait for **one** session state transition — a command waits for four. Default 100 ms over 5 s. The timeout is a wall-clock deadline and every attempt is a SOAP round trip, so a slow SCADA gets fewer attempts from the same budget; a minimum of four is made regardless. Values above the session lifetime are clamped to it. |
| `WithMaxStateAge` | Demand values no older than this, and reject what arrives older with `ErrStaleValue`. Every read carries the age as OPC XML-DA's `MaxAge`, which obliges the server to read the device instead of its cache (*prevention*); the item timestamp is then checked against the same limit (*detection*, for a server that ignores the attribute). **Off by default** — the detecting half depends on the server returning item timestamps and on both clocks agreeing. The check is strict: an item with no timestamp is rejected with `ErrNoItemTime`, because an age that cannot be established is not an age within the limit. At most ~24 days (`MaxAge` is an `xs:int` in milliseconds). |
| `WithSessionLifetime` | Override the 60 s session lifetime that bounds every wait inside a command. Only for an installation whose documentation states a different value. |
| `WithLenientWriteConfirmation` | Accept a written item the server did not confirm. Gives up the only evidence a `Reset` has; the value read-back remains. |
| `WithLenientSessionVerification` | Accept a session whose id, or whose written value, could not be read back. Gives up **both** checks the session schema requires; attach a logger, or the warnings go nowhere. |
| `WithLenientVerification` | Both of the above. The widest tolerance the package offers; see *Session verification*. |
| `WithCommandTimeout` | Bound a whole command, including the wait for a free session before any reservation exists — which the session lifetime does not cover. Off by default. |
| `WithItemRoot` | Point the client at a different root of the address space. Default `"Loc"`; everything below it follows the data sheet. |
| `WithSessionRelease` | Close sessions that were reserved but could not be completed. |

### Sessions that cannot be completed

If a session is reserved and the procedure then fails, the plant's result carries
`ErrSessionLeftOpen`; the session stays reserved until the server's own 60 s
timeout, and commands for that plant fail as occupied in the meantime. The log
line reports the remaining time, read from the `SessionTimeOut` item.

No default release is shipped, because the Enercon technical data sheet
describes no way to abort a session — a session ends by running into its
timeout. `WithSessionRelease` exists for installations whose documentation does
define an abort telegram.

## Timing

The Enercon technical data sheet gives these timings for control access to a
single plant. They constrain how commands may be scheduled:

| | |
| --- | --- |
| Session timeout (enforced: bounds every wait, then `ErrSessionExpired`) | 60 s |
| Default polling budget **per state transition** (`WithSessionPolling`) | 5 s |
| Extension timeout after setting a value | 60 s |
| Delay before a new reservation **after a stop** | 360 s |
| Delay before a new reservation after a start | 0 s |
| Error timeout after three wrong entries | 180 s |
| Wrong user id | 300 s |

The 360 s figure matters most: a plant that was just stopped answers
"occupied" for six minutes. A control loop must not re-send a stop faster than
that.

This package never retries a write, because three rejected keys lock a plant out
for three minutes. Errors wrapping `ErrInsufficientRights` or
`ErrIncorrectUserID` are permanent — do not retry them.

`OutcomeCommanded` means the session completed, **not** that the turbine has
reached the state. Enercon: *"the status resulting from a start and stop of the
wind turbine must be monitored. Monitoring the status is the responsibility of
the customer or the operator."* Poll `PlantCtrlState` until the plant reports the
expected state.

## Session verification

The session id written to `SessionRequest` is the only way a client can tell its
own reservation from another client's, and the Enercon session schema requires
the client to check it — after the reservation and again after entering the
parameters. This package does both, and reads the written value back before
submitting, so a session is submitted only when it is this client's *and* holds
the requested command. A mismatch yields `ErrSessionIDMismatch`; a value that did
not arrive yields `ErrParameterNotAccepted`.

**A check that could not be carried out is not a check that passed.** If the
session id or a written value cannot be read back — a faulted item, a missing
item, bad quality, a stale value — the plant fails with `ErrSessionUnverified`
and nothing is submitted. The same holds for the write itself: an item missing
from the `WriteResponse` is unconfirmed, and an unconfirmed write is not a write.
That is the only evidence a `Reset` has, since the data sheet defines no
readable parameter for it.

Exactly one case stays tolerated: a server that does not report the session id at
all. The id is never drawn as zero, so a zero read-back is logged as "cannot
verify" rather than treated as a mismatch.

`WithLenientSessionVerification` restores the tolerant behaviour for a server
that genuinely cannot answer these reads, and `WithLenientWriteConfirmation`
covers a server that does not echo written items;
`WithLenientVerification` enables both. They give up real evidence — turn one on
only for an installation where the strict behaviour has been shown to reject
commands the server did carry out, and prefer whichever of the two narrower
options is actually needed.

One limit of the value read-back is worth knowing: comparing the value the
session holds against the value written establishes nothing when that value is
zero, because `CtrlStart`, `RbhSetStandard` and `IceDetLampOff` all write 0 and
an untouched item reads back the same. Where the server also reports the private
key back, that key is checked instead — it is never drawn as zero. Where it does
not, the write confirmation is the remaining evidence and the gap is logged.

> The session id space holds 19 values, so two clients competing for one plant
> draw the same id about once in nineteen attempts, and the ownership check then
> confirms the other client's reservation. The check narrows the race; it does
> not close it. Commands for one park belong in one process.

### Transport security

> Enercon on the session mechanism: *"this mechanism only serves to regulate and
> identify access by trusted communication partners. It is not a security
> mechanism such as VPN or SSL encryption."*

The session id and the keys are not credentials, and OPC XML-DA over plain HTTP
carries the user id in clear text. Anyone who can reach the SCADA endpoint can
command the park. Put the endpoint behind a VPN or TLS and restrict who can route
to it — this package cannot make that decision for you, and the session
mechanism is not a substitute for it.

The `ServerState` that every read and write response carries **is** checked, not
just the one `GetStatus` reports before a command: a server that degrades to
`failed` or `suspended` mid-session stops receiving commands, and its values stop
being used as process values.

## Data freshness

A server may answer a read from its cache, and a control decision taken on a
stale state is a decision taken on the wrong state. `WithMaxStateAge` closes
that from both ends:

- **Prevention.** Every read carries the age as the `MaxAge` attribute on the
  request's item list, which obliges the server to fetch a fresh value from the
  device.
- **Detection.** The item timestamp is checked against the same limit, so a
  server that ignores `MaxAge` and answers from its cache anyway is still
  caught.

With the option off, no `MaxAge` is sent at all — the specification reads a
`MaxAge` of 0 as a demand for the most accurate data available, so sending it
for a caller who never asked would turn every read into a device read.

## The address space

Item names follow the technical data sheet: the park number at `Loc/LocNo`, the
plants below `Loc/Wec` as `Loc/Wec/Plant<n>`, and the `Ctrl` and `Reset`
branches below each. `WithItemRoot` moves the root for an installation that does
not expose its park under `Loc` — everything below the root, including the `/`,
follows the data sheet and is not configurable. Without that option such an
installation answers *nothing*: every item comes back missing, for every plant,
with no hint as to why.

A plant node enters the listing only if this package would address it under the
name the server itself gave it. A server naming its nodes `Plant007` reports
plant 7, but a command would go to `Loc/Wec/Plant7`, which that server does not
have — so the node lands in `TurbineInfo.Unsupported` rather than being listed
as usable.

## Plant numbers

A plant number is a `uint8` throughout, deliberately. An Enercon park holds 20 to
30 turbines, so 255 is an order of magnitude of headroom, and the narrow type
keeps the number usable as a map key and struct field everywhere without
conversions.

The limit is not allowed to hide a turbine, though: a plant node under `Loc/Wec`
that this package cannot address — a number above 255, or a name outside the
`Loc/Wec/Plant<n>` scheme — is reported in `TurbineInfo.Unsupported` and logged,
never dropped. A plant missing from a park listing is never commanded and never
monitored, so that has to be visible.

## Concurrency

A `Client` is safe for concurrent use. Commands are **serialised per plant**
inside the `Client`: the control session is a single plant-wide resource, and
overlapping sessions overwrite each other's keys. A command for a plant another
goroutine is currently commanding waits for it, in plant-number order, so
overlapping plant sets cannot deadlock. Read-only calls are never blocked.

The serialisation is per `Client`, which covers callers that share one — the
intended use, since a `Client` owns one park. It cannot cover a second process;
there the session id check is the only defence, and its reach is limited (see
*Session verification*). Route commands for one park through one process.

## Tests

```sh
go test ./...          # unit and protocol tests, no network, no turbines
go test -race ./...
```

Every test file is named after the production file it covers, so `session.go` is
tested by `session_test.go` and nothing else. Three files carry more than that
name suggests, and are worth knowing about:

`transport_test.go` holds the in-memory `Transport` the protocol tests run
against. It models the session state machine and can inject the failures a real
SCADA produces: faulted items, bad quality, missing items, reordered responses,
occupied and stuck sessions, sessions in loop mode, a session reserved by
another client, a value that does not read back, a write the server does not
confirm, a server that degrades mid-command, and a transport that breaks in any
single phase of the procedure.

`opcxmlda/transport_test.go` drives the real `gopcxmlda` adapter against an
HTTP test server with real SOAP responses — the layer the in-memory transport
sits above and therefore cannot reach: response correlation (including a handle
that contradicts its item name), value decoding for every `xsi:type` a server
may choose, browse paging, the distinction between an error about the request
and an error about one item, and the payloads that go out. The array element
width is pinned there in particular, where a `[]uint64` would emit
`ArrayOfUnsignedLong` instead of the long word the items are typed as.

`doc_test.go` is the one file that sees the package from outside
(`package energontrol_test`). It does not repeat the protocol tests; it asks
whether the exported surface is enough to use the package — whether a caller can
implement `Transport` without reaching for an unexported type, classify every
failure through the sentinels, and read a result without guessing. The runnable
example lives there too.

The value sets, item names, array layouts, session states and timing
constraints are reconciled against the ENERCON SCADA PDI-OPC technical data
sheet by the `TestSpec…` tests, which sit in the file of whichever production
code they pin down.

Tests that command **real turbines** live in a package of their own, `test/`,
behind a build tag *and* an environment guard, and verify the park number before
sending anything. Only the exported API is reachable from there, so the suite
doubles as a standing check that what the library exports is enough to run a
park with:

```sh
export OPC_URL=http://scada.example:8080/DA
export USERID=1234
export PARKNO=5678                    # the park these tests may command
export ENERGONTROL_TEST_PLANTS=2,4    # the plants these tests may command
export ENERGONTROL_ALLOW_LIVE_CONTROL=yes-i-know
go test -tags opc_integration -run TestLive -v ./...
```
