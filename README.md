# Control Enercon Wind Turbines

This package uses the [gopcxmlda](https://github.com/dernate/gopcxmlda) package to
interact via OPC XML DA with an Enercon SCADA PC and exposes functions to control
Enercon Wind Turbines.

`v2` is a breaking release. See [CHANGELOG.md](CHANGELOG.md) for the migration from
v1 and for the defects that motivated it.

## Install

```sh
go get github.com/dernate/energontrol/v2
```

## Functions
The following Functions are implemented:
- [x] Start
- [x] Stop
- [x] SetCtrl
- [x] Reset
- [x] RbhOn
- [x] RbhAutoOff
- [x] RbhStandard
- [x] SetRbh
- [x] IceDetOn
- [x] IceDetOff
- [x] SetIceDet
- [x] ControlAndRbh
- [x] Turbines
- [x] ParkNo
- [x] ParkNoMatch
- [x] PlantCtrlState
- [x] PlantRbhState
- [x] PlantIceDetState
- [x] ServerAvailable

Roadmap:
- [ ] No new features are planned at the moment. Feel free to open an issue if you have a feature request or create a pull request if you want to contribute.

### Basic Procedure
Basic usage is as follows:

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
	_url, _ := url.Parse("http://your-opc-server:port/DA")
	// Note the pointer: gopcxmlda.Server carries an http.Client and a mutex and
	// must not be copied. The timeout is a time.Duration — plain 10 means 10ns.
	server := &gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10 * time.Second,
	}
	client := energontrol.New(opcxmlda.New(server))

	UserId := uint64(1234)
	PlantNo := []uint8{2, 4}
	res, err := client.Stop(context.Background(), UserId, true, false, PlantNo...)
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

Every command is also available as a package-level function that builds a client
with default settings: `energontrol.Stop(ctx, transport, UserId, true, false, PlantNo...)`.

### Start(Context, UserId, PlantNo...)
Start one or more turbines.

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
started, err := client.Start(context.Background(), UserId, PlantNo...)
```

### Stop(Context, UserId, FullStop, ForceExplicitCommand, PlantNo...)
Stop one or more turbines. FullStop can be false for 60° stop or true for 90° Stop.
If ForceExplicitCommand is false, then a stop deeper than the requested one is
accepted, as is a stop Enercon holds the plant in.
(For example: Requested status Stop60, but the plant is already at Stop90, then it is
not stopped at Stop60, but Stop90 is accepted. The other way round it *is* stopped:
a plant at Stop60 is taken to Stop90. A plant stopped at 60° by a gradient or
species-protection stop is taken to the plain Stop60 — same blade angle, different
operating mode.)
If ForceExplicitCommand is true, then the plant is stopped at the requested status,
even if the plant is in a similar status.

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
stopped, err := client.Stop(context.Background(), UserId, true, true, PlantNo...)
```

### SetCtrl(Context, UserId, Value, ForceExplicitCommand, PlantNo...)
Send any documented control value, including the gradient stops and the stops for
ice detection, shadow flicker and species protection. Start and Stop cover the
values 0, 1 and 2; this is the way to reach the rest.

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
stopped, err := client.SetCtrl(context.Background(), UserId,
	energontrol.CtrlStopSpeciesProtection60, false, PlantNo...)
```

### Reset(Context, UserId, PlantNo...)
Reset one or more turbines.

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
resetted, err := client.Reset(context.Background(), UserId, PlantNo...)
```

### RbhOn(Context, UserId, PlantNo...)
Set the Rotor Blade Heating to "Manual On". It asks for *"the heating runs"*, not
for a particular status bit: a plant already heating under automatic control yields
`OutcomeAlreadyInState`.

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
rbhOn, err := client.RbhOn(context.Background(), UserId, PlantNo...)
```

### RbhAutoOff(Context, UserId, PlantNo...)
Set the Rotor Blade Heating to "Auto Off" (Supress Automatic -> Off).

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
rbhAutoOff, err := client.RbhAutoOff(context.Background(), UserId, PlantNo...)
```

### RbhStandard(Context, UserId, PlantNo...)
Set the Rotor Blade Heating to "Standard". If automatic heating is allowed, the
automatic takes control.

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
rbhStandard, err := client.RbhStandard(context.Background(), UserId, PlantNo...)
```

### SetRbh(Context, UserId, Value, PlantNo...)
Send any documented heating value, including `RbhSetPresetDuration`, which the named
methods above do not cover.

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
heated, err := client.SetRbh(context.Background(), UserId,
	energontrol.RbhSetPresetDuration, PlantNo...)
```

### IceDetOn / IceDetOff / SetIceDet(Context, UserId, Value, PlantNo...)
Switch the ice warning lamp on or off.

Example:
```go
UserId := uint64(1234)
PlantNo := []uint8{2, 4}
lampOn, err := client.IceDetOn(context.Background(), UserId, PlantNo...)
```

### ControlAndRbh(Context, UserId, Values, PlantNo...)
Set Ctrl, Rbh and IceDet value for one plant at once. Enercon transmits all three in
one session, so any combination costs a single session per plant. If any requested
part cannot be carried out, that plant yields `OutcomeNotPermitted` and nothing is
written for it.

Example:
```go
UserId := uint64(1234)
Values := energontrol.ControlAndRbhValue{
	SetCtrlValue:         true,
	CtrlValue:            energontrol.CtrlStop60,
	SetRbhValue:          true,
	RbhValue:             energontrol.RbhSetManualOn,
	SetIceDetValue:       true,
	IceDetValue:          energontrol.IceDetLampOn,
	ForceExplicitCommand: false,
}
PlantNo := []uint8{2, 4}
controlled, err := client.ControlAndRbh(context.Background(), UserId, Values, PlantNo...)
```

### Turbines(Context)
Get a list of turbines and which controls are available for each turbine. Plants the
package cannot address are reported in `TurbineInfo.Unsupported` rather than dropped.

Example:
```go
turbines, err := client.Turbines(context.Background())
```

### ParkNo(Context) / ParkNoMatch(Context, ParkNo, checkAvailable)
Read the Park Number from the Server, or compare it with the provided ParkNo. If
checkAvailable is true, the function also checks if the Server is running.

Example:
```go
match, err := client.ParkNoMatch(context.Background(), 1234, false)
```

### PlantCtrlState / PlantRbhState / PlantIceDetState(Context, PlantNo...)
Read states without commanding anything. One entry per requested plant; a plant whose
state could not be established carries the reason in its `Err`, and its `Ctrl` or
`Status` is then meaningless — `Ctrl` 0 means *running*. The returned `error` is
reserved for failures of the whole request, so one unreadable turbine does not blind
a caller to the rest of the park. `PlantStates.Err()` gives the all-or-nothing answer
where that is what a caller wants.

Example:
```go
PlantNo := []uint8{2, 4}
states, err := client.PlantCtrlState(context.Background(), PlantNo...)
```

### ServerAvailable(Context)
Returns an error unless the server is reachable **and** running.

Example:
```go
err := client.ServerAvailable(context.Background())
```

## Results

A command returns one `PlantResult` per plant, in the order the plants were given.
Each result carries its own plant number, so results never have to be matched by
position.

| Outcome | Meaning | `Err` |
| --- | --- | --- |
| `OutcomeCommanded` | The value was written and the session confirmed it. | `nil` |
| `OutcomeAlreadyInState` | The plant was already there; nothing was written. | `nil` |
| `OutcomeNotPermitted` | The requested state cannot be reached. | why |
| `OutcomeFailed` | The command was attempted but did not complete. | why |

Use `PlantResult.InRequestedState()` rather than `Err == nil` to decide whether a
setpoint was reached — it is true for the first two outcomes only.
`Results.InRequestedState()` and `Results.Err()` answer the same for a whole batch.

`OutcomeFailed` does **not** mean nothing was written. If the procedure fails after
the session submit was confirmed, the server has committed the value and only the
confirmation that the session wound down is missing. Such a plant additionally
carries `ErrOutcomeUncertain`; read its state before retrying, because a `Start` and
a `Reset` are not protected by the 360 s re-reservation delay that follows a `Stop`.

These are deliberately **not** reported as a success:

- A plant Enercon stopped with higher rights (`CtrlStop60Enercon`, `CtrlStopEnercon`)
  cannot be started: `OutcomeNotPermitted` with `ErrPlantUnderEnerconControl`.
- A plant in `CtrlCommError` has an unknown state. It is never reported as running
  and never as stopped: `OutcomeNotPermitted` with `ErrPlantCommunication`.
- A plant reporting a `Ctrl` value outside the set this package knows (0 to 8, 121,
  129, 130, 255): `OutcomeNotPermitted` with `ErrPlantStateUnknown`, naming the raw
  value. The value 137 has been observed on a real park.
- A state whose OPC item is missing, faulted or of bad quality is an error, not a
  state.

## Values

Control and heating values are typed constants, so a typo is a compile error. The set
is the one Enercon documents for `SetCtrl` and `SetRbH`.

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

| Constant | Value | Meaning |
| --- | --- | --- |
| `RbhSetStandard` | 0 | Neither suppress the automatic system nor heat manually |
| `RbhSetAutoOff` | 2 | Suppress automatic operation |
| `RbhSetManualOn` | 10 | Suppress automatic operation **and** heat manually |
| `RbhSetPresetDuration` | 128 | Heat for the preset duration (one-shot; always sent) |

The data sheet also lists 8 ("switch heating on manually", leaving automatic operation
enabled), but the server rejects a bare 8 — the heating is switched on with 10, that
is 8+2. There is no constant for 8.

| Constant | Value | Meaning |
| --- | --- | --- |
| `IceDetLampOff` | 0 | Switch the ice warning lamp off |
| `IceDetLampOn` | 8 | Switch the ice warning lamp on |

The `IceDet` status read from a plant says *which system* detected ice
(`IceDetPowerCurve`, `IceDetPreventive`, `IceDetExternalSensor`,
`IceDetExternalSCADA`, `IceDetParkDetection`), decoded with `IceDetStatusStrings`.
Switching the lamp on from SCADA is what the plant reports as `IceDetExternalSCADA`,
so that bit is the read-back of the command.

The heating **status word** read from a plant is a bit field, decoded with
`RbhStatusStrings` / `RbhStatusString`. Its constants (`RbhInstalled`,
`RbhManualOnSCADA`, `RbhFault`, …) are separate from the `RbhSet…` command values.
`RbhIsInstalled(status)` answers whether a plant has heating at all (bit 15), which
also covers the whole-word "not installed" value.

Which control values a plant accepts depends on its controller type — the data sheet's
footnotes are reproduced on the constants (`CtrlStop60`, `CtrlGradientStop60`,
`CtrlStopSpeciesProtection60`, `CtrlStop60Enercon` are not available on EP5-CS-03, the
last one except on the E-160 EP5 E3). This package does not check controller types: a
plant that does not support a value answers with `Ctrl` 121, which surfaces as
`ErrCtrlValueRejected`. Knowing which plants support which command is the caller's
responsibility.

`Ctrl` reports the value that was set: after `SetCtrl 7` a plant reports 7 and holds
it, not the 60° blade angle that stop produces. `CtrlValue.Reached(want)` is therefore
exact equality — a plant reporting `CtrlStop90` has not carried out a gradient stop at
90°, however its blades stand, so a forced request for one is still sent. Every state
a client can have caused (0 to 8) can be commanded again.

Values reserved for the system (`CtrlValueRejected` 121, `CtrlStop60Enercon` 129,
`CtrlStopEnercon` 130, `CtrlCommError` 255) exist for reading a state but are rejected
with `ErrInvalidValue` if a caller tries to write one.

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

`*SessionStateError` (via `errors.As`) additionally reports the plant, the expected
and the observed session state.

If a session is reserved and the procedure then fails, the plant's result carries
`ErrSessionLeftOpen`; the session stays reserved until the server's own 60 s timeout,
and commands for that plant fail as occupied in the meantime. No default release is
shipped, because the Enercon technical data sheet describes no way to abort a session.
`WithSessionRelease` exists for installations whose documentation does define one.

## Options

```go
client := energontrol.New(opcxmlda.New(server),
	energontrol.WithLogger(slog.Default()),
	energontrol.WithSessionPolling(100*time.Millisecond, 5*time.Second),
	energontrol.WithCommandTimeout(30*time.Second),
	energontrol.WithMaxStateAge(30*time.Second),
)
```

| Option | Effect |
| --- | --- |
| `WithLogger` | Attach an `*slog.Logger`. By default the package logs nothing. |
| `WithSessionPolling` | How often and how long to wait for **one** session state transition — a command waits for four. Default 100 ms over 5 s, with a minimum of four attempts regardless of the clock. |
| `WithMaxStateAge` | Demand values no older than this and reject what arrives older with `ErrStaleValue`. Off by default. At most ~24 days. |
| `WithSessionLifetime` | Override the 60 s session lifetime that bounds every wait inside a command. Only for an installation whose documentation states a different value. |
| `WithCommandTimeout` | Bound a whole command, including the wait for a free session before any reservation exists. Off by default. |
| `WithItemRoot` | Point the client at a different root of the address space. Default `"Loc"`. |
| `WithLenientWriteConfirmation` | Accept a written item the server did not confirm. Gives up the only evidence a `Reset` has. |
| `WithLenientSessionVerification` | Accept a session whose id, or whose written value, could not be read back. |
| `WithLenientVerification` | Both of the above. |
| `WithSessionRelease` | Close sessions that were reserved but could not be completed. |

`New` panics on a nil transport or an unusable option value — both are programming
errors that should surface on the first run. Where the values come from a configuration
file, use `NewWithOptions`, which returns them as an error wrapping `ErrInvalidOption`.

A `Client` is safe for concurrent use. Commands are serialised per plant, because the
control session is a single plant-wide resource and overlapping sessions overwrite
each other's keys. That cannot cover a second process: route commands for one park
through one process.

> Enercon on the session mechanism: *"this mechanism only serves to regulate and
> identify access by trusted communication partners. It is not a security mechanism
> such as VPN or SSL encryption."* OPC XML-DA over plain HTTP carries the user id in
> clear text — put the endpoint behind a VPN or TLS.

## Checking a real SCADA

`cmd/scadaprobe` is an interactive diagnostic and control client. It takes its
configuration from the environment — the same variables the live test suite uses, so
an existing `.env` works as is:

```sh
OPC_URL=http://scada.example:8080/DA \
USERID=1234 PARKNO=4242 ENERGONTROL_TEST_PLANTS=2,4 \
    go run ./cmd/scadaprobe
```

It starts by reading: the park listing, which functions each plant offers, and the
state of the selected plants. Then it verifies the park number against the server and
opens a menu — read the states, diagnose the server, change the plant selection,
toggle `forceExplicitCommand`, or one of the control operations.

**Every command the library can send has a menu entry**, grouped and listed the way
the data sheet is:

| Group | Entries |
| --- | --- |
| Control values (`Ctrl/SetCtrl`) | all nine documented values, in data-sheet order: start (0), stop at 60° (1) and 90° (2), the gradient stops (3, 4), the stops for ice detection (5) and shadow flicker (6), and the species-protection stops at 60° (7) and 90° (8) |
| Rotor blade heating (`Ctrl/SetRbh`) | back to automatic (0), suppress automatic (2), heating on (10), preset duration (128) |
| Ice warning lamp (`Ctrl/SetIceDet`) | off (0), on (8) |
| Fault acknowledgement (`Reset/SetReset`) | reset |
| Several parameters in one session | a control value, a heating value and the lamp together; each of the three is asked for separately and can be left unchanged |

Every label names the value it writes, so a selection can be checked against the data
sheet without reading the source.

**Nothing is written until an operation is confirmed.** Choosing one shows what would
be sent, to which plants, as which user, the state those plants are in right now, and
what the consequences are — and then asks the operator to type that operation's *name*
back. A plain `yes` does not send it; neither does the menu key. Where an operation
needs values, they are asked for *before* the confirmation and listed on the
confirmation screen, so what is being confirmed is always on screen. A command is offered
at all only when the server confirmed `PARKNO`, `ENERGONTROL_TEST_PLANTS` names the
plants explicitly, and `USERID` is set. A command never defaults to the whole park.

The diagnosis reports whether the server pages its browse answers, whether it fills
item timestamps, whether it reports a problem with one item *per item* rather than
failing the whole request, and how long a round trip takes — with a recommended
`WithSessionPolling` budget from the p95.

Everything goes into `scadaprobe.log` as JSON lines: every menu choice, every offer
and confirmation or abort, every OPC request with the items and values it carried and
what came back, the library's own warnings, and the outcome per plant. The log is
appended to and never truncated. The user id is kept out of it, including inside the
`SessionRequest` write, where the Enercon schema puts it between the session id and
the private key; it is redacted in place.

### What it writes

Reading writes nothing — `GetStatus`, `Browse` and `Read` only. A control operation
writes exactly the three items the Enercon session schema defines, per plant, and
nothing else:

| Step | Item | Value |
| --- | --- | --- |
| reserve | `.../{Ctrl,Reset}/SessionRequest` | session id, user id, private key |
| enter | `.../Ctrl/SetCtrl`, `SetRbh`, `SetIceDet`, or `.../Reset/SetReset` | the value, private key, public key |
| submit | `.../{Ctrl,Reset}/SessionSubmit` | private key, public key |

## Layout

```
energontrol/           the library: client, protocol, value types, errors
  opcxmlda/            the OPC XML-DA transport, on top of gopcxmlda
  cmd/scadaprobe/      a diagnostic client to point at a real SCADA
  test/                tests that command real turbines (build tag opc_integration)
```

`energontrol` does not import `gopcxmlda`; only `opcxmlda` does.

## Tests

```sh
go test ./...          # unit and protocol tests, no network, no turbines
go test -race ./...
```

Every test file is named after the production file it covers. The value sets, item
names, array layouts, session states and timing constraints are reconciled against the
ENERCON SCADA PDI-OPC technical data sheet by the `TestSpec…` tests.

Tests that command **real turbines** live in `test/`, behind a build tag *and* an
environment guard, and verify the park number before sending anything:

```sh
export OPC_URL=http://scada.example:8080/DA
export USERID=1234
export PARKNO=5678                    # the park these tests may command
export ENERGONTROL_TEST_PLANTS=2,4    # the plants these tests may command
export ENERGONTROL_ALLOW_LIVE_CONTROL=yes-i-know
go test -tags opc_integration -run TestLive -v ./...
```

# Important:
**Wind turbines are critical infrastructure!** It is important to be particularly careful when interacting with them and only carry out tests in suitable test environments. I assume no liability for any consequences of using this source code, **use at your own risk**!
