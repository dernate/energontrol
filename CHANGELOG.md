# Changelog

## v2.0.0

Breaking release. The module path is now `github.com/dernate/energontrol/v2`, and
the module requires Go 1.26 and `gopcxmlda` v1.2.2.

### Migration

- `New`, `NewWithOptions` and every package-level command take a `Transport`
  instead of a `*gopcxmlda.Server`: `energontrol.New(opcxmlda.New(server))`,
  importing `github.com/dernate/energontrol/v2/opcxmlda`.
- Commands return `Results` — one `PlantResult` per plant, carrying its own plant
  number, `Outcome` and `Err` — instead of `([]bool, []error)`. Use
  `InRequestedState()`, not `Err == nil`.
- `PlantCtrlState`, `PlantRbhState` and `PlantIceDetState` return one entry per
  requested plant with the reason in that entry's `Err`. The returned `error` is
  reserved for failures of the whole request.
- Control and heating values are typed constants (`CtrlValue`, `RbhValue`,
  `IceDetValue`), not bare numbers. In v1 they lived in a `map[string]uint64`,
  where a misspelled `"Stop"` yielded the zero value — which is `CtrlStart`.
- `Turbines` is all-or-nothing and reports plants it cannot address in
  `TurbineInfo.Unsupported`.
- Options reject an unusable value instead of ignoring it. `New` panics on one;
  `NewWithOptions` returns it wrapping `ErrInvalidOption`.

### What a plant reports in `Ctrl`

`Ctrl` carries the value that was set: after `SetCtrl 7` a plant reports 7 and
holds it, never the blade angle that stop produces. The package assumed the
opposite; for the values 1 and 2 both readings are numerically identical, so only
the values 3 to 8 expose the difference.

Consequently `Commandable` covers 0 to 8 rather than 0, 1, 2 — so a plant stopped
by a gradient, ice-detection, shadow-flicker or species-protection stop can be
started again, and such a stop is recognised as having taken effect.
`CtrlValue.Reached(want)` is new and exported: exact equality against the written
value, so a plant reporting `CtrlStop90` has not carried out a gradient stop at
90° and a forced request for one is still sent.

Enercon's "only operating states with the values 0-2 can be changed" describes the
three states its own table lists; it is not a lock-out.

### Commands that reported success without it

- **A command could be reported as successful without being sent.**
  `ControlAndRbh` built its per-plant flags over the full plant list but indexed
  them by position in the filtered one, so as soon as one plant needed no command
  the others' write was skipped. Success was inferred from the session reaching
  its final state.
- **An unforced stop accepted a state nobody asked for.** A plant at 60° asked
  for a 90° full stop returned `OutcomeAlreadyInState` with
  `InRequestedState() == true` and nothing written; so did a plant stopped for
  species protection at 60° asked for a plain 60° stop, which left it under
  species protection. A request is satisfied only where sending the command
  would be wrong or impossible: a *deeper* stop, whose blades commanding would
  open back up, or a stop Enercon holds the plant in. Anything else is sent.
- **Plants that could not be commanded were reported as commanded.** `Start`
  returned success for `CtrlStop60Enercon`, `CtrlStopEnercon` and `CtrlCommError`;
  `Stop` did the same for `CtrlCommError`. Both yield `OutcomeNotPermitted` now.
- **An unconfirmed write counted as a write.** A missing item in the
  `WriteResponse` mapped to "accepted" — the inverse of the rule reads follow, and
  `Reset` has no other evidence. Now `ErrItemMissing`.
- **Verification degraded silently to a log warning.** An unreadable session id or
  value read-back logged and carried on, and the plant came back
  `OutcomeCommanded` with `Err == nil` — invisible, since the default logger
  discards everything. Now `ErrSessionUnverified`.
- **`WithMaxStateAge` failed open** on an item without a timestamp, which is the
  kind of server it guards against. Now `ErrNoItemTime`.
- **`OutcomeFailed` implied that nothing was written.** A procedure failing after
  a confirmed submit also carries `ErrOutcomeUncertain`: a retried `Start` or
  `Reset` is a second command to a plant that may already have had one.
- **An unknown plant state was blamed on the session.** A `Ctrl` value outside the
  known set — 137 was observed on a real park — is refused with the new
  `ErrPlantStateUnknown` rather than `ErrSessionState`.
- **Unchecked type assertions on OPC values could panic the calling process.**

### Protocol and data sheet

- **The session id is verified**, after the reservation and again after entering
  the parameters. It is the only way to tell one's own reservation from another
  client's, so a client that lost a race wrote into somebody else's session.
  Mismatch yields `ErrSessionIDMismatch`. Ids are drawn from 1 to 19, so a zero
  read-back unambiguously means the server does not report it.
- **The written value is read back** before the submit. Where the value is 0
  (`CtrlStart`, `RbhSetStandard`, `IceDetLampOff`) the private key is checked
  instead, since an item never written reads back as 0 too.
- **The polling budget bounds the session.** It applied per state transition, and
  a command waits for four: at the documented 55 s a command could run for over
  three minutes, writing into a session the server had dropped after 60 s. The
  session lifetime starts with the reservation and clamps every wait;
  `ErrSessionExpired` reports running out. The default is 5 s with a minimum of
  four attempts.
- **`ServerState` is checked on every read and write response**, not once per
  command via `GetStatus`, so a server degrading mid-session stops receiving
  commands.
- **`SetCtrl` accepts 0 to 8**; only 0, 1 and 2 were writable. Added the gradient
  stops, the stops for ice detection and shadow flicker, and the
  species-protection stops, plus `SetCtrl` and `SetRbh` to reach them.
- **`SetIceDet` is implemented**, with `PlantIceDetState` and
  `IceDetStatusStrings`. `ControlAndRbhValue` carries it, so a stop, a heating
  command and the lamp travel in one session.
- **`SetRbh` 128** (preset duration) was missing. Value 8 is deliberately not
  offered: the server rejects a bare 8, the heating is switched on with 10.
- **`Ctrl` 121** (value rejected) was unknown. Added as `CtrlValueRejected`,
  surfacing as `ErrCtrlValueRejected` — which is how a plant answers a value its
  controller type does not support.
- **Array items are written as long words** (`ArrayOfUnsignedInt`); v1 sent
  `ArrayOfUnsignedLong`.
- **"No heating installed" is detected by bit 15**, which also covers the
  whole-word values 0, 65536 and 65636.
- **Commands are serialised per plant** inside the `Client`, in plant-number
  order, cancellable through the context.
- **Timings are documented**, notably the 360 s delay before a new reservation
  after a stop and the lockouts after wrong keys or a wrong user id.
- **`go test ./...` no longer commands real turbines.** They are behind the
  `opc_integration` tag, an environment guard and a park number check.

### The transport is a port of this package's own

`Transport` speaks item names and unsigned integers; `opcxmlda.New(server)` adapts
a `*gopcxmlda.Server` to it. The interface it replaced was built from
`gopcxmlda`'s types, so every breaking change there was a breaking change here.
`go list -deps` on the root package no longer names `gopcxmlda`.

Three rules live in the adapter:

- **An item fault no longer fails the whole request.** OPC XML-DA reports it in
  the item's `ResultID` and adds an `<Errors>` element, which the client library
  surfaces as an error on the call. The per-item error path was therefore
  unreachable against a conformant server, one bad item name failed every plant of
  a batch, and each reserved session stayed open for the server's 60 s timeout.
- **A paged browse is followed to its end.** `MoreElements` and
  `ContinuationPoint` were read nowhere, so a paged listing produced a short park
  with `err == nil`. `ErrBrowseIncomplete` covers a server that pages badly.
- **Write replies are not decoded for values.** A bare confirmation carries no
  `Value`; decoding it as one made every command fail at the first write against a
  server that confirms without echoing — which an Enercon SCADA does.

Responses are correlated by `ClientItemHandle` and cross-checked against
`ItemName`; a contradiction is `ErrUncorrelatable`. v1 matched by array position.

Every read carries its freshness requirement as OPC XML-DA's `MaxAge` attribute on
the request's item list, so `WithMaxStateAge` prevents a stale value rather than
only detecting one.

### API additions

`WithItemRoot`, `WithCommandTimeout`, `WithSessionLifetime`,
`WithLenientWriteConfirmation`, `WithLenientSessionVerification`,
`WithLenientVerification`, `WithSessionRelease`, `CtrlValue.Reached`,
`TurbineInfo.Unsupported`, `PlantStates`/`RbhStates`/`IceDetStates` with `Err()`
and `Get(plant)`.

New sentinels: `ErrSessionUnverified`, `ErrSessionExpired`, `ErrNoItemTime`,
`ErrInvalidUserID`, `ErrInvalidOption`, `ErrBrowseIncomplete`,
`ErrOutcomeUncertain`, `ErrPlantStateUnknown`.

### Layout, tooling and tests

```
energontrol/           the library
  opcxmlda/            the OPC XML-DA transport, on top of gopcxmlda
  cmd/scadaprobe/      an interactive diagnostic client for a real SCADA
  test/                tests that command real turbines (tag opc_integration)
```

Every test file is named after the production file it covers.
`opcxmlda/transport_test.go` drives the adapter against real SOAP over HTTP — the
layer the in-memory fake sits above and therefore cannot reach. `doc_test.go` is
`package energontrol_test` and checks that the exported surface is enough to use
the package.

`cmd/scadaprobe` is new: it reads and diagnoses a real SCADA, offers every command
the library can send, and writes nothing until an operation is confirmed by typing
that operation's name. It found the write-reply decoding defect above. The user id
is kept out of its log.

CI runs `gofmt`, `go vet`, golangci-lint (pinned, with `errorlint`, `nilerr`,
`exhaustive` and `durationcheck`) and `go test -race` with an 88 % coverage floor.

### Security

The session mechanism is not a credential. Enercon: *"this mechanism only serves
to regulate and identify access by trusted communication partners. It is not a
security mechanism such as VPN or SSL encryption."* OPC XML-DA over plain HTTP
carries the user id in clear text, so the endpoint belongs behind a VPN or TLS.
