# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

`github.com/floatdrop/fsm` is a finite state machine for Go 1.27+ built on
generic methods, with no dependency outside the standard library.
[`README.md`](README.md) is the model — what the API is and why it is shaped
that way. This file is the working detail behind it; the two are edited
together.

Library: `fsm.go` (package doc, `Event`, `Machine`, `Fire`, `Send`, `Check`,
`Can`, `To`, the four error types), `rules.go` (`New`/`MustNew`, the `Rule`
interface and everything that produces one — the `From`/`FromEach`/`FromGroup`
chain through `On`/`To` with its `Guard`/`Action` methods, plus `Rules`,
`Group`, `Gauge`, `GaugeWith`, `OnEnter`, `OnExit`, `OnEnterVia`, `OnExitVia`,
`OnTransition`, `Initial`), `introspect.go` (`States`, `Events`, `Edges`,
`Groups`, `Initial`, `Terminals`, `Unreachable`, `DOT`). Tests are
`fsm_test.go` and the worked machines in `example_test.go`.

## Working rules

- **Target modern Go.** Invoke the `modern-go-guidelines:use-modern-go` skill
  before writing Go here, and follow it: prefer `slices`, `maps`, `cmp`,
  range-over-func, and the rest of what Go 1.27 offers over legacy patterns.
  (If the skill is not listed, install it:
  `/plugin install modern-go-guidelines@goland-claude-marketplace`.)
- **Never reimplement the standard library.** No hand-rolled `max`/`min`,
  `slices.Contains`, `slices.SortFunc`, `maps.Keys`, `cmp.Or`, `errors.Join`,
  `sync.OnceValue`. If a builtin or stdlib function does it, call it.
- **Comments are short.** State what the code does or which invariant it
  carries, in one line where possible. Do not narrate the change, justify the
  edit, compare with the previous version, or record history — the deep
  rationale belongs in `README.md` and in commit messages.
- **Review every change before reporting it done.** Run the full gate below,
  then run the `code-review` skill over the diff and act on its findings. A
  change is not finished until both pass.

## Commands

```sh
go test -race -count=1 ./...                    # the suite; always run with -race
go test -race -run '^TestGuardBlocksTransition$' .   # one test
go test -count=1 -run TestFireDoesNotAllocate . # the allocation gate, without -race
go test -run '^$' -bench . -benchmem ./...      # benchmarks
go test -run '^$' -bench Fire -benchmem -count=6 ./...  # before quoting a number
go vet ./... && golangci-lint run ./...         # lint (config in .golangci.yml)
test -z "$(gofmt -l .)"                         # formatting gate
go test -cover ./...                            # coverage
```

The full gate, as one chain, the way CI runs it — run this before committing:

```sh
test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... \
  && golangci-lint run ./... \
  && go test -count=1 -run TestFireDoesNotAllocate ./...
```

## Architecture

The package is small, but four things only make sense together.

**The machine holds no state.** `Fire` takes a `*S` the caller owns; a
`Machine` is immutable after `Build` and needs no lock. This is the whole
reason the API is shaped this way: a state serialized into a protobuf field,
written to a snapshot or replayed from a log cannot also live inside a machine
object, because the two copies drift. Do not add a `Machine.State()` — if a
state seems to need to live in the machine, the machine is being asked to be
the aggregate.

**Type erasure is one-directional, and that is what keeps `Fire`
allocation-free.** Guards and actions are stored in `map[edge[S]]any` holding a
*concrete* `func(context.Context, A) ...`. An edge is only ever written by
`On[A]` and only ever read by a `Fire[A]`/`Check[A]` whose `A` came from the
same `Event[A]`, so the assertion is safe by construction and the payload is
never boxed — `A` is known at the call site. Any new way to register or fire
must keep that pairing, or the assertion becomes a runtime panic on a path that
is supposed to return errors. `TestFireDoesNotAllocate` is the guard on the
allocation half; it runs without `-race`, which changes the profile.

**Multiple guards and actions are combined at `On` time, not at `Fire` time,**
for the same reason: the combining closure is built where `A` is still known.
Guards run in registration order and the first rejection wins, reported under
the description it was declared with.

**The order inside `Fire` is fixed** — lookup, guards, action, the
state-changed check, `OnExit(from)`, assign, `OnTransition`, `OnEnter(to)`.
`OnTransition` precedes the entry hooks so an entry hook that fires again is
logged after its cause (`TestOnTransitionKeepsCausalOrder`); the price is that
a transition hook is an observer and must not fire, or the new state's exit
would run before its entry. Do not move it after the entry hooks to lift that
rule without giving up the ordering.
A failure anywhere before the assignment leaves the state untouched and runs
no hook, which is what makes `Gauge` safe: hooks cannot fail, so an increment
and its decrement cannot come apart. Changing this order breaks
`TestHooksBracketTheAssignment` and `TestGaugeStaysPaired`, which is the point
of both. `Fire` reads `*st` once, before any callback: the payload is usually
the aggregate holding the state, and a guard or action that writes it is
reported as `*StateChangedError` with nothing assigned. Writing `to` is not
exempt: a nested fire landing there would run every hook twice. The check
runs after the guards and again after the action, and always before the exit
hooks, because after an exit hook has run an error would leave a gauge
decremented with no assignment (`TestActionWritingTheStateIsAnError`).
`Fire` returns the `Transition` it
made. `Transition` holds only the two states and the trigger, and `Event()`
is a method: an `Event string` field made the result two words wider and cost
`BenchmarkFire` six nanoseconds a round trip, so do not add fields to it
without measuring.

**`Initial` is configuration, not state.** It records where a fresh instance
starts so that `New` can reject an unreachable state or a dead start, and so
`DOT` can draw the `__start` point. It must not grow into a `Machine.State()`;
see the first paragraph. The check runs in `b.deferred`, after group
expansion, so inherited edges count as reachability, and is skipped when the
definition already has errors, since a mistyped group member is a state that
exists only because of the mistake (`TestInitialDoesNotReportAMistakeTwice`).

**`builder` has two deferred phases, and the order is load-bearing.**
`b.expand` runs group transitions, which *add* edges; `b.deferred` runs the
rules that *read* the finished table, `GaugeWith` and `Initial`. Expansion must go
first or a gauge silently misses every edge a group contributed
(`TestGaugeWithSeesGroupInheritedEdges`). They cannot be one slice: `New`
ranges over it, and Go evaluates a range expression once, so work appended
during the loop is never visited. Group member validation sits between the
first pass and expansion, while `b.seen` still holds only explicitly declared
states — that is what makes a mistyped member an error instead of a state
that quietly gets edges of its own. A group transition's *target* is declared
eagerly in the first pass for the same reason.

**A group member that declares the event itself overrides the inherited edge,
and that is the whole point of `Group` over `FromEach`.** Expansion skips such
a member; it does not report a duplicate. Two *groups* claiming the same
(state, event) is a different thing and is an error — `b.inherited` records
which group owns each inherited row so the two cases can be told apart. With
no nesting there is no specificity rule to fall back on.

**Groups are not states and `Fire` knows nothing about them.** They expand to
ordinary rows before `New` returns. Do not add group entry/exit hooks or a
group `Gauge`: the only reason to want them is superstate hook suppression,
which a flat table cannot express, and a group gauge would dip on an
intra-group move. `docs/DESIGN.md` has the argument and the sum-the-members
answer. `TestGroupFireDoesNotAllocate` pins that the expansion produces rows
indistinguishable from hand-written ones.

**`ToStep.combine` builds the guard/action closures once and every expanded
row shares them.** This is what keeps the one-directional type erasure intact
across expansion — the closure is still built where `A` is known, and a nil
result means "store nothing", since a nil `func` boxed into `any` is not a nil
interface.

**`row[S]` is a named struct because `register` would otherwise take three
states and three `any` values positionally.** That is the same argument as
`From`/`To`: transposable parameters of the same type do not belong in this
package.

**`DOT` collapses a group edge to the cluster boundary only when all three
conditions in `collapsible` hold.** `ltail` plus `compound=true` draws one
arrow from the cluster and skips the sibling rows, so a wrong `yes` does not
just mislabel — Graphviz drops the `ltail` with a warning and the skipped rows
vanish from the diagram. The three: every member inherited it (otherwise the
arrow claims to cover the member that overrode it); the group holds all its
members in its own cluster (an overlapping group loses members to whichever
cluster is emitted first, and a cluster cannot be the tail of an edge whose
tail node is elsewhere); and the target is not itself a member (that is a
self-loop out of its own cluster, which Graphviz also refuses). Each has a
regression test, and the outputs were checked against real `dot`.

**The collapse is keyed on `(group, *eventDef)`, never on the event name.**
Two events can share a name, so counting rows by name merges two group
transitions and can collapse an arrow that covers neither. `Edge` therefore
carries an unexported `trigger`, with `Edge.Is` as the accessor, exactly
mirroring `Transition.Event()`/`Transition.trigger`/`Transition.Is`.
`compound=true` is only emitted when the machine has groups, which keeps the
existing `ExampleMachine_DOT` output byte-identical.

**A broken group reports one error, not a cascade.** `declareGroup` returns a
bool and `expandGroup` bails on false, so an unnamed group, an empty one, or
one listing a member twice does not also get reported as "every member
overrides it". Same for a member lost to another group: the clash is reported
and the `inherited == 0` branch is suppressed, because that member did not
override anything. `TestGroupWithNoMembersReportsOneError` and
`TestGroupClashDoesNotAlsoReportUnreachable` are the guards. The deferred
Via-hook check likewise stays quiet when `b.broken` — errors existed before the
deferred phase — since the broken rule may be what dropped the row
(`TestBrokenGroupDoesNotAlsoReportTheViaHook`). Duplicate members
are rejected rather than deduplicated — silently accepting them inflates the
group size so no boundary arrow can ever be drawn.

**`GaugeWith` expands in a second pass and validates payload types.** It is
declared per state but needs the whole transition table, so it defers through
`builder.deferred` and runs after every rule has applied — which is why
declaration order does not matter. It compares payload types with
`payloadToken[A]()`, two typed nil pointers being equal exactly when the types
match, so no reflect is involved. A state reachable by events of differing
payload types is an error, never a silently skipped edge: that silence would
be the drift the gauge exists to prevent. Hooks are keyed by `(s, event)`,
which several rows share when sources fan in — every group transition does —
so each key gets one hook, not one per row, or a single entry counts N times
(`TestGaugeWithCountsFanInOnce`).

**A plain `Hook` cannot see the payload, and that is structural.** A state can
be entered by events carrying different `A`, so there is no single type to
hand it. `OnEnterVia`/`OnExitVia` name the event, which fixes `A`; that is the
only way to get a typed hook, so do not widen `Hook` to carry `any`. They run
after the plain hooks of the same state. `New` rejects one no transition on its
event can trigger, the same way `GaugeWith` rejects an isolated state.

**`Transition.Event()` and `Edge.Event()` are names, for display; `.Is` is
identity.** Two events can share a name, so anything branching on the trigger
uses `Is`. `Machine.Events()` is names too, deduplicated, for the same reason. Do not add logic keyed on the string — this package broke that rule
once already, in `DOT`'s group-edge collapse, and produced an arrow that
described a machine nobody had declared.

**A machine with no hooks must not pay for hooks.** `Machine.hasHooks` is set
at construction and short-circuits the whole block in `Fire`, including the
map lookups the plain hooks would do — it is why the hookless path is faster
than it was before payload hooks existed. `BenchmarkFire` and
`BenchmarkFireWithHooks` are the pair that keeps this honest; measure both
before and after touching the fire path.

**Self-transitions are UML's external kind.** `From(a).On(ev).To(a)` runs
exit then entry, so a `Gauge` dips and returns. Pinned by
`TestSelfTransitionRunsExitAndEntry`; do not turn it into an internal
transition that skips the hooks.

**Declaration order is the output order.** `Machine.states` and `Machine.edges`
are slices kept alongside the maps purely so `States`, `Edges`, `Terminals` and
`DOT` never iterate a map. `S` is only `comparable`, not ordered, so there is
nothing to sort by; first-seen order is the stable answer. `DOT` output is
committed-and-diffable only as long as that holds.

## Repo conventions

- **Errors, not panics, once the machine is built.** Configuration mistakes come
  back from `New`; fire-time failures come back from `Fire` as
  `*NoTransitionError`, `*GuardError`, `*ActionError` or `*StateChangedError`,
  every one carrying the edge. `MustNew` panics, but only at construction, so
  a bad definition fails at process start. The motivating caller drives a
  machine from a replicated log, where a panic on a malformed event takes down
  every replica replaying it rather than one host. Do not introduce a panic
  reachable from `Fire`. `Event` carries a zero-size `[0]*A` field for that
  reason: without it two instantiations convert to each other and the
  concrete-func assertion in `Fire` panics.
- **`GuardError` unwraps to the guard's error**, so a guard can reject with a
  sentinel the caller matches with `errors.Is`. `Guard`'s `desc` is the
  *static* condition — it labels the edge in `DOT` and names the guard in the
  message — and the returned error is the *dynamic* reason it did not hold this
  time. Keep both; they are not redundant.
- **Examples are `Example` functions with verified `Output`.** `example_test.go`
  carries the two machines the design was drawn from, a recording lifecycle and
  a participant connection. They are tests, not prose: changing the API means
  changing them, and `ExampleMachine_DOT` pins the exact rendering.
- **Events are prefixed `ev`; states take the plain domain prefix.** The rule
  exists because the natural names collide by tense — `recStop` the event
  beside `recStopped` the state, `pcpReconnect` beside `pcpReconnecting`. A
  reader scanning a transition table cannot be asked to tell those apart.
- **A transition is declared as `fsm.From(a).On(ev).To(b)`**, never as one
  call taking both states. Two adjacent parameters of the same state type are
  indistinguishable and a swap silently reverses the edge. Bulk sources are
  `FromEach(a, b, c)` and not `From(s S, more ...S)`, which keeps that rule:
  sources live in one call, the target in another. The variadic form is also
  unusable in practice — Go rejects `From(xs...)` when a fixed parameter
  precedes the variadic one, so a computed source set could not be spread.
- **Inline multi-line guards and actions are fine now.** They format correctly
  inside `New`'s argument list; it was the old method chain that made `gofmt`
  dedent them. Named callbacks in `example_test.go` (`markStopped`,
  `uploadsSettled`) are a readability choice, not a workaround.
- **There is one option type: `Rule[S]`.** Guards and actions are methods on
  the transition (`.To(b).Guard(…).Action(…)`), not a second `Option[A]` value
  type. That was tried and removed: a struct of nillable fields failed
  silently, dropping a nil guard *and its description*, so the machine read as
  guarded and drew unguarded in `DOT`. As methods they can report through
  `New`. Add a new modifier as a method on `ToStep`, never as a new argument
  to `To`.
- **Tests take their context from `t.Context()`, benchmarks from `b.Context()`.**
  The two `context.Background()` calls left in `example_test.go` are not an
  oversight: an `Example` function has no `testing.TB`, so there is nothing to
  take a context from.
- **Benchmark numbers in `README.md` are measured, not estimated.** Re-run with
  `-count=6` before changing one; single runs vary by ~10% on this machine and
  a number quoted from one is noise.
- **No dependencies.** The module requires nothing. Keep it that way — the
  package is meant to be vendorable into a monorepo without pulling a tree in.

## Tooling caveats

- **Generic methods need Go 1.27.** Go 1.26 rejects the code outright with
  `syntax error: method must have no type parameters`. `go.mod` says `go 1.27`
  for that reason, not as a preference.
- **Generic methods do not satisfy interfaces.** `Machine[S]` cannot be hidden
  behind an `interface{ Fire(...) }` — a method with its own type parameter
  never matches an interface method (`have M[A any](A) A / want M(int) int`).
  This rules out mocking the machine; test against the states instead.
- **gopls needs v0.23.0+**, and even then a `go list` timeout makes it report a
  cascade of phantom `undefined: Machine` / `undefined: Edge` errors across
  every file at once. That shape — everything undefined, plus
  `initialization failed: context deadline exceeded` — is a stale language
  server, not a broken tree. Confirm with `go build ./...` before chasing it.
- **golangci-lint v2.13.1+** parses generic methods; older releases do not.
  `.golangci.yml` carries no staticcheck exclusions — if a future version
  crashes rather than reports, check upstream before contorting code around it.
- **revive's `unused-parameter` is excluded for `_test.go`.** Guards, actions
  and hooks have signatures fixed by the API, so a callback ignoring its
  context or payload can only rename the parameter to `_`, and the name is
  often what documents what it would have received. The exclusion is
  load-bearing: it fires the moment a test names an unused parameter.
