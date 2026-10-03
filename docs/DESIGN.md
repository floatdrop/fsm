# Design

Why the API is shaped the way it is. [`README.md`](../README.md) is the tour;
this is the reasoning behind it.

## The machine holds no state

`Fire` takes a `*S` that lives in your struct. This is the whole reason the
package exists in this shape: a state that is serialized into a protobuf
field, written to a snapshot, or replayed from a log cannot also live inside a
machine object, because the two copies drift. A `Machine` is immutable after
`New` and safe for concurrent use with no lock.

`Fire` returns the `Transition` it made, so a caller that logs or publishes
the change does not have to read the state before and after.

`Initial` is the one thing about a starting point the machine does record,
and it is configuration, not state: a machine with an initial state declared
still holds nothing at fire time. What it buys is a build-time check that
every state can be reached from the start and that the start has a way out,
and a marker in the diagram. A machine shared by aggregates that start in
different states leaves it undeclared.

## Events carry typed payloads

`fsm.Define[*recording]("finish")` can only be fired with a `*recording`, and
its guards and actions only ever see a `*recording`. There is no `...any` in
the public API and no type assertions in user code:

```go
m.Fire(ctx, &st, evRecFinish, "nope")
// compile error: cannot use "nope" (untyped string constant) as *recording value
```

This is what generic methods buy. `Fire[A any](ctx, *S, Event[A], A)` is a
method on `Machine[S]`, which knows nothing about `A` — before Go 1.27 this had
to be a package-level `fsm.Fire(m, ctx, &st, ev, arg)`, or `A` had to be erased
to `any` and checked at runtime.

## A machine is a set of rules, not a builder

`New` takes its transitions and hooks as `Rule[S]` values and returns
`(*Machine[S], error)` in one call. There is no `Build()` step to forget and no
mutable builder to hold half-finished. Because rules are ordinary values, a
shared set can be declared once and reused:

```go
shared := []fsm.Rule[state]{
    fsm.From(idle).On(evStart).To(running),
    fsm.From(running).On(evCancel).To(cancelled),
}

short := fsm.MustNew("short", shared...)
long  := fsm.MustNew("long", fsm.Rules(shared...), fsm.From(running).On(evFinish).To(done))
```

`Rules` bundles a set into one `Rule`, so a shared set sits beside the rules
that extend it without splicing slices.

`S` is inferred from the rules, so `fsm.New("recording", …)` needs no explicit
type argument.

## A transition names its source and target in separate calls

`From(a).On(ev).To(b)` rather than `On(ev, a, b)`. Two adjacent parameters of
the same state type are indistinguishable to the compiler and to a reader, and
swapping them silently reverses the edge. Splitting them into three calls means
there is nothing to swap:

```go
fsm.From(recStopped).On(evRecFinish).To(recFinished)
```

Guards and actions hang off the end of the same chain:

```go
fsm.From(recStopped).On(evRecFinish).To(recFinished).
    Guard("all chunks and tracks uploaded", uploadsSettled).
    Action(recordFinishTime)
```

The chain is what carries the payload type: `From` knows `S`, `On[A]` picks up
`A` from the event, and `Guard`/`Action` accept only callbacks matching it — a
guard written for the wrong payload is a compile error at the point it is
declared. `ToStep[S, A]` erases `A` only when `New` applies it, which is why it
can sit in the same list as transitions carrying any other payload.

There is one option type, not two. An earlier design passed guards and actions
as `Option[A]` values into `To(...)`, which meant a second options concept
alongside `Rule`, and a struct of nillable fields that failed silently: a nil
guard was dropped along with its description, so a machine read as guarded,
ran unguarded, and drew unguarded in `DOT()`. As methods they report through
`New` like every other configuration mistake, and a new modifier is a new
method rather than a new struct field.

`ToStep` mutates and returns itself, so a guard attached to a stored
transition takes effect whether or not the result is reassigned.

## Groups are a build-time expansion

Several states usually accept an event the same way, and writing that per
state duplicates a rule that has one reason to change. `FromEach(a, b, c)` is
the shorthand for the fan-in, and `Group` is the version that the machine
knows about.

`FromEach` is variadic rather than `From` growing a second parameter, for two
reasons. Go will not let a slice be spread into `From(s S, more ...S)` — the
call needs `From(xs[0], xs[1:]...)`, which panics on an empty slice and makes
the group-as-a-value idiom unusable. And `From(a, b)` puts two adjacent
parameters of the same state type back into the API, which is the one thing
the `From`/`On`/`To` split exists to prevent: `From(a, b).On(ev).To(b)`,
written by someone who read the pair as source-and-target, type-checks and
builds, silently adding `b --ev--> b` as an external self-transition that dips
any gauge on `b` and empties the terminal set.

A `Group` differs from `FromEach` in one way that matters: **a member that
declares the event itself overrides the inherited edge.** That is what turns
an enumeration into a hierarchy-like thing. It also means membership is
declared in one place, so a new member inherits every group transition instead
of needing each enumerating site found and edited.

The expansion happens before `New` returns, so `Fire` is untouched — a group
is not a state, never appears in `States()`, and a `*S` never holds one. This
is why groups cost nothing: `BenchmarkFire` and `BenchmarkFireWithHooks` are
unchanged by their existence.

Because the override rule needs the whole explicit table before it can know
what to skip, group expansion is a deferred pass — and it must run *before*
the `OnEnterWith`/`OnExitWith` pass, which reads the finished table. So there are two phases,
not one: expansion adds edges, then the rules that read edges run.

### What groups deliberately are not

A group has no entry or exit hooks. The reason to want them is exactly the
thing a flat expansion cannot reproduce: in a real hierarchy, moving between
two substates of the same superstate does *not* run the superstate's hooks.
With every row flat there is nowhere to record that, so a counter of what is in
the group would decrement and increment on a move a hierarchy would treat as
staying put — the same dip as a self-transition, in a counter whose whole
purpose is not to drift.

The usual demand for it dissolves on inspection. "How many participants are
live" is the sum of the per-state gauges, computed where the counters are
read; `sum by (state)` in a metrics backend needs no group at all, and has no
dip to observe. Adding exact suppression means precomputing the ordered hook
sequence per edge and restructuring `Machine` around a single
`map[edge[S]]*plan[S]` — worth doing if a superstate ever genuinely needs its
own hook, and not before.

Groups do not nest, and entering one does not select an initial member.
Overlapping groups are allowed, but two groups claiming the same event for one
state is an error rather than a silent most-specific-wins, because with no
nesting there is no specificity to appeal to.

### The diagram is where a group can still lie

`DOT` draws one arrow from a cluster boundary rather than one per member, and
skips the sibling rows when it does. That makes a wrong decision worse than
cosmetic: Graphviz refuses an `ltail` it cannot honour and the skipped rows
disappear, so the picture shows a transition table that does not exist. Three
cases therefore fall back to per-member arrows — a member that overrode the
event, a group that lost members to an overlapping cluster, and a target that
is itself a member.

The count behind that decision is keyed on the trigger's identity, not its
name. Keying on the name merges two same-named events into one group
transition, and the arrow it collapses to covers neither of them. `Edge` keeps
an unexported trigger and exposes `Edge.Is` for the same reason
`Transition.Is` exists.

## States and events are named so they cannot be confused

Events take an `ev` prefix; states take the plain domain prefix. Without that
rule a machine ends up with `recStop` the event next to `recStopped` the state,
and `pcpReconnect` next to `pcpReconnecting` — one tense apart, which is not a
difference worth relying on when reading a transition table.

## Nothing panics at fire time

Configuration mistakes come back from `New`; fire-time failures come back from
`Fire` as `*NoTransitionError`, `*GuardError`, `*ActionError` or
`*StateChangedError`, each carrying the edge it happened on. `MustNew` panics,
but only at construction, so a bad definition fails at process start. This
matters when a machine is driven by a replicated log: a panic on a malformed
event takes down every replica replaying it, not just one.

`StateChangedError` covers the one way user code can reach past the API: the
payload is usually the aggregate that holds the state, so a guard or action can
write the field `Fire` is working on. `Fire` reads the state once, and if a
guard or action changed it, assigns nothing, runs no hook and reports it,
rather than overwriting the write and running the hooks of an edge that was
never taken. Writing the target is no exception: a nested fire that landed
there would otherwise run every hook twice. The write is reported even when
the callback also failed, since a `GuardError` or `ActionError` would tell the
caller the state was untouched. The callback's error is kept in `Err` but
not unwrapped, so a sentinel meaning "retry later" does not match a state
that has moved.

## Guards reject with an error, not a bool

A refusal usually has a reason the caller needs to act on — retry later, or
give up. The guard's error is wrapped in a `*GuardError[S]`, which unwraps to
it, so both the structure and the reason are available:

```go
_, err := recordingFSM.Fire(ctx, &r.state, evRecFinish, r)

errors.Is(err, ErrUploadPending)   // the guard's own reason — retry later

ge, ok := errors.AsType[*fsm.GuardError[recState]](err)
// ge.Guard, ge.From, ge.To, ge.Event
```

The `desc` passed to `Guard` is the *static* condition, used to label the
edge in `DOT()` output and named in the error message. The returned error is
the *dynamic* reason the condition did not hold this time. `Check` returns the
same error without firing, when you want the reason but not the transition;
`Can` is `Check(...) == nil`.

When several guards are registered on one transition they run in order and the
first rejection wins, reported under the description it was declared with.

A refusal is not always news. A controller that fires `reconcile` on every turn
and lets the guards say whether there is anything to do would pay for an error
value on every quiet turn, and would have to tell refusals from failures by
type. `TryFire` reports a missing transition or a rejecting guard as `false`
with a nil error, and allocates nothing; the failures that still are errors are
the ones that mean something went wrong: an action that failed, a callback that
wrote the state.

## Entry/exit hooks cannot fail

They exist for bookkeeping that must stay paired with the state change. Work
that can fail goes in `Action`, which runs *before* the state changes and
aborts the transition on error. The ordering is fixed:

```
lookup → guards → action → exit(from) → *st = to → transition hooks → enter(to)
```

If the lookup, a guard, or the action fails, the state is untouched and no hook
runs. `OnTransition` hooks run for every edge, just after the assignment; they
are where an audit log or a trace goes, instead of one `OnEnter` per state
that a new state would silently escape. They run before the entry hooks so
that an entry hook which fires the machine again is logged after the
transition that caused it, which is also why a transition hook must not fire
the machine itself: the new state is not yet fully entered.

A counter of "how many things are currently in state *s*" is the canonical
case. By hand it is a `+= 1` and a `-= 1` at every call site that changes the
state, and it drifts the first time a site is missed. As an `OnEnter` and an
`OnExit` on the state it cannot: neither hook can fail, and neither runs unless
the assignment happens. A `Gauge(s, inc, dec)` declaring the pair in one call
would add nothing the two hooks do not already guarantee, so there is none.

### Hooks that need the payload name their event

A plain `Hook` gets a `Transition[S]` and no payload, because a state can be
entered by events carrying different types — there is no single `A` to hand
it. Naming the event fixes that:

```go
fsm.OnEnterVia(pcpDeleted, evPcpKick, func(_ context.Context, tr fsm.Transition[pcpState], d disconnect) {
    log.Info("participant kicked", "reason", d.reason)
})
```

`OnEnterVia` and `OnExitVia` run only when that event is what caused the
transition, and they are typed in its payload. Without them the only place to
see the payload is an `Action`, which has to be repeated on every incoming
edge and runs before the state changes — exactly the duplication hooks exist
to remove.

### Hooks that need the payload of every event

A counter labelled by the thing the transition is *about* — a per-instance
metric, labelled by a tenant or a service — needs the payload on every edge
into and out of the state, not on one event. Listing each with `OnEnterVia` is
the call-site drift again: a new event entering the state silently skips the
count. `OnEnterWith` and `OnExitWith` take no event:

```go
fsm.OnEnterWith(jobRunning, func(_ context.Context, _ fsm.Transition[jobState], j *job) {
    running.WithLabelValues(j.tenant).Inc()
})
fsm.OnExitWith(jobRunning, func(_ context.Context, _ fsm.Transition[jobState], j *job) {
    running.WithLabelValues(j.tenant).Dec()
})
```

Each expands to a Via hook on every event entering (or leaving) the state,
read from the finished transition table in a second pass — so group-inherited
edges count, and the hook can be declared before the edges it applies to. That
requires every such event to carry the hook's payload type. Rather than let a
mismatched event silently skip the hook, `New` reports it:

```
OnEnterWith for state running: transition done --other--> running carries a
different payload type, so the hook would miss it
```

A machine whose events carry different payloads can still use them on the
states where they agree, and use `OnEnterVia` per event where they do not. A
With hook no transition can trigger is rejected like a Via hook, so a terminal
state takes `OnEnterWith` alone: the `OnExitWith` half could never run.

### Ordering and cost

Plain hooks of a state run first, then its Via hooks, then its With hooks,
which attach last because they wait for the finished table. None of them costs
anything on a machine that declares none: a single flag set at construction skips the whole
hook block, including the lookups the plain hooks would do.

### Branch on the trigger, not on its name

`Transition.Event()` is the trigger's name, for logging. Names are not unique —
two events declared with the same name are different triggers — so a hook that
needs to know what fired should ask:

```go
if tr.Is(evPcpKick) { … }
```

### Self-transitions run exit and entry

`From(a).On(ev).To(a)` is allowed, and is UML's *external* self-transition: it
runs the exit hooks, assigns, then runs the entry hooks. A counter kept by those
hooks therefore dips to zero and comes back. There is no internal transition
that skips the hooks; an `Action` covers that case.

## Fire does not allocate

Guards and actions are combined at build time, where the payload type is still
known, and stored as a concrete `func(context.Context, A) error`. `Fire`
asserts to that type and calls it with the payload directly, so nothing is
boxed. See [Benchmark](../README.md#benchmark).

## Limitations

- **Generic methods do not satisfy interfaces.** `Machine[S]` cannot be hidden
  behind an `interface{ Fire(...) }`, because a method with its own type
  parameter never matches an interface method:
  ```
  have M[A any](A) A
  want M(int) int
  ```
  In practice a machine is a package-level variable, not a dependency you
  inject, so this has not been a problem — but it rules out mocking the machine
  itself. Test against the states instead.
- **Flat states only.** [`Group`](#groups-are-a-build-time-expansion) covers
  shared transitions and per-member overrides, but not the rest of a
  hierarchy: no group entry/exit hooks or gauges, no nesting, no initial
  transition into a group, no orthogonal regions.
- **No built-in async.** No trigger queue, no run-to-completion mode. `Fire` is
  synchronous: if your state is already serialized behind a queue or a mutex,
  a second one inside the machine is pure overhead. A nested fire on the same
  state from a guard or action is caught as `*StateChangedError`, because it
  leaves the state somewhere the outer transition did not expect. One from an
  exit hook is not caught: the outer assignment overwrites it.
- **`S` must be `comparable`.** Integer-backed enums are the intended shape;
  that keeps states usable as protobuf fields.
- **Guards must be pure.** They are also evaluated by `Check` and `Can`.
