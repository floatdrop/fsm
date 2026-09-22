# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

`github.com/floatdrop/fsm` is a finite state machine for Go 1.27+ built on
generic methods, with no dependency outside the standard library.
[`README.md`](README.md) is the model — what the API is and why it is shaped
that way. This file is the working detail behind it; the two are edited
together.

Library: `fsm.go` (package doc, `Event`, `Machine`, `Fire`, `Send`, `Check`,
`Can`, `To`, the error types), `rules.go` (`New`/`MustNew`, the `Rule` type
and everything that produces one — the `From`/`On`/`To` chain, `Gauge`,
`OnEnter`, `OnExit` — plus the `WithGuard`/`WithAction` options),
`introspect.go` (`States`,
`Edges`, `Terminals`, `Unreachable`, `DOT`). Tests are `fsm_test.go` and the
worked machines in `example_test.go`.

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

**The order inside `Fire` is fixed** — lookup, guards, action, `OnExit(from)`,
assign, `OnEnter(to)`. A failure anywhere before the assignment leaves the
state untouched and runs no hook, which is what makes `Gauge` safe: hooks
cannot fail, so an increment and its decrement cannot come apart. Changing this
order breaks `TestHooksBracketTheAssignment` and `TestGaugeStaysPaired`, which
is the point of both.

**Declaration order is the output order.** `Machine.states` and `Machine.edges`
are slices kept alongside the maps purely so `States`, `Edges`, `Terminals` and
`DOT` never iterate a map. `S` is only `comparable`, not ordered, so there is
nothing to sort by; first-seen order is the stable answer. `DOT` output is
committed-and-diffable only as long as that holds.

## Repo conventions

- **Errors, not panics, once the machine is built.** Configuration mistakes come
  back from `Build`; unknown transitions and rejected guards come back from
  `Fire` as `*NoTransitionError` / `*GuardError`. `MustNew` panics, but only
  at construction, so a bad definition fails at process start. The motivating
  caller drives a machine from a replicated log, where a panic on a malformed
  event takes down every replica replaying it rather than one host. Do not
  introduce a panic reachable from `Fire`.
- **`GuardError` unwraps to the guard's error**, so a guard can reject with a
  sentinel the caller matches with `errors.Is`. `WithGuard`'s `desc` is the
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
  indistinguishable and a swap silently reverses the edge. If a shorthand for
  bulk declarations is ever added, it must keep the roles distinguishable in
  the same way — a `[]Edge{{From: …, To: …}}` literal qualifies, a positional
  pair does not.
- **Inline multi-line guards and actions are fine now.** They format correctly
  inside `New`'s argument list; it was the old method chain that made `gofmt`
  dedent them. Named callbacks in `example_test.go` (`markStopped`,
  `uploadsSettled`) are a readability choice, not a workaround.
- **`Rule[S]` and `Option[A]` are different things** and the names have to
  keep them apart: a `Rule` declares part of a machine and goes to `New`; an
  `Option` configures one transition and goes to `To`. This is the one real
  cost of taking rules as arguments instead of chaining a builder, so do not
  blur it by naming a new constructor ambiguously.
- **Tests take their context from `t.Context()`, benchmarks from `b.Context()`.**
  The two `context.Background()` calls left in `example_test.go` are not an
  oversight: an `Example` function has no `testing.TB`, so there is nothing to
  take a context from.
- **Benchmark numbers in `README.md` are measured, not estimated.** Re-run with
  `-count=6` before changing one; single runs vary by ~10% on this machine and
  a number quoted from one is noise.
- **No dependencies.** The module requires nothing. Keep it that way — the
  package is meant to be vendorable into a monorepo without pulling a tree in.
- **`docs/assets/logo.svg` is traced, not drawn.** It came from a painted PNG
  via vtracer: crop to content, median-filter away the brush texture, map
  every pixel to the four real colours (`#FBF5E8` cream, `#378ECF` blue,
  `#393B57` pupils, `#EFDFBE` teeth), then trace bodies, enclosed eye whites,
  teeth and pupils as four binary layers over a cream rectangle.
- **The logo keeps its cream canvas on purpose; do not "fix" it to be
  transparent.** Two of the six eye whites are *open to the background* — the
  head outline does not close around them — so on the cream canvas they are
  white only because the canvas is. Drop the canvas and those two eyes lose
  their fill and read as bare dark pupils, which is exactly the bug that was
  reported once already. Reconstructing them (grow cream from the pupil, clip
  to a morphologically closed head) was tried and leaves visible lobes
  bulging past the silhouette on a dark background. The teeth are a separate
  colour rather than a shape, which is what keeps all four sets: one gopher's
  teeth connect to the cream inside the ring of arms and would otherwise be
  dropped with it.
- Edit the SVG directly; there is no committed script, and re-tracing from the
  PNG would not reproduce it byte for byte.

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
