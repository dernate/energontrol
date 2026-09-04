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

	client := energontrol.New(server)

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
with default settings: `energontrol.Stop(ctx, server, userID, true, false, 2, 4)`.

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

`Results.InRequestedState()` and `Results.Err()` answer the same questions for a
whole batch.

### What is deliberately *not* a success

- A plant Enercon stopped with higher rights (`CtrlStop60Enercon`,
  `CtrlStopEnercon`) cannot be started: `OutcomeNotPermitted` with
  `ErrPlantUnderEnerconControl`.
- A plant in `CtrlCommError` has an **unknown** state. It is never reported as
  running and never as stopped: `OutcomeNotPermitted` with
  `ErrPlantCommunication`.
- A state whose OPC item is missing, faulted, of bad quality, or (with
  `WithMaxStateAge`) stale is an error, not a state.

An unforced `Stop` does accept a plant Enercon already stopped — the plant is
standing still, which is what was asked for. A forced `Stop` does not.

## Commands

| Method | Purpose |
| --- | --- |
| `Start(ctx, userID, plants...)` | Run the plants. |
| `Stop(ctx, userID, fullStop, forceExplicitCommand, plants...)` | Stop at 90° (`fullStop`) or 60°. With `forceExplicitCommand` the plant must reach exactly that stop state; without it, any stop state satisfies the request. |
| `SetCtrl(ctx, userID, value, forceExplicitCommand, plants...)` | Send any documented control value, including the gradient stops and the stops for ice detection, shadow flicker and species protection. |
| `SetRbh(ctx, userID, value, plants...)` | Send any documented heating value, including `RbhSetHeatOn` and `RbhSetPresetDuration`. |
| `IceDetOn` / `IceDetOff` / `SetIceDet(ctx, userID, value, plants...)` | Switch the ice warning lamp. |
| `Reset(ctx, userID, plants...)` | Acknowledge faults. |
| `RbhOn` / `RbhAutoOff` / `RbhStandard` | Rotor blade heating: manually on, automatic suppressed, back to automatic. |
| `ControlAndRbh(ctx, userID, values, plants...)` | A control and a heating value in one session. |
| `Turbines(ctx)` | List the plants of the park and which functions each offers. |
| `ParkNo(ctx)` / `ParkNoMatch(ctx, parkNo, checkAvailable)` | Read or verify the park number. |
| `PlantCtrlState` / `PlantRbhState` / `PlantIceDetState` `(ctx, plants...)` | Read states without commanding anything. |
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
}
```

`*SessionStateError` (via `errors.As`) additionally reports the plant, the
expected and the observed session state.

## Options

```go
client := energontrol.New(server,
	energontrol.WithLogger(slog.Default()),
	energontrol.WithSessionPolling(100*time.Millisecond, 2*time.Second),
	energontrol.WithMaxStateAge(30*time.Second),
	energontrol.WithSessionRelease(releaseSession),
)
```

| Option | Effect |
| --- | --- |
| `WithLogger` | Attach an `*slog.Logger`. By default the package logs nothing. |
| `WithSessionPolling` | How often and how long to wait for a session state transition. Default 100 ms over 1 s; raise the timeout for a slower SCADA. |
| `WithMaxStateAge` | Reject plant states older than this with `ErrStaleValue`. **Off by default** — it depends on the server returning item timestamps and on both clocks agreeing. Enabling it with a value well above the SCADA's update cycle is recommended in production. |
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
| Session timeout | 60 s |
| Default polling budget per state transition (`WithSessionPolling`) | 1 s |
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

A server that does not report the session id is tolerated: the id is never drawn
as zero, so a zero read-back is logged as "cannot verify" rather than treated as
a mismatch.

> Enercon on the session mechanism: *"this mechanism only serves to regulate and
> identify access by trusted communication partners. It is not a security
> mechanism such as VPN or SSL encryption."* Protect the network path
> accordingly.

## Concurrency

A `Client` is safe for concurrent use — as long as **no two goroutines command
the same plant at the same time**. The control session is a single plant-wide
resource; overlapping sessions overwrite each other's keys. Serialise per plant,
or route all commands for a park through one goroutine.

## Tests

```sh
go test ./...          # unit and protocol tests, no network, no turbines
go test -race ./...
```

The protocol tests run against an in-memory OPC server (`fake_opc_test.go`) that
models the session state machine and can inject the failures a real SCADA
produces: unexpected value types, faulted items, bad quality, missing items,
reordered responses, occupied and stuck sessions, a session reserved by another
client, and a value that does not read back.

`spec_test.go` reconciles the implementation with the ENERCON SCADA PDI-OPC
technical data sheet: the value sets, the item names, the array layouts, the
session states and the timing constraints.

Tests that command **real turbines** are behind a build tag *and* an environment
guard, and verify the park number before sending anything:

```sh
export OPC_URL=http://scada.example:8080/DA
export USERID=1234
export PARKNO=5678                    # the park these tests may command
export ENERGONTROL_TEST_PLANTS=2,4    # the plants these tests may command
export ENERGONTROL_ALLOW_LIVE_CONTROL=yes-i-know
go test -tags opc_integration -run TestLive -v ./...
```
