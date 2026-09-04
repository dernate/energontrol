# Changelog

## v2.0.0

A breaking release that came out of an independent architecture and code audit of
v1. Everything below the "Fixes" heading was a defect in v1 that could be
reproduced; each one now has a regression test.

### Fixes

**A command could be reported as successful without ever being sent.**
`ControlAndRbh` built its per-plant action flags over the full plant list, then
filtered the list and passed the unfiltered flags to the session procedure, which
indexed them by position in the *filtered* list. As soon as one plant needed no
command, the remaining plants were checked against another plant's flag, their
`SetCtrl`/`SetRbh` write was skipped — and because success was inferred from the
session reaching its final state, the caller was told the command had been
carried out. Commands are now built as self-describing per-plant structures that
carry their own plant number, and success additionally requires that a value was
actually written.

**Plants that could not be commanded were reported as commanded.** `Start`
returned `true, nil` for plants in `CtrlStop60Enercon`, `CtrlStopEnercon` and
`CtrlCommError`; `Stop` did the same for `CtrlCommError`. A plant Enercon has
stopped is not running, and a plant with a communication error has an unknown
state. Both now yield `OutcomeNotPermitted` with `ErrPlantUnderEnerconControl` or
`ErrPlantCommunication`. An unforced `Stop` still accepts a plant Enercon already
stopped — it is standing still — but a forced one does not.

**Unchecked type assertions could panic the calling application.** The Go type of
an OPC value follows the `xsi:type` the server chose; v1 asserted `.(uint64)` and
`.(uint16)` and crashed the host process when a server disagreed, or when an item
came back without a value. All values now go through a conversion that accepts
every plausible numeric type and returns `ErrUnexpectedType` otherwise.

**Item quality and per-item fault codes were ignored.** A value the server had
explicitly marked as `bad` or flagged with a `ResultID` was used as a process
value. Reads now reject anything outside the `good…` quality family, and reject
faulted items. An optional staleness check is available via `WithMaxStateAge`.

**Responses were matched to requests by array position.** OPC XML-DA does not
guarantee that a response lists items in request order, and a response that
omitted an item left v1 with the zero value — plant 0, control state 0, which
means "running". Correlation is now by `ClientItemHandle`, then by `ItemName`; a
response that carries neither is rejected with `ErrUncorrelatable`, because
guessing by position could command the wrong turbine. Related: the request
options were spelled `returnItemName` instead of `ReturnItemName`, so a
conformant server ignored them.

**`go test ./...` commanded real turbines.** The live tests are now behind the
`opc_integration` build tag *and* `ENERGONTROL_ALLOW_LIVE_CONTROL=yes-i-know`,
they take the plants they may touch from `ENERGONTROL_TEST_PLANTS`, and they
verify `PARKNO` against the server before sending anything.

**Reserved sessions were abandoned silently.** A session that could not be
completed stayed reserved with no record, so later commands failed as "occupied"
for no visible reason. Cleanup now runs on every exit path — including a
cancelled context — checks the actual session state, and either calls the
function from `WithSessionRelease` or reports `ErrSessionLeftOpen`.

**A server that was not running produced no error.** `ServerAvailable` returned
`(false, nil)` for a reachable but suspended server, and every caller propagated
the nil. It now returns an error wrapping `ErrServerNotRunning` that names the
observed state.

**Values were written to the plant controller without validation.**
`ControlAndRbh` passed the caller's `CtrlValue` and `RbhValue` straight through,
including values reserved for the system. Writable values are now checked against
a whitelist, and a command that requests nothing returns `ErrNothingRequested`
instead of reporting success for every plant.

**`gopcxmlda.Server` was passed by value.** From gopcxmlda v1.2.0 on that is a
`copylocks` violation (over 40 of them), and it prevented the connection reuse
and typed errors that version brings. The OPC client is now an interface.

**No usable error contract.** Errors were built with `fmt.Errorf` without
sentinels, so callers could only match on strings, and slice lengths did not
always match the plant list. Every error now wraps a sentinel, and results carry
their own plant number.

**One request per plant.** `SessionRequest`, `SessionPubKey`, the value write and
`SessionSubmit` each sent a separate request per plant, and `Reset` ran the whole
state machine sequentially per plant. All steps are batched: a command for eight
plants now takes 9 requests instead of 37.

**Retry loops ignored context cancellation.** `time.Sleep` in the polling loops
meant a cancelled context did not stop a command until its retry budget was
spent. Waiting is now cancellable, and the budget is configurable through
`WithSessionPolling`.

**The library logged, and reconfigured the caller's global logger.** `LogLevel`
called `logrus.SetLevel` on the global logger. Logging is now optional via
`WithLogger` and uses `log/slog`; the logrus dependency is gone.

**Session keys came from `math/rand`.** A fresh source was seeded from
`time.Now().UnixNano()` on every call, which is predictable and can repeat.
Credentials now come from `crypto/rand`.

### Reconciliation with the ENERCON technical data sheet

After the fixes above, the implementation was compared line by line against the
ENERCON SCADA PDI-OPC data sheet, sections 3.4 and 4.1. `spec_test.go` pins down
each of these.

**The session id was never verified.** The Enercon session schema requires the
client to check its session id twice — after the reservation and again after
entering the parameters — because the session id is the only means by which a
client can tell its own reservation from another client's. Neither v1 nor the
first v2 draft did this: a client that lost a race wrote its command into
somebody else's session. Both checks are now performed, a mismatch yields
`ErrSessionIDMismatch` and the session is not treated as this client's to
release. The session id is now drawn from 1 to 19 rather than 0 to 19, so a
zero read-back unambiguously means "the server does not report it", which is
logged instead of failing the command.

**The written value was never read back.** The data sheet notes that the value
set in `SetCtrl`, `SetRbH` and `SetIceDet` can be read back for checking. It now
is, before the session is submitted, so a session is only ever submitted when it
holds the command that was requested. A mismatch yields
`ErrParameterNotAccepted`. `SetReset` is not read back — the data sheet makes no
such statement about it.

**Six documented control values could not be sent.** `SetCtrl` accepts 0 to 8;
only 0, 1 and 2 were writable. Added: `CtrlGradientStop60` (3),
`CtrlGradientStop90` (4), `CtrlStopIceDetection` (5), `CtrlStopShadowFlicker`
(6), `CtrlStopSpeciesProtection60` (7), `CtrlStopSpeciesProtection90` (8), and
the methods `SetCtrl` / `SetRbh` to reach them.

**A plant never reports a command value.** `Ctrl` only ever carries 0, 1, 2, 121,
129, 130 or 255. A forced request is therefore compared against the state the
command produces — a gradient stop at 60° is satisfied by `Ctrl` 1 — rather than
against the command value, which would never match. For the two commands whose
name states no blade angle (ice detection, shadow flicker) the data sheet
documents no resulting state, so nothing satisfies a forced request and the
command is always sent.

**`Ctrl` value 121 was unknown.** The data sheet lists it among the feedback
codes for a rejected control value. Added as `CtrlValueRejected`, mapping to
`ErrCtrlValueRejected`.

**A documented heating value was missing.** `SetRbH` accepts 0, 2, 8, 10 and
128. v1 knew 128 (`PresetDuration`); the first v2 draft dropped it. Restored as
`RbhSetPresetDuration` (one-shot, therefore always sent).

**Value 8 is deliberately not offered.** The data sheet lists it ("switch heating
on manually", leaving automatic operation enabled), but the Enercon server
rejects a bare 8 — the heating is switched on with 10, that is 8+2. There is no
constant for 8, and `RbhValue.Writable` rejects it. `RbhOn` keeps its v1
meaning: it asks for "the heating runs", not for a particular status bit, so a
plant already heating under automatic control yields `OutcomeAlreadyInState`.

**`SetIceDet` is implemented.** `IceDetOn`, `IceDetOff` and `SetIceDet` switch
the ice warning lamp (0 off, 8 on); `PlantIceDetState` and `IceDetStatusStrings`
read and decode the `IceDet` status, which says which system detected ice.
`ControlAndRbhValue` gained `SetIceDetValue` / `IceDetValue`, so a stop, a
heating command and the lamp travel in one session — the combination the ice
case actually calls for.

**Array items are written as long words.** Enercon types `SessionRequest`,
`SetCtrl`, `SetRbh`, `SetIceDet` and `SessionSubmit` as arrays of *long word*,
that is 32-bit unsigned. v1 wrote `[]uint64`, which gopcxmlda encodes as
`ArrayOfUnsignedLong` (`xsd:unsignedLong`, 64 bit); they are now `[]uint32`,
encoded as `ArrayOfUnsignedInt`. A user id or public key that does not fit in 32
bits is rejected with `ErrInvalidValue` instead of being truncated silently.
`wire_test.go` drives a real `*gopcxmlda.Server` against an HTTP test server and
asserts the type on the wire.

**The controller-type footnotes are reproduced** on `CtrlStop60`,
`CtrlGradientStop60`, `CtrlStopSpeciesProtection60` and `CtrlStop60Enercon`: not
available on controller type EP5-CS-03, the last one except on the E-160 EP5 E3.
They are documentation only — the package does not check controller types, and a
plant that does not support a value answers with `Ctrl` 121.

**"No heating installed" was detected by the wrong bit.** v1 used bit 16 as a
mask. The data sheet marks an installed heating with bit 15 and gives a
whole-word value for a plant without one — printed as 65636, where 65536 follows
from the 16-bit status word. `RbhIsInstalled` now tests bit 15, which covers a
status of 0 and both whole-word values. `RbhNotInstalled` is replaced by
`RbhNotInstalledValue`.

**The polling budget is documented against the session timeout.** The data sheet
gives 60 s for a control session on a single plant. The default budget stays at
v1's one second, which is proven in the field and well inside that; unlike in v1
it is configurable through `WithSessionPolling` and the wait is cancellable.

**`SessionTimeOut` was unused.** It is now read when a session is left open, so
the log line says how long the plant will answer "occupied".

**The session release question is settled.** The data sheet describes no way to
abort a session: it ends by running into its timeout. `WithSessionRelease`
remains for installations whose documentation defines an abort telegram, and the
documentation no longer describes the mechanism as unknown.

**Timings are now documented** for callers: 60 s session timeout, 60 s extension
after setting a value, **360 s before a new reservation after a stop**, 0 s after
a start, 180 s lockout after three wrong entries, 300 s for a wrong user id.

### API changes

| v1 | v2 |
| --- | --- |
| `module github.com/dernate/energontrol` | `module github.com/dernate/energontrol/v2` |
| `Start(ctx, Server, userID, plants...) ([]bool, []error)` | `Start(ctx, opc, userID, plants...) (Results, error)`, or `client.Start(ctx, userID, plants...)` |
| `Server gopcxmlda.Server` (by value) | `OpcClient` interface — pass `&server` |
| `CtrlValues["Stop60"]` (`map[string]uint64`) | `CtrlStop60` (typed `CtrlValue`) |
| `RbhValues["ManualOn"]` | `RbhSetManualOn` (typed `RbhValue`) |
| `RbhValues["PresetDuration"]` | `RbhSetPresetDuration` |
| — | `IceDetOn` / `IceDetOff` / `SetIceDet`, `PlantIceDetState`, `IceDetStatusStrings` |
| `RbhNotInstalled` (bit 16 mask) | `RbhNotInstalledValue` + `RbhIsInstalled(status)` |
| — | `SetCtrl` / `SetRbh` for the values the named methods do not cover |
| `RbhStatus` (exported mutable map) | `RbhStatusStrings(status)` / `RbhStatusString(status)` |
| `GetPlantCtrlOrRbhState(ctx, s, "Ctrl", plants)` | `PlantCtrlState(ctx, plants...)` / `PlantRbhState(ctx, plants...)` |
| `ServerAvailable(ctx, s) (bool, error)` | `ServerAvailable(ctx) error` |
| `LogLevel(uint32)` | `New(opc, WithLogger(l))` |
| `ControlAndRbhValue.CtrlAction` / `.RbhAction` | removed (internal state that leaked into the API) |
| — | `ControlAndRbhValue.ForceExplicitCommand` (was hardwired to `false`) |

### Migration

```go
// v1
started, errList := energontrol.Start(ctx, server, userID, 2, 4)
for i, ok := range started {
    if !ok { log.Println(plants[i], errList[i]) }
}

// v2
res, err := energontrol.Start(ctx, &server, userID, 2, 4)
if err != nil {
    return err // the command could not be attempted at all
}
for _, r := range res {
    if !r.InRequestedState() {
        log.Printf("plant %d: %v", r.PlantNo, r.Err)
    }
}
```

The important change is in the reading, not the shape: `InRequestedState()`
replaces the old `bool`, and it is false in cases where v1 returned `true` — a
plant under Enercon control, a plant with a communication error, and a plant
whose state could not be read. Code that relied on those returning `true` was
relying on a defect.

### Known limitations and open points

- **Controller type is not checked.** `CtrlValue.Writable` accepts every value
  the data sheet defines; whether a given plant supports it depends on its
  controller type. A plant that does not answers with `Ctrl` 121, surfacing as
  `ErrCtrlValueRejected`.
- **The "not installed" whole-word value is printed as 65636** in the data sheet,
  which is not a power of two and lies outside the documented 16-bit status word.
  `RbhIsInstalled` sidesteps the question by testing bit 15, which is correct for
  65636, for 65536 and for 0.
- **The private key is drawn from 1 to 32000** although the item is a long word.
  The range is inherited from v1, which is known to work; widening it would
  improve unguessability but has not been verified against a plant.
- No default session release is shipped; the data sheet defines none.
- `WithMaxStateAge` is off by default and has to be enabled deliberately.
- OPC XML-DA `MaxAge` is not sent, because gopcxmlda does not support it. The
  staleness check works from the item timestamp instead.
