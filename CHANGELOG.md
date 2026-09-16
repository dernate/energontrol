# Changelog

## v2.0.0 (unreleased)

### Ctrl carries the value that was set

`Ctrl` reports the value that was written, not the blade angle it produces:
after `SetCtrl 7` a plant reports 7 and holds it. The package assumed the
opposite. For the values 1 and 2 both readings are numerically identical, so
only the values 3 to 8 expose the difference.

| | before | now |
| --- | --- | --- |
| `Commandable` | 0, 1, 2 | 0 to 8 |
| `Stopped` | 1, 2, 129, 130 | plus 3 to 8 |
| `stopDepth` | 1, 129 → 60°; 2, 130 → 90° | plus 3, 7 → 60°; 4, 8 → 90° |
| a forced request is satisfied by | the blade angle | the written value, exactly |

- A plant stopped by a gradient, ice-detection, shadow-flicker or
  species-protection stop can be started again. It was refused as a state the
  package does not know.
- Such a stop is recognised as having taken effect. A monitoring loop waiting
  for the blade angle never matched.
- `CtrlValue.Reached(want)` is new and exported: exact equality against the
  written value. A plant reporting `CtrlStop90` has not carried out a gradient
  stop at 90°, so a forced request for one is still sent.
- The stops for ice detection and shadow flicker became verifiable. They are
  still always sent, since no state satisfies them in advance.

Enercon's "only operating states with the values 0-2 can be changed" describes
the three states its own table lists; it is not a lock-out.

### An unforced stop no longer accepts a shallower one

`ctrlSatisfied` ended in `current.Stopped()`, which is true for every stop
state, so any stop satisfied any stop request. A plant at 60° asked for a 90°
full stop returned `OutcomeAlreadyInState` with `InRequestedState() == true`,
and nothing was written.

A stop request is now satisfied only by a state at least as stopped as the one
asked for, ranked by blade angle.

| plant reports | request | before | now |
| --- | --- | --- | --- |
| `Stop60` | 90° | already in state, nothing written | commanded |
| `Stop90` | 60° | already in state | unchanged |
| `Stop60Enercon` | 90° | already in state | `ErrPlantUnderEnerconControl` |
| `Stop60Enercon` | 60° | already in state | unchanged |
| `StopEnercon` | either | already in state | unchanged |
| `CommError` | either | not permitted | unchanged |

`forceExplicitCommand` is unaffected. `CtrlValue.Stopped()` keeps its meaning
and stays exported; it no longer decides whether a command is needed.

### ErrPlantStateUnknown

A `Ctrl` value outside the set this package knows — 137 was observed on a real
park — is refused with a new sentinel instead of `ErrSessionState`, whose
message pointed at the session machinery:

	energontrol: plant reports a control state this package does not know (state CtrlValue(137))

The behaviour is unchanged: refuse, write nothing, never report the plant as
being in the requested state. `scadaprobe` adds that retrying changes nothing
and that the raw value has to be looked up for the controller type.

### The OPC layer is a port of this package's own

`Transport` speaks item names and unsigned integers; `opcxmlda.New(server)`
adapts a `*gopcxmlda.Server` to it. The interface it replaces, `OpcClient`, was
built from `gopcxmlda`'s types and signatures, so every breaking change there
was a breaking change in this package's public API. `go list -deps` on the root
package no longer names `gopcxmlda`.

Three rules moved into the adapter:

- **An item fault no longer fails the whole request.** OPC XML-DA reports it in
  the item's `ResultID` and adds an `<Errors>` element; `gopcxmlda` surfaces that
  as an error from `Read`, and the package discarded the whole response. The
  per-item error path — `ErrItemFault`, `ErrBadQuality`, `ErrItemMissing`,
  `PlantState.Err` — was therefore unreachable against a conformant server, one
  bad item name failed every plant of a batch, and each reserved session stayed
  open for the server's 60 s timeout. An error consisting of nothing but
  `*OpcResponseError` is now classified as item-level. A transport failure, a
  SOAP fault, and an `<Errors>` element no returned item accounts for remain
  failures of the request.
- **A paged browse is followed to its end.** `MoreElements` and
  `ContinuationPoint` were read nowhere, so a paged listing produced a short park
  with `err == nil` and an empty `Unsupported`. `ErrBrowseIncomplete` covers a
  server that announces more elements without a point, repeats one, or never
  finishes.
- **Write replies are not decoded for values.** A write reply confirms items;
  `ReturnValuesOnReply` invites a server to echo values but does not oblige it.
  Decoding them anyway made every bare confirmation `ErrUnexpectedType`, so
  against a server that confirms without echoing — which an Enercon SCADA does —
  every command failed at the first write of the session. Found by
  `cmd/scadaprobe` against real SOAP; the in-memory fake sits above the decoding.

### Findings of the second pre-release audit

- **A contradictory correlation is refused.** Where a server supplied both
  `ClientItemHandle` and `ItemName` and they disagreed, the handle won silently —
  two such items are two swapped turbines. A contradiction is now
  `ErrUncorrelatable`.
- **A plant named only in `Name` is no longer lost.** `filterPlants` matched the
  full item name only, which OPC XML-DA does not require for branches. Both forms
  are recognised, duplicates collapsed, and a node that cannot be made sense of
  is reported in `Unsupported` rather than dropped.
- **The polling budget has a floor.** The default of 1 s per state transition
  bought two or three attempts on a SCADA answering in 300 ms. It is 5 s now,
  with at least four attempts regardless of the clock; the session lifetime still
  overrides both.
- **`WithLenientVerification` is split.** One name covered three tolerances and
  its godoc described one. `WithLenientWriteConfirmation` and
  `WithLenientSessionVerification` each document what they give up;
  `WithLenientVerification` remains as the convenience for both.
- **`New` reports what it cannot use.** `New(nil)` returned a client that
  panicked on its first command; `WithSessionPolling(0, 0)` and
  `WithSessionLifetime(-1)` discarded their arguments silently. `New` panics
  immediately, `NewWithOptions` returns an error wrapping `ErrInvalidOption`.
- **Options are order-independent.** `WithSessionPolling` clamped against
  whichever lifetime was set at the moment it ran. Options now only record; the
  clamping happens once in `New`.
- **`WithCommandTimeout` bounds a command as a whole.** The session lifetime
  starts at the reservation, so the wait for state "free" was outside it.
- **`OutcomeFailed` no longer implies that nothing was written.** A procedure
  that fails only after a confirmed submit also carries `ErrOutcomeUncertain`: a
  retry of `Start` or `Reset` would be a second command to a plant that may
  already have had one, and neither is protected by the 360 s re-reservation
  delay.
- **A zero value read-back is checked against the private key.** `CtrlStart`,
  `RbhSetStandard` and `IceDetLampOff` write 0, which is what an item that was
  never written reads back. The private key is never drawn as zero; where the
  server reports it, it is checked instead.

### The low-severity findings

- **`WithItemRoot`.** Item names were built from a hardcoded "Loc" prefix, so an
  installation exposing its park elsewhere answered nothing, with no hint why. A
  plant node also enters the listing only if this package would address it under
  the name the server gave it — `Plant007` was taken as plant 7 and commanded as
  `Plant7`.
- **A swallowed error no longer becomes a different reason.** `parameterItems`
  discarded the error from `longWord`, which `writeParameters` reported as
  "nothing to write". It returns `([]ItemWrite, error)` now.
- **A diagnostic read only happens when the diagnosis is read.** The remaining
  session timeout was a call argument in a log line, so every left-open session
  cost an extra OPC read into a discarded log. It is behind `Logger.Enabled`, and
  the default logger is `slog.DiscardHandler` — a text handler on `io.Discard`
  reports itself as enabled, so the guard alone would not have worked.
- **The result set copies the plant list.** `newResultSet` kept the caller's
  variadic slice and read the report order from it after the command, so a caller
  reusing that slice got results attributed to plants it never asked about.
- **The linter is pinned.** CI ran golangci-lint at `latest` with no
  configuration. `.golangci.yml` pins the rules, `ci.yml` pins v2.13.2. Beyond
  the standard set: `errorlint`, `nilerr`, `exhaustive`, `durationcheck`. The
  coverage floor moves from 85 % to 88 %.
- **`doc_test.go` sees the package from outside.** `package energontrol_test`,
  implementing a `Transport` from the exported types alone — a compile-time proof
  that the port carries no unexported dependency.

### Data freshness is demanded, not just checked

`WithMaxStateAge` compared the timestamp the server chose to send, which detects
a stale value but does not prevent one and does nothing at all on a server that
reports no timestamps.

Every read now carries the age as the `MaxAge` attribute on the request's item
list, which is where the specification puts it — not in `RequestOptions`, which
has no such attribute. The timestamp check stays as the second line, so a server
that ignores the attribute is still caught.

With the option off, no `MaxAge` is sent: `MaxAge=0` is a demand for the most
accurate data available, not the absence of one. Sub-millisecond ages round down
to that device read, and an age beyond the `xs:int` range the attribute uses
(about 24 days) is rejected as an option error. This is what `Transport.Read`
gained its `ReadOptions` argument for.

### cmd/scadaprobe

An interactive diagnostic and control client to point at a real SCADA,
configured from `OPC_URL`, `USERID`, `PARKNO` and `ENERGONTROL_TEST_PLANTS`.

- Nothing is written until an operation is confirmed by typing that operation's
  own name, not `yes`. A command is offered only when the park is confirmed, the
  plants are named explicitly, and a user id is set.
- Every command the library can send has a menu entry, grouped and ordered as the
  data sheet is, each label naming the value it writes.
  `TestEveryDocumentedValueIsOnTheMenu` fails if a documented value has no entry
  or the control group leaves data-sheet order.
- `forceExplicitCommand` is a menu toggle; only `false` was ever passed before.
  `ControlAndRbh` is reachable: a control value, a heating value and the lamp in
  one session per plant, each part optional.
- The diagnosis reports whether the server pages browse answers, fills item
  timestamps and reports a faulted item per item, and the round-trip latency the
  polling budget has to be sized against.
- Everything lands in `scadaprobe.log` as JSON lines. The user id is kept out of
  it — it was a field of the `session started` and `command sending` records and
  the middle element of the `SessionRequest` write array, where the Enercon
  schema puts it between the session id and the private key; it is redacted in
  place.
- The fake SCADA in the tool's tests confirmed written parameters with a constant
  `{0, 0, 0}`, so the library's read-back verification was never exercised
  through it. It serves back what was written now.

### Package layout

```
energontrol/           the library: client, protocol, value types, errors
  opcxmlda/            the OPC XML-DA transport, on top of gopcxmlda
  cmd/scadaprobe/      a diagnostic client to point at a real SCADA
  test/                tests that command real turbines (tag opc_integration)
```

`NewGopcxmldaTransport(server)` became `opcxmlda.New(server)`; the package
boundary turns a leaked `gopcxmlda` type into a compile error. Three unexported
helpers the adapter used were replaced by local equivalents: `wrapf`,
`serverStateRunning` and `serverStateError`, the last by a wrapped
`ErrServerNotRunning`.

The live suite is `test/live_test.go`. It needs nothing unexported, so it also
serves as a check that the exported API is enough to run a park with.

The core is not split further: `tracker`, `sessionProcedure`, `plantCommand`,
`itemNamer`, `resultSet`, `correlate` and `ctrlSatisfied` are unexported and used
across every boundary a layered split would draw.

### Test file layout

Every test file is named after the production file it covers. Three carry more
than their name suggests: `transport_test.go` holds the in-memory `Transport`;
`opcxmlda/transport_test.go` holds everything that needs real SOAP over HTTP;
`doc_test.go` is `package energontrol_test` and holds the runnable example.

Comments no longer refer to audit findings by number.

`opcxmlda/transport_test.go` covers what the in-memory fake sits above: item
faults with and without an `<Errors>` element, a SOAP fault alongside item
errors, correlation by handle and by name, a handle contradicting its item name,
duplicated and unrequested items, every `xsi:type` a server may choose for a
number, arrays, quality and timestamps, browse paging in its three failure modes,
and `ServerState` on a browse reply.

### Transport dependency

`gopcxmlda` moves to v1.2.2:

- v1.2.1 adds the `MaxAge` attribute.
- v1.2.2 corrects where a Read request carries `LocaleID` and
  `ClientRequestHandle`. The WSDL gives `Read` no attributes of its own; they
  belong on `RequestOptions`. `opcxmlda/transport_test.go` pins the element as
  attributeless.

Not a finding, recorded so it is not raised as one: a response `gopcxmlda` cannot
decode — a `<Value>` without the `xsi:type` the schema requires — fails as a
whole rather than per item. That is a deliberate fail-fast contract on the
transport's side, pinned by `TestAdapterUnparseableResponseIsARequestFailure`.

### Go version

The `go` directive moves from 1.23.0 to 1.26, which raises the minimum for
consumers and is what makes `slog.DiscardHandler` available.

- `go 1.26` is the minimum a consumer needs, without a patch on purpose: any
  1.26.x or newer toolchain builds the module, whereas `go 1.26.7` would force
  every build on an older 1.26.x to download a matching toolchain first.
- `toolchain go1.26.7` is what building *this* module uses and is ignored when
  the module is a dependency. Bump with `go get toolchain@go1.26.8`.

CI takes neither from go.mod: `setup-go` prefers a `toolchain` line over the `go`
line, so the workflow asks for `go-version: '1.26.x'` with `check-latest: true`.
The suite passes under 1.26.7 and, with `GOTOOLCHAIN=local`, under 1.26.0.

### Migration from the earlier v2 candidate

- `New`, `NewWithOptions` and every package-level command take a `Transport`
  instead of an `OpcClient`: `energontrol.New(opcxmlda.New(server))`, importing
  `github.com/dernate/energontrol/v2/opcxmlda`.
- `SessionReleaseFunc` receives a `Transport`.
- `WithLenientVerification` still enables both tolerances; prefer
  `WithLenientWriteConfirmation` or `WithLenientSessionVerification`.
- `WithSessionPolling`, `WithSessionLifetime`, `WithMaxStateAge` and
  `WithCommandTimeout` reject an unusable value instead of ignoring it. `New`
  panics on one; use `NewWithOptions` to get an error.
- The default polling timeout is 5 s instead of 1 s.
- New sentinels: `ErrInvalidOption`, `ErrBrowseIncomplete`, `ErrOutcomeUncertain`,
  `ErrPlantStateUnknown`.
- New option `WithItemRoot`; new method `CtrlValue.Reached`.
- `Transport.Read` takes a `ReadOptions` argument carrying `MaxAge`.
- The module requires `gopcxmlda` v1.2.2 and Go 1.26 or newer.

### Findings of the first pre-release audit

The safety contract v2 states in its own documentation was not upheld on several
paths — the same class as the v1 defects v2 was written to remove: reporting
success without the evidence for it.

- **An unconfirmed write counted as a write.** `writeValues` mapped an item
  missing from the `WriteResponse` to "accepted". `Reset` has no second line of
  defence, since the data sheet defines no readable parameter for `SetReset`, so
  a `Reset` whose item the server never echoed was `OutcomeCommanded`. A missing
  item is `ErrItemMissing`; `WithLenientWriteConfirmation` restores the old
  behaviour for a server that does not echo.
- **Verification degraded silently to a log warning.** An unreadable session id
  or value read-back logged and carried on, and the plant came back
  `OutcomeCommanded` with `Err == nil` — invisible, since the default logger
  discards everything. It fails with `ErrSessionUnverified` now. One case stays
  tolerated: a session id read back as zero, which means the server does not
  report it, because this package never draws zero. A one-element array typed as
  a bare scalar counts as a valid read-back.
- **The polling budget did not bound the session.** It applied per state
  transition, and a command waits for four. At the documented 55 s a command
  could run for over three minutes, writing into a session the server had dropped
  after 60 s. The session lifetime starts with the reservation, every wait is
  clamped to what is left of it, and a command that runs out reports
  `ErrSessionExpired` joined with the observed `SessionStateError`.
- **`ServerState` was checked once per command, via `GetStatus`.** It is carried
  in the reply base of every response, so a server that degraded mid-session kept
  receiving control commands. Every read and write response is checked; an empty
  state stays tolerated. This also closes `ParkNoMatch(ctx, parkNo, false)`
  reporting a positive match from a suspended server.
- **`WithMaxStateAge` failed open** on an item without a timestamp — the kind of
  server it guards against. Now `ErrNoItemTime`, wrapping `ErrStaleValue`.
- **One unreadable plant blinded the caller to the whole park.**
  `PlantCtrlState`, `PlantRbhState` and `PlantIceDetState` returned `nil` and the
  first plant's error. They return one entry per requested plant with the reason
  in that entry's `Err`; the returned `error` is reserved for failures of the
  whole request, and `PlantStates.Err()` gives the all-or-nothing answer.
- **The user id was validated too late, per plant, with the wrong sentinel.** It
  was rejected inside `requestSessions` after three reads, as a per-plant
  `OutcomeFailed` wrapping `ErrInvalidValue`. It is an argument error: checked at
  every public entry point before anything is sent, wrapping `ErrInvalidUserID`.
- **The documented concurrency invariant was not enforced.** Commands are
  serialised per plant inside the `Client`, acquired in plant-number order,
  cancellable through the context, never applied to read-only calls.
- **`SessionWaitLoop` (3) was treated as a failure.** Reaching it already proves
  the submit was accepted, since only a submit moves a session out of parameter
  input.
- **Plants the package cannot address disappeared silently.** A `Plant<n>` node
  whose number does not fit a `uint8`, and any plant-shaped node off the scheme,
  were dropped without a trace. They are reported in `TurbineInfo.Unsupported`
  and logged, and the plant list is sorted.
- **`Turbines` returned a half-filled park next to an error** and issued up to
  three sequential browses per plant — over a hundred round trips for a park of
  forty. It is all-or-nothing now, and the per-plant browses run six at a time.
- **Test gaps.** `discovery.go` and `api.go` had no unit test, and the
  `ReadErr`/`WriteErr` knobs the fake provided were never set. Added
  `discovery_test.go`, `api_test.go`, `transport_test.go` and a test for every
  item above, plus a CI workflow running `go vet`, `gofmt`, `staticcheck` and
  `go test -race` with a coverage floor.
- **Documentation drift.** `README.md` referred to a constant `RbhSetHeatOn` that
  does not exist; `ErrNothingRequested` documented two of three values;
  `releaseSessions` described a session in `SessionWaitEnd` as unheld although
  the plant answers occupied for another 360 s. The transport-security
  consequence is now stated outright: the session mechanism is not a credential,
  plain HTTP carries the user id in clear, so the endpoint belongs behind a VPN
  or TLS.
- **Smaller items.** `joinErrors` removed; `ControlAndRbh` computed
  `stateError()`/`rbhStateError()` twice per plant; `cred.requested` was set
  before the argument checks, producing a pointless cleanup read; the
  nil-credential invariants in `fetchPublicKeys`, `writeParameters` and
  `submitSessions` are checked rather than relied upon.

### API changes from the audits

| candidate | released |
| --- | --- |
| `PlantCtrlState(...) ([]PlantState, error)` | `(PlantStates, error)` — one entry per plant, `PlantState.Err` per plant |
| `PlantRbhState(...) ([]RbhState, error)` | `(RbhStates, error)` — likewise |
| `PlantIceDetState(...) ([]IceDetState, error)` | `(IceDetStates, error)` — likewise |
| — | `PlantStates`/`RbhStates`/`IceDetStates` with `Err()` and `Get(plant)` |
| — | `TurbineInfo.Unsupported` |
| — | `WithLenientVerification`, `WithSessionLifetime`, `WithItemRoot`, `WithCommandTimeout` |
| — | `CtrlValue.Reached` |
| — | `ErrSessionUnverified`, `ErrSessionExpired`, `ErrNoItemTime`, `ErrInvalidUserID`, `ErrInvalidOption`, `ErrBrowseIncomplete`, `ErrOutcomeUncertain`, `ErrPlantStateUnknown` |

## v2.0.0 — original v1 audit

A breaking release that came out of an independent architecture and code audit of
v1. Everything below the "Fixes" heading was a defect in v1 that could be
reproduced; each one now has a regression test.

### Fixes

**A command could be reported as successful without ever being sent.**
`ControlAndRbh` built its per-plant action flags over the full plant list but
indexed them by position in the *filtered* one, so as soon as one plant needed no
command the others were checked against another plant's flag and their
`SetCtrl`/`SetRbh` write was skipped. Success was inferred from the session
reaching its final state, so the caller was told otherwise. Commands are
self-describing per-plant structures now, and success requires that a value was
written.

**Plants that could not be commanded were reported as commanded.** `Start`
returned `true, nil` for plants in `CtrlStop60Enercon`, `CtrlStopEnercon` and
`CtrlCommError`; `Stop` did the same for `CtrlCommError`. Both now yield
`OutcomeNotPermitted` with `ErrPlantUnderEnerconControl` or
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
ENERCON SCADA PDI-OPC data sheet, sections 3.4 and 4.1. The `TestSpec…` tests
pin down each of these.

**The session id was never verified.** The session schema requires the client to
check it after the reservation and again after entering the parameters; it is the
only way to tell one's own reservation from another client's, so a client that
lost a race wrote its command into somebody else's session. Both checks are
performed now, and a mismatch yields `ErrSessionIDMismatch` without treating the
session as this client's to release. The id is drawn from 1 to 19 rather than
0 to 19, so a zero read-back unambiguously means the server does not report it,
which is logged rather than failing the command.

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

**A plant never reports a command value.** `Ctrl` was taken to carry only 0, 1,
2, 121, 129, 130 or 255, so a forced request was compared against the state the
command produces rather than against the command value.

> **Superseded.** This is wrong. A real park reports the value that was written
> and holds it. See "Ctrl carries the value that was set" above.

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
`opcxmlda/transport_test.go` drives a real `*gopcxmlda.Server` against an HTTP
test server and asserts the type on the wire.

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
| `Start(ctx, Server, userID, plants...) ([]bool, []error)` | `Start(ctx, transport, userID, plants...) (Results, error)`, or `client.Start(ctx, userID, plants...)` |
| `Server gopcxmlda.Server` (by value) | `Transport` port — pass `opcxmlda.New(&server)` |
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
- **Plant numbers are `uint8` by design.** An Enercon park holds 20 to 30
  turbines, so 255 is an order of magnitude of headroom, and the narrow type
  keeps the number usable as a map key and struct field everywhere without
  conversions. A plant node outside the range is reported in
  `TurbineInfo.Unsupported` rather than dropped, so the limit cannot hide a
  turbine.
- **Only the root of the address space is configurable.** `WithItemRoot` moves
  the prefix; the `Wec` branch, the `Plant<n>` nodes, the `Ctrl` and `Reset`
  branches, the item names and the `/` between them follow the data sheet. The
  item-name delimiter is vendor-defined in OPC XML-DA, so a SCADA using another
  one would need more than a root — but that is not a shape any Enercon
  installation is known to take, and inventing configuration for it would be
  speculative.
- No default session release is shipped; the data sheet defines none.
- `WithMaxStateAge` is off by default and has to be enabled deliberately.
