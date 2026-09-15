# Changelog

## v2.0.0 (unreleased)

### Findings from the first run against a real SCADA

**A plant already stopped at 60° can now be taken to 90°.** An unforced `Stop`
reported a plant at 60° as `OutcomeAlreadyInState` when a 90° full stop was
requested, wrote nothing, and `InRequestedState()` answered true — a wrong yes
of exactly the kind the rest of the package is built to avoid. 60° is not 90°,
and the blades stayed where they were while the caller was told the full stop
had happened.

The cause was the tolerance being expressed as set membership. `ctrlSatisfied`
ended in `current.Stopped()`, and `Stopped()` is true for all four stop states,
so every stop satisfied every stop request. The tolerance was only ever
justified for the two states Enercon reserves for itself: a plant Enercon has
stopped is standing still, which is what a stop asks for, and it cannot be
commanded anyway.

The tolerance is now monotonic instead. An unforced stop request is satisfied by
any state at least as stopped as the one asked for, ranked by blade angle:
running, 60° (`CtrlStop60`, `CtrlStop60Enercon`), 90° (`CtrlStop90`,
`CtrlStopEnercon`). A state that says nothing about where the blades are —
`CtrlValueRejected`, `CtrlCommError` — ranks below running and satisfies
nothing, unchanged. In practice:

| plant reports | request | before | now |
| --- | --- | --- | --- |
| `Stop60` | 90° full stop | already in state, nothing written | commanded |
| `Stop90` | 60° stop | already in state | already in state (unchanged — commanding it would open the blades back up) |
| `Stop60Enercon` | 90° full stop | already in state | `OutcomeNotPermitted`, `ErrPlantUnderEnerconControl` |
| `Stop60Enercon` | 60° stop | already in state | already in state (unchanged) |
| `StopEnercon` | either stop | already in state | already in state (unchanged) |
| `CommError` | either stop | not permitted | not permitted (unchanged) |

`forceExplicitCommand` is unaffected: it still requires exactly the state the
command produces. The stops for ice detection and shadow flicker still have no
documented resulting state and are therefore always sent.

`CtrlValue.Stopped()` keeps its meaning and stays exported — it answers "is this
plant standing still", which is a fair question for a caller to ask. It is no
longer what decides whether a command is needed.

**The user id no longer reaches the `scadaprobe` log.** It was written as a
field of the `session started` and `command sending` records and, less visibly,
as the middle element of the `values` array of the `SessionRequest` write, where
the Enercon schema puts it between the session id and the private key. It is the
operator's credential for the park and a log file gets copied into tickets, so
it is redacted in place — the array still reads as the data sheet defines it and
the other two elements stay checkable.

### Ctrl carries the value that was set

A live run stopped a plant for species protection. The command went through, the
plant reported `Ctrl` = **7** — the value that was set — and held it. Two things
went wrong, both from one assumption:

- the tool waited a full minute for `Stop60` and then warned that the command
  might still be taking effect, while the plant had been stopped for 59 of those
  seconds;
- afterwards the plant could not be started, because `Commandable` allowed only
  0, 1 and 2, so state 7 was refused as a state this package does not know.

The assumption was that a plant never reports the command values 3 to 8 and
always answers with the resulting blade angle. It was written into
`expectedCtrlAfter`, `Stopped`, `stopDepth` and `Commandable`, stated in the
package documentation, and pinned by a test named after it. **No blade angle is
ever reported in `Ctrl`.** The item carries the value that was set, and for the
values 1 and 2 the two readings are numerically identical, which is why nothing
ever contradicted the wrong one.

	// Reached reports whether a plant reporting state v has carried out the
	// command value want.
	func (v CtrlValue) Reached(want CtrlValue) bool { return v == want }

`Reached` is exported, because a monitoring loop needs exactly this question and
had no way to ask it — the diagnostic client had reimplemented the old, wrong
answer for itself. That duplicate is gone; the loop now calls `Reached`.

An intermediate version of this fix accepted *both* readings, on the reasoning
that a client cannot know a park's convention in advance. That was wrong in a way
worth recording, because it was not merely redundant: with the blade angle also
counting, a forced request for a command sharing an angle with the plant's
current state was reported as already satisfied and never sent — a forced
gradient stop at 90° on a plant standing at `Stop90` wrote nothing and returned
`InRequestedState() == true`. Tolerance that costs a wrong "yes" is not
tolerance. `Reached` is exact.

The rest follows from the corrected model:

| | before | now |
| --- | --- | --- |
| `Commandable` | 0, 1, 2 | 0 to 8 — every state a client can have caused |
| `Stopped` | 1, 2, 129, 130 | all of those plus 3 to 8 |
| `stopDepth` | 1, 129 → 60°; 2, 130 → 90° | plus 3, 7 → 60° and 4, 8 → 90° |
| forced request | the blade angle only | the value that was set, exactly |

The blade angle a command produces is still what ranks how deep a stop is —
that is the separate question an unforced `Stop` asks, and it is unchanged.

The data sheet's "only operating states with the values 0-2 can be changed"
describes the three states its own table lists; it is not a lock-out, and
reading it as one meant a client-initiated species-protection stop could never
be undone — which is what the run hit.

One consequence worth naming: the stops for ice detection and shadow flicker,
which name no blade angle, are now verifiable. Nothing can be claimed to satisfy
them in advance, so they are still always sent, but the plant reporting the value
that was set lets the monitoring loop confirm they took effect.

A test of this package's own was wrong in a way worth recording: it asserted
`strings.Contains(out, "every plant has carried out X")`, which the warning
"**not** every plant has carried out X" also satisfies. It passed whatever the
code did. The assertions are anchored on the pass marker now, and the rest of
the suite was swept for the same trap.

### An undocumented plant state is reported as one

A test run found a plant reporting `Ctrl` = **137**. Enercon documents 0, 1, 2,
121, 129, 130 and 255 for that item, so 137 is outside the set this package was
written against. The plant could not be started — correctly, because an unknown
state says nothing about where the blades are or who holds control, so neither
the target-state comparison nor the rights check can be carried out, and the
command is refused before a session is opened.

What was wrong was the *reason given*. The refusal wrapped `ErrSessionState`,
whose message reads "unexpected session state: unknown state CtrlValue(137)" —
which points an operator at session timeouts and reservation delays, the one
place where the problem is not. There is now `ErrPlantStateUnknown`, sitting
with the other plant-state errors rather than with the session errors:

	energontrol: plant reports a control state this package does not know (state CtrlValue(137))

`scadaprobe` adds what to do about it: retrying changes nothing, the raw value
has to be looked up in the data sheet for the controller type, and the value is
worth reporting because this package's value set may need extending.

The behaviour — refuse, write nothing, never report the plant as being in the
requested state — is unchanged.

### The test client offers every command

`cmd/scadaprobe` listed eleven operations and reached the gradient stops, the
stops for ice detection and shadow flicker, and the species-protection stops
only through a sub-prompt behind one generic "another documented control value"
entry. They are now menu entries of their own, so each one can be selected and
confirmed like any other command. The menu is grouped and ordered the way the
data sheet is, and every label names the value it writes — `stop for species
protection at 90° (SetCtrl 8)` — so a selection can be checked against the data
sheet without reading the source.

Two things that were unreachable from the tool are now reachable:

- **`forceExplicitCommand`** as a menu toggle. Only `false` was ever passed
  before, which is precisely the dimension the Stop60 finding above lived in.
  The confirmation screen shows how it is set for the commands it affects.
- **`ControlAndRbh`**, the combined command, which writes a control value, a
  heating value and the lamp in one session per plant. Each part is optional; a
  set that writes nothing is refused at the prompt.

`TestEveryDocumentedValueIsOnTheMenu` holds this open: it walks the documented
value sets and fails if any value Enercon defines for `SetCtrl`, `SetRbh` or
`SetIceDet` has no entry, if the control group has more or fewer than the nine
writable values, or if that group is out of data-sheet order.

The fake SCADA in the tool's tests confirmed written parameters with a constant
`{0, 0, 0}`, so the library's read-back verification of a written value was
never exercised through it. It now remembers what was written, serves it back,
and applies a submitted control value to the reported state — which is what lets
the Stop60 regression test run through the whole stack.

### Hardening after the second pre-release audit

A second independent architecture and code audit was run on the hardened v2
candidate. Its findings had a shape worth naming, because it is the shape a
test suite cannot catch: the package stated a property, documented it at
length, tested it — and the property did not hold, because the tests sat above
the layer where it broke. The fake implemented the OPC client interface
directly, so everything between a SOAP response and a Go value was unreachable
from 130 tests. All findings rated critical, high or medium are fixed below;
each fix carries a test that fails without it.

**The OPC layer is now a port of this package's own, not the client library's
interface.** `Transport` speaks item names and unsigned integers;
`opcxmlda.New(server)` adapts a `*gopcxmlda.Server` to it. The
interface it replaces, `OpcClient`, was built from `gopcxmlda`'s types and
signatures — including its `*string` out-parameters and
`map[string]interface{}` options — so every breaking change in that library was
a breaking change in energontrol's public API, and the documented promise that
"an alternative transport can be substituted" held only for something that
produced `gopcxmlda` structs. It was a test seam, not an abstraction. Three
rules now live in the adapter, in one place each, instead of being spread
through the package or missing:

**An error about one item no longer fails the whole request.** This was
the critical finding. OPC XML-DA reports an item fault in that item's
`ResultID` attribute and, since `ReturnErrorText` defaults to true — and this
package requests it explicitly — adds an `<Errors>` element carrying the
localised text. `gopcxmlda` surfaces that element as an error from `Read`, and
the package treated it as a failure of the request, discarding a response that
was complete and usable. The consequence was that the entire per-item error
path this package is built around — `ErrItemFault`, `ErrBadQuality`,
`ErrItemMissing`, the `PlantState.Err` column, the promise that "one unreadable
plant does not blind the caller to the others" — was unreachable against a
conformant server. Reading a park failed on a single bad item name; inside a
command it failed every plant of the batch and left each reserved session open
for the server's 60 s timeout, so one misconfigured item could take the park's
control out of service a minute at a time. The adapter now classifies an error
consisting of nothing but `*OpcResponseError` as item-level and keeps the
response. A transport failure, a SOAP fault, and an `<Errors>` element that no
returned item accounts for all remain failures of the request.

**A paged browse is followed to its end.** `MoreElements` and
`ContinuationPoint` were read nowhere in the package. A server that answers a
browse of `Loc/Wec` with part of the listing produced a short park — with
`err == nil` and an empty `Unsupported`, breaking the exact guarantee
`Turbines` gives in its own documentation. The same fault made a plant appear
to have no `SetCtrl` when its `Set*` listing was paged. The adapter now follows
continuation points, and reports `ErrBrowseIncomplete` for a server that
announces more elements without handing out a point, repeats one, or never
finishes.

**A response whose handle contradicts its item name is refused.**
Correlation was by `ClientItemHandle` with a fall-back to `ItemName`, but where
a server supplied both and they disagreed, the handle won silently. Two such
items are two swapped turbines — the failure positional matching was rejected
for, only harder to see. Since `ReturnItemName` is requested precisely so the
name is available, the cross-check is free; a contradiction is now
`ErrUncorrelatable`.

**A plant named only in `Name` is no longer lost.** `filterPlants`
matched the full item name only. OPC XML-DA requires `ItemName` for items but
not for branches, so a server that fills in only `Name` produced a park listing
with the plant in neither `PlantNo` nor `Unsupported` — the silent loss
`Unsupported` exists to prevent. Both forms are now recognised, duplicates are
collapsed, and the burden of proof is reversed: a node that cannot be made
sense of at all is reported, since staying silent about one requires being sure
it is not a plant.

**The polling budget has a floor.** The default was one second per state
transition, justified in a code comment as "ten attempts at 100 ms, the budget
v1 used and proven in the field". v1 polled a fixed eleven times with a sleep
between attempts, independent of latency; v2's budget is a wall-clock deadline,
and every attempt is a SOAP round trip — on a SCADA answering in 300 ms it
bought two or three. The default is now five seconds, and at least four
attempts are made regardless of the clock. The session lifetime still overrides
both: a guaranteed attempt on a session the server has dropped is not worth
guaranteeing.

**`WithLenientVerification` was three tolerances behind one name, and its godoc
described one of them.** It said it accepted an unconfirmed write and
that "for control commands it leaves the value read-back as the sole check" —
while the same flag also disabled the session id read-back *and* that very
value read-back. It is now `WithLenientWriteConfirmation` and
`WithLenientSessionVerification`, each documenting what it gives up, with
`WithLenientVerification` kept as the both-of-them convenience.

**`New` reports what it cannot use.** `New(nil)` returned a client that
panicked with a nil pointer dereference on its first command, somewhere inside
a session. `WithSessionPolling(0, 0)` and `WithSessionLifetime(-1)` discarded
their arguments without a word, so a duration misread from a configuration file
became a default nobody chose. `New` now panics immediately, with a message
naming the problem, and `NewWithOptions` returns it as an error wrapping
`ErrInvalidOption` for callers whose values come from configuration.

**Options are order-independent.** `WithSessionPolling` clamped its
timeout against whichever session lifetime happened to be set at the moment it
ran, so `WithSessionPolling(1s, 90s), WithSessionLifetime(120s)` produced a
60 s budget while the same two reversed produced 90 s. Options now only record
what was asked for; the clamping happens once in `New`, with every option
applied.

**`WithCommandTimeout` bounds a command as a whole.** The session
lifetime bounds everything from the reservation onwards, but the wait for state
"free" happens before a session exists — so a generous polling budget could be
spent waiting to start and a full lifetime spent afterwards, while the
documentation claimed the lifetime was "the ceiling on everything a command
waits for". That claim is now accurate about what it covers, and this option is
the single figure a scheduler can reason about.

**`OutcomeFailed` no longer implies that nothing was written.** If the
procedure fails only at the last step, the submit was written and confirmed:
the server holds the value and has committed it, and only the confirmation that
the session wound down is missing. Reporting a plain failure invited a retry,
and a retry of a `Start` or a `Reset` is a second command to a plant that may
already have had one — a `Stop` is protected by the 360 s re-reservation delay,
those two are not. Such a plant now also carries `ErrOutcomeUncertain`.

**The value read-back is checked against the private key where the value is
zero.** `CtrlStart`, `RbhSetStandard` and `IceDetLampOff` all write 0,
and an item that was never written reads back the same — so for those three the
read-back established nothing, and `Start` is not a rare command. The private
key is never drawn as zero, so where the server reports it back it is checked
instead. Where it does not, the write confirmation is the remaining evidence
and the gap is logged rather than counted as a check that passed.

### Data freshness is demanded, not just checked

The last open finding of the second audit, and the one that could not be fixed
from this repository until now: `gopcxmlda` v1.2.1 adds the `MaxAge` attribute,
so `WithMaxStateAge` can finally ask for what it was always meant to ask for.

Before, the option only compared the timestamp the server had chosen to send.
That detects a stale value but does not prevent one, and it depends on the
server filling `ItemTime` and on both clocks agreeing — so on a server that
answers from its cache and reports no timestamps, the option did nothing at all
beyond rejecting the reads outright.

Every read now carries the age as `MaxAge` on the request's item list, which is
where the specification puts it — not among the `RequestOptions`, which have no
such attribute. It goes on the list rather than on each item because every item
of one request carries the same requirement. The timestamp check stays as the
second line: a server that ignores the attribute is still caught.

With the option off, no `MaxAge` is sent at all. A `MaxAge` of 0 is a demand for
the most accurate data available, not the absence of one, so sending it for a
caller who never asked would silently turn every read into a device read.
Sub-millisecond ages round down to exactly that device read, and an age beyond
the `xs:int` range the attribute uses — about 24 days — is rejected as an option
error rather than as a read that fails at run time.

This is what the `Transport` port gained a `ReadOptions` argument for. A
transport that cannot express `MaxAge` degrades to detection rather than to
nothing, since the timestamp check sits above the port.

### A diagnostic client, and the bug it found

`cmd/scadaprobe` is an interactive client to point at a real SCADA. It is
configured from the environment (`OPC_URL`, `USERID`, `PARKNO`,
`ENERGONTROL_TEST_PLANTS`), reads the park and the plant states, verifies the
park number against the server, and then offers a menu: read, diagnose, change
the plant selection, or one of eleven control operations.

Nothing is written until an operation is confirmed, and the confirmation is the
operation's own name typed back rather than a `yes` — so the habit of confirming
cannot send a command nobody read. A command is offered at all only when the
park is confirmed, the plants are named explicitly, and a user id is set.

The diagnosis answers the things the library's contracts rest on: whether the
server pages its browse answers, whether it fills item timestamps, whether it
reports a faulted item per item rather than failing the request, and the
round-trip latency the session polling budget has to be sized against — which
was the one figure the library's default had been reasoned about rather than
measured.

Everything lands in `scadaprobe.log` as JSON lines: every choice, every offer
and its confirmation or abort, every OPC request with its items and values and
what came back, the library's own warnings, and the outcome per plant. That is
deliberate: a run against a real park is the evidence, and evidence has to be
readable afterwards.

Driving it against a simulated SCADA found a real defect on its first run.
**A write reply had its item values decoded as if it were a read.** An
OPC XML-DA write reply confirms items; `ReturnValuesOnReply` invites a server to
echo their values but does not oblige it to, and nothing above the port looks at
them. Decoding them anyway turned every bare confirmation into
`ErrUnexpectedType` — so against a server that confirms without echoing, which
is what an Enercon SCADA does, **every command failed** on the very first write
of the session.

Nothing in the suite could have seen it. The core's in-memory transport sits
above the decoding, and the adapter's own tests all happened to echo a value.
It took driving the whole library through real SOAP, which is what the tool
does, and it is exactly the class of gap the second audit named: a contract that
holds against the fake and not against a server. It is now pinned from both
sides — `TestAdapterWriteConfirmationNeedsNoValue` for a write that carries no
value, `TestAdapterReadStillNeedsAValue` for the read where the absence of one
is still an error.

### Package layout

The library is three packages now, and the split is the point rather than a side
effect.

```
energontrol/           the library: client, protocol, value types, errors
  opcxmlda/            the OPC XML-DA transport, on top of gopcxmlda
  cmd/scadaprobe/      a diagnostic client to point at a real SCADA
  test/                tests that command real turbines (tag opc_integration)
```

**`energontrol` no longer imports `gopcxmlda`.** The adapter moved to
`opcxmlda`, and `NewGopcxmldaTransport(server)` became `opcxmlda.New(server)`.
This is the only API change, and it is the reason for the move: the port was
already clean, but only by discipline — nothing stopped a `gopcxmlda` type from
appearing in a `Transport` signature again, which is exactly the defect the
first audit found in the interface this port replaced. A package boundary turns
that mistake into a compile error. The core cannot leak types it cannot name.

Three unexported helpers the adapter used stayed behind and were replaced by
local equivalents: `wrapf`, which is now gone from the core because nothing
there wrapped an OPC error any more; `serverStateRunning`, a one-word constant;
and `serverStateError`, for which the adapter returns a wrapped
`ErrServerNotRunning` instead — nobody inspected the concrete type, only
`errors.Is`.

**The live suite moved to `test/`.** It needs nothing unexported — it drives the
library the way a caller does — so it costs no visibility change, and it buys
three things. The tests that move real machinery are physically separated from
the ones that do not. The naming rule below holds everywhere again, since a
directory without production code has nothing to pair with. And only the
exported API is reachable from there, so the suite doubles as a standing check
that what the library exports is enough to run a park with — the question
`doc_test.go` asks against a fake, asked against real turbines.

What was *not* split: the core. `tracker`, `sessionProcedure`, `plantCommand`,
`itemNamer`, `resultSet`, `correlate` and `ctrlSatisfied` are unexported and
used across every boundary a layered split would draw, so such a split would
have to export them — widening the public surface with pure plumbing — and would
break the eleven of thirteen test files that are white-box. 4,200 lines in one
package is not a size problem; `net/http` is one package with several times
that.

### Test file layout

Every test file is now named after the production file it covers: `session.go`
is tested by `session_test.go` and nothing else. The suite had grown a set of
files named after the work that produced them rather than after the code they
exercise, which told a reader nothing about where to look for a test or where to
put a new one. Nothing was dropped in the move; the tests are the same 204
functions, redistributed.

Three files carry more than their name suggests, which is worth knowing:
`transport_test.go` holds the in-memory `Transport` the protocol tests run
against, since that is an implementation of the port `transport.go` defines;
`opcxmlda/transport_test.go` holds everything that needs real SOAP over HTTP;
and `doc_test.go` is `package energontrol_test`, the only file that sees the
package from outside, and holds the runnable example. The live suite is
`test/live_test.go`, in a package with no production code to pair with.

Comments no longer refer to findings by number. The numbering belonged to audit
documents a reader of this repository does not have, so each reference was
replaced by what it was actually pointing at.

### Transport dependency

`gopcxmlda` moves to v1.2.2, which brings two things this package depends on.

v1.2.1 added the `MaxAge` attribute, without which the freshness demand above
could not have been made at all.

v1.2.2 corrects where a Read request carries `LocaleID` and
`ClientRequestHandle`. The WSDL gives the `Read` element no attributes of its
own — only an `Options` and an `ItemList` child — and they belong on
`RequestOptions`, which has both. Putting them on `Read` was something a
strictly validating server could reject, and it meant the request handle was
never echoed in the reply. `opcxmlda/transport_test.go` now pins the element as
attributeless, so the shape cannot drift back.

One thing that is *not* a finding, and is recorded here so it does not get
re-raised as one: a response `gopcxmlda` cannot decode — a `<Value>` without the
`xsi:type` the schema requires — fails as a whole rather than per item. That is
a deliberate fail-fast contract on the transport's side, and the right one: a
reply that malformed says nothing trustworthy about any of its items, and
keeping the ones that happened to parse would mean assuming the rest of the
document is still to be believed. The boundary is pinned by
`TestAdapterUnparseableResponseIsARequestFailure` and described in the package
documentation, so it reads as a contract rather than as an oversight.

### The low-severity findings

**The item address space is configurable, and a node it cannot address is no
longer listed as though it could be.** Every item name was built from a
hardcoded "Loc" prefix, so an installation that exposes its park anywhere else
answered nothing at all: every item missing, for every plant, with no hint as to
why. `WithItemRoot` sets the root; everything below it follows the data sheet
and stays fixed. The second half of the finding was a node named `Plant007`,
which was taken as plant 7 and then commanded as `Plant7` — an item such a
server does not have. A plant node now enters the listing only if this package
would address it under the name the server itself gave it, and otherwise lands
in `Unsupported`.

**A swallowed error no longer becomes a different reason.**
`parameterItems` discarded the error from `longWord` and returned no items,
which `writeParameters` then reported as "nothing to write" — for a public key
that does not fit in a long word. It now returns `([]ItemWrite, error)`, and the
plant's result names the public key.

**A diagnostic read only happens when somebody reads the diagnosis.**
The remaining session timeout went into a log line as a call argument, and Go
evaluates those whether or not the handler keeps the record — so every
left-open session cost an extra OPC read into a discarded log, at the moment the
server was already in trouble. The call is now behind `Logger.Enabled`, and the
default logger is `slog.DiscardHandler` rather than a text handler writing to
`io.Discard`: the latter reports itself as enabled, so the guard alone would not
have helped.

**The result set copies the plant list.** `newResultSet` kept the
caller's variadic slice — `client.Start(ctx, id, plants...)` passes its own
backing array — and read the report order from it after the command had
finished. A caller reusing that slice got results attributed to plants it never
asked about. `lockPlants` already copied for the same reason.

**The linter is pinned and its rule set is checked in.** The CI ran
golangci-lint at `latest` with no configuration, so a green build had passed
whatever rules that day's release enabled by default — and the v1-to-v2
transition changed both the default set and the configuration format, which
raising the `go` directive to 1.26 forces anyway. `.golangci.yml` now pins the
rules and `ci.yml` pins v2.13.2. Beyond the standard set it enables `errorlint`,
`nilerr`, `exhaustive` and `durationcheck`: the contract of this package is its
errors, and its logic is full of timing arithmetic. Turning them on found a
`max` shadowing the builtin and two unused test helpers. The coverage floor
moves from 85 % to 88 %.

**There is a test that sees the package from outside.**
`doc_test.go` is `package energontrol_test`. It does not repeat the
protocol tests — those need to be driven from within — but asks the question
nobody was asking: is what this package exports enough to use it? It implements
a `Transport` from the exported types alone, which is a compile-time proof that
the port carries no dependency on an unexported type, and it classifies every
argument error, reads a park listing, decodes the status words, and inspects
`*SessionStateError` and `*ItemError` through `errors.As`.

### Go version

The `go` directive moves from 1.23.0 to 1.26, which raises the minimum Go
version for consumers of the module and is what makes `slog.DiscardHandler`
available for the discarding default logger described above.

The two directives do different jobs, and for a module that is a dependency of
others the Go documentation names exactly this split:

- `go 1.26` is the **minimum a consumer needs**. It carries no patch on
  purpose. It is a minimum, not a pin, so any 1.26.x or newer toolchain builds
  the module — whereas `go 1.26.7` would force every build on an older 1.26.x
  to download a matching toolchain first, consumers included.
- `toolchain go1.26.7` is what building **this** module uses, and it is ignored
  when the module is somebody's dependency. Bump it with
  `go get toolchain@go1.26.8`.

CI takes neither from go.mod. `setup-go` prefers a `toolchain` line over the
`go` line, which would pin the build to that one patch; the workflow therefore
asks for `go-version: '1.26.x'` with `check-latest: true`, so it exercises the
newest patch of the declared minor without anyone editing a file when a patch
lands. The cost is that the minor is named in two places.

The suite passes on both ends of that range: under 1.26.7 and, with
`GOTOOLCHAIN=local`, under the declared minimum 1.26.0.

### Tests

The test gap that let the item-level error escalation survive is closed.
`opcxmlda/transport_test.go` drives the real adapter against an HTTP test server with real SOAP responses,
covering what the in-memory fake sits above: item faults with and without an
`<Errors>` element, a SOAP fault alongside item errors, correlation by handle
and by name, a handle contradicting its item name, duplicated and unrequested
items, every `xsi:type` a server may choose for a number, arrays, quality and
timestamps, browse paging in its three failure modes, and `ServerState` on a
browse reply.

The fake now implements `Transport`, which is both smaller and honest about
what it proves: the protocol logic, not the wire format. The knobs that
modelled wire-level behaviour moved to the adapter tests. Statement coverage is
90.1 %, above the 85 % floor the CI enforces.

### Migration from the earlier v2 candidate

- `New`, `NewWithOptions` and every package-level command take a `Transport`
  instead of an `OpcClient`. Wrap the server:
  `energontrol.New(opcxmlda.New(server))`, importing
  `github.com/dernate/energontrol/v2/opcxmlda`.
- `SessionReleaseFunc` receives a `Transport`.
- `WithLenientVerification` still exists and still enables both tolerances;
  prefer `WithLenientWriteConfirmation` or
  `WithLenientSessionVerification`.
- `WithSessionPolling`, `WithSessionLifetime`, `WithMaxStateAge` and
  `WithCommandTimeout` reject an unusable value instead of ignoring it. `New`
  panics on one; use `NewWithOptions` to get an error.
- The default polling timeout is 5 s instead of 1 s.
- New sentinels: `ErrInvalidOption`, `ErrBrowseIncomplete`,
  `ErrOutcomeUncertain`.
- New option: `WithItemRoot`, for an installation whose address space does not
  start at "Loc".
- `Transport.Read` takes a `ReadOptions` argument, which carries the freshness
  requirement (`MaxAge`) a transport has to put on the wire.
- The module requires `gopcxmlda` v1.2.2: v1.2.1 for the `MaxAge` attribute,
  v1.2.2 for the corrected placement of `LocaleID`/`ClientRequestHandle` on a
  Read request.
- The module requires Go 1.26 or newer.

### Hardening after the pre-release audit

An independent architecture and code audit was run on the v2 candidate before
release. It found that the safety contract v2 states in its own documentation
was not upheld on several paths, in the same class as the v1 defects v2 was
written to remove: reporting success without the evidence for it. Each item
below was reproduced with a test before it was changed, and each test is in the
suite.

**An unconfirmed write counted as a write.** `writeValues` mapped an item
missing from the `WriteResponse` to "accepted" — the exact inverse of the rule
reads follow. `Reset` has no second line of defence, because the data sheet
defines no readable parameter for `SetReset`, so the write confirmation is its
only evidence: a `Reset` whose item the server never echoed was reported as
`OutcomeCommanded`. A missing item is now `ErrItemMissing`, and
`WithLenientVerification` restores the old behaviour for a server that genuinely
does not echo written items.

**Verification degraded silently to a log warning.** If the session id or a
written value could not be read back — a faulted item, a missing item, bad
quality, an unexpected type — `verifySession` logged a warning and carried on,
and the plant came back as `OutcomeCommanded` with `Err == nil`. Since the
default logger discards everything, a caller had no way to learn that the check
the documentation promises had not happened. A check that cannot be carried out
now fails the plant with `ErrSessionUnverified`. Exactly one case stays
tolerated, and it is the one that was ever meant to be: a session id read back as
zero, which means the server does not report it, because this package never
draws zero. A one-element array typed as a bare scalar is now accepted as a
valid read-back, so a server's encoding choice does not turn into an
unverifiable session.

**The polling budget did not bound the session.** `WithSessionPolling` set a
budget per state transition, and a command waits for four of them, with nothing
bounding the whole. The documentation advised keeping the timeout "below 60 s",
which invited exactly the failure it was meant to prevent: at 55 s a command
could run for over three minutes, writing `SetCtrl` and `SessionSubmit` into a
session the server had dropped after 60 s. The session lifetime is now a first
class notion: it starts with the reservation, every wait inside a session is
clamped to what is left of it, the remaining steps are not attempted once it has
run out, and the plant reports `ErrSessionExpired` — joined with the observed
`SessionStateError`, so both the cause and the state stay visible. A polling
timeout above the lifetime is clamped to it, and `WithSessionLifetime` overrides
the value for an installation that documents a different one.

**The `ServerState` of read and write responses was ignored.** OPC XML-DA carries
`ServerState` in the reply base of *every* response; the package checked it only
once per command, via `GetStatus`. That is a time-of-check to time-of-use test
across several requests: a server that degraded to `failed` mid-session kept
receiving control commands and kept having its values used as process values.
Every read and write response is now checked, and an empty state stays tolerated
because not every server fills the attribute. This also closes
`ParkNoMatch(ctx, parkNo, false)` reporting a positive park match from a
suspended server — which is how the live tests guard themselves.

**`WithMaxStateAge` failed open.** The check was skipped for an item without a
timestamp, which switched the caller's explicit freshness requirement off
precisely on the servers it guards against: one that answers from a cache is
more likely, not less, to be one that does not fill `ItemTime`. A missing
timestamp is now `ErrNoItemTime`, which wraps `ErrStaleValue` so existing
classification still works.

**One unreadable plant blinded the caller to the whole park.** `PlantCtrlState`,
`PlantRbhState` and `PlantIceDetState` returned `nil` and the first plant's error,
throwing away the per-plant errors the layer below had carefully separated. That
is the call the package's own documentation prescribes for monitoring a command,
so a single unknown item name made a park invisible at the moment its states were
needed most. They now return one entry per requested plant with the reason in
that entry's `Err`; the returned `error` is reserved for failures of the whole
request. `PlantStates.Err()` gives the all-or-nothing answer for callers who want
it.

**The user id was validated too late, per plant, with the wrong sentinel.** An id
that does not fit in a long word was rejected inside `requestSessions`, after
three reads had gone to the SCADA, as a per-plant `OutcomeFailed` with
`err == nil` at the call level, wrapping `ErrInvalidValue` ("value must not be
written by a client"). It is an argument error: it is now checked at every public
entry point before anything is sent, and wraps the new `ErrInvalidUserID`.

**The documented concurrency invariant was not enforced.** "No two goroutines may
command the same plant" appeared in three documentation sections and nowhere in
the code, although `validatePlants` rejects the same collision *within* one call
for the same stated reason. Commands are now serialised per plant inside the
`Client`, acquired in plant-number order so overlapping plant sets cannot
deadlock, cancellable through the context, and never applied to read-only calls.

**`SessionWaitLoop` (3) was treated as a failure.** The state is declared and
named but was never used: `waitState` accepted only `SessionWaitEnd`. Reaching 3
already proves the submit was accepted, because only a submit moves a session out
of parameter input, so it now counts as the end of the procedure.

**Plants the package cannot address disappeared silently.** `filterPlants`
dropped a `Loc/Wec/Plant<n>` node whose number does not fit a `uint8`, and any
plant-shaped node that does not match the scheme, without a trace — and a plant
missing from `Turbines` is never commanded and never monitored. They are now
reported in `TurbineInfo.Unsupported` and logged. The plant list is sorted, so
two runs against one park compare equal.

**`Turbines` returned a half-filled park next to an error** and issued up to
three sequential browses per plant — over a hundred round trips for a park of
forty. It is now all or nothing (a caller who overlooked the error would read
"not listed" as "has no Ctrl"), and the per-plant browses run six at a time.

**Test gaps.** `discovery.go` and `api.go` had no unit test at all, and the
`ReadErr`/`WriteErr` knobs the fake already provided were never set by any test,
so no transport-failure path was covered. Added: `discovery_test.go`,
`api_test.go`, `transport_test.go` (a failure in each phase of the procedure,
including the one that leaves a session reserved), and a test for every item
above. A GitHub Actions workflow now runs `go vet`, `gofmt`,
`staticcheck` and `go test -race` with a coverage floor.

**Documentation drift.** `README.md` referred to a constant `RbhSetHeatOn` that
does not exist; `ErrNothingRequested` documented two of the three values it
checks; the comment on `ctxErr` claimed the context is checked before every
request; `releaseSessions` described a session in `SessionWaitEnd` as "nothing is
holding the session" although the plant answers occupied for another 360 s; the
`doc.go` note on `SetRbH` value 8 ran into the Enercon security quote without a
break. All corrected, and the transport-security consequence is now stated
outright: the session mechanism is not a credential, plain HTTP carries the user
id in clear, so the endpoint belongs behind a VPN or TLS.

**Smaller items.** `joinErrors` was an alias for `errors.Join` and is gone;
`reasonText` uses `strings.TrimPrefix`; `ControlAndRbh` computed
`stateError()`/`rbhStateError()` twice per plant; `cred.requested` was set before
the argument checks, so a rejected call produced a pointless cleanup read; the
session credential map was allocated with a capacity of zero; the nil-credential
invariant in `fetchPublicKeys`, `writeParameters` and `submitSessions` is now
checked rather than relied upon, so a future reordering of the procedure surfaces
as a failed command instead of a panic; the 19-value session id space and its
residual collision probability are documented.

### API changes from the audit

| candidate | released |
| --- | --- |
| `PlantCtrlState(...) ([]PlantState, error)` | `(PlantStates, error)` — one entry per plant, `PlantState.Err` per plant |
| `PlantRbhState(...) ([]RbhState, error)` | `(RbhStates, error)` — likewise |
| `PlantIceDetState(...) ([]IceDetState, error)` | `(IceDetStates, error)` — likewise |
| — | `PlantStates`/`RbhStates`/`IceDetStates` with `Err()` and `Get(plant)` |
| — | `TurbineInfo.Unsupported` |
| — | `WithLenientVerification`, `WithSessionLifetime` |
| — | `ErrSessionUnverified`, `ErrSessionExpired`, `ErrNoItemTime`, `ErrInvalidUserID` |

## v2.0.0 — original v1 audit

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
ENERCON SCADA PDI-OPC data sheet, sections 3.4 and 4.1. The `TestSpec…` tests
pin down each of these.

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
