# Design

Why the API is shaped the way it is. [`README.md`](../README.md) is the tour;
this is the reasoning behind it.

## The machine holds no state

`Fire` takes a `*S` that lives in your struct. This is the whole reason the
package exists in this shape: a state that is serialized into a protobuf
field, written to a snapshot, or replayed from a log cannot also live inside a
machine object, because the two copies drift. A `Machine` is immutable after
`New` and safe for concurrent use with no lock.

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

`New` takes its transitions, gauges and hooks as `Rule[S]` values and returns
`(*Machine[S], error)` in one call. There is no `Build()` step to forget and no
mutable builder to hold half-finished. Because rules are ordinary values, a
shared set can be declared once and reused:

```go
shared := []fsm.Rule[state]{
    fsm.From(idle).On(evStart).To(running),
    fsm.From(running).On(evCancel).To(cancelled),
}

short := fsm.MustNew("short", shared...)
long  := fsm.MustNew("long", slices.Concat(shared, []fsm.Rule[state]{
    fsm.From(running).On(evFinish).To(done),
})...)
```

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

The chain is also what carries the payload type: `From` knows `S`, `On[A]`
picks up `A` from the event, and `To` takes options that must match it. A
`WithGuard` written for the wrong payload is a compile error at the point it is
declared. `To` erases `A` only after that check, which is why a `Rule[S]` can
sit in the same list as transitions carrying any other payload.

Two things are called options-ish, and they are not the same: a **`Rule[S]`**
declares part of a machine and goes to `New`; an **`Option[A]`** configures one
transition and goes to `To`.

## States and events are named so they cannot be confused

Events take an `ev` prefix; states take the plain domain prefix. Without that
rule a machine ends up with `recStop` the event next to `recStopped` the state,
and `pcpReconnect` next to `pcpReconnecting` — one tense apart, which is not a
difference worth relying on when reading a transition table.

## Nothing panics at fire time

Configuration mistakes come back from `New`; unknown transitions come back from
`Fire` as `*NoTransitionError`. `MustNew` panics, but only at construction, so a
bad definition fails at process start. This matters when a machine is driven by
a replicated log: a panic on a malformed event takes down every replica
replaying it, not just one.

## Guards reject with an error, not a bool

A refusal usually has a reason the caller needs to act on — retry later, or
give up. The guard's error is wrapped in a `*GuardError[S]`, which unwraps to
it, so both the structure and the reason are available:

```go
err := recordingFSM.Fire(ctx, &r.state, evRecFinish, r)

errors.Is(err, ErrUploadPending)   // the guard's own reason — retry later

ge, ok := errors.AsType[*fsm.GuardError[recState]](err)
// ge.Guard, ge.From, ge.To, ge.Event
```

The `desc` passed to `WithGuard` is the *static* condition, used to label the
edge in `DOT()` output and named in the error message. The returned error is
the *dynamic* reason the condition did not hold this time. `Check` returns the
same error without firing, when you want the reason but not the transition;
`Can` is `Check(...) == nil`.

When several guards are registered on one transition they run in order and the
first rejection wins, reported under the description it was declared with.

## Entry/exit hooks cannot fail

They exist for bookkeeping that must stay paired with the state change. Work
that can fail goes in `WithAction`, which runs *before* the state changes and
aborts the transition on error. The ordering is fixed:

```
lookup → guards → action → exit(from) → *st = to → enter(to)
```

If the lookup, a guard, or the action fails, the state is untouched and no hook
runs.

`Gauge` is that pattern with a name. A counter of "how many things are
currently in state *s*" is otherwise a `+= 1` and a `-= 1` at every call site
that changes the state, and it drifts the first time a site is missed.
`Gauge(s, inc, dec)` binds the pair to the state itself.

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
- **Flat states only.** No hierarchical states, no substates, no orthogonal
  regions.
- **No built-in async.** No trigger queue, no run-to-completion mode. `Fire` is
  synchronous and reentrant-unsafe by design: if your state is already
  serialized behind a queue or a mutex, a second one inside the machine is pure
  overhead.
- **`S` must be `comparable`.** Integer-backed enums are the intended shape;
  that keeps states usable as protobuf fields.
- **Guards must be pure.** They are also evaluated by `Check` and `Can`.
