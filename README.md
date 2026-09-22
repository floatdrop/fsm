# fsm

A small finite state machine for Go, built around two ideas: **the caller owns the state**, and **events carry typed payloads**.

Requires **Go 1.27** — the API uses generic methods, which earlier versions reject with `method must have no type parameters`.

```go
type recState int32

const (
    recActive recState = iota
    recStopped
    recFinished
    recUploaded
)

var (
    recStop   = fsm.Define[*recording]("stop")
    recFinish = fsm.Define[*recording]("finish")
    recUpload = fsm.Signal("uploaded")
)

var recordingFSM = fsm.New[recState]("recording").
    Gauge(recActive, metrics.Inc(recActive), metrics.Dec(recActive)).
    Gauge(recStopped, metrics.Inc(recStopped), metrics.Dec(recStopped)).
    On(recStop, recActive, recStopped, fsm.WithAction(func(_ context.Context, r *recording) error {
        r.stoppedAt = time.Now()
        return nil
    })).
    On(recFinish, recStopped, recFinished,
        fsm.WithGuard("all chunks and tracks uploaded", func(_ context.Context, r *recording) bool {
            return r.inProgressChunks == 0 && r.inProgressTracks == 0
        }),
    ).
    On(recUpload, recFinished, recUploaded).
    MustBuild()

// The state lives in your struct. Fire mutates it in place.
err := recordingFSM.Fire(ctx, &r.state, recStop, r)
```

## Design

**The machine holds no state.** `Fire` takes a `*S` that lives in your struct. This is the whole reason the package exists in this shape: a state that is serialized into a protobuf field, written to a snapshot, or replayed from a log cannot also live inside a machine object, because the two copies drift. A `Machine` is immutable after `Build` and safe for concurrent use with no lock.

**Events carry typed payloads.** `fsm.Define[*recording]("finish")` can only be fired with a `*recording`, and its guards and actions only ever see a `*recording`. There is no `...any` in the public API and no type assertions in user code:

```go
m.Fire(ctx, &st, recFinish, "nope")
// compile error: cannot use "nope" (untyped string constant) as *recording value
```

This is what generic methods buy. `Fire[A any](ctx, *S, Event[A], A)` is a method on `Machine[S]`, which knows nothing about `A` — before Go 1.27 this had to be a package-level `fsm.Fire(m, ctx, &st, ev, arg)`, or `A` had to be erased to `any` and checked at runtime.

**Nothing panics at fire time.** Configuration mistakes come back from `Build`; unknown transitions come back from `Fire` as `*NoTransitionError`. `MustBuild` panics, but only at construction, so a bad definition fails at process start. This matters when a machine is driven by a replicated log: a panic on a malformed event takes down every replica replaying it, not just one.

**Entry/exit hooks cannot fail.** They exist for bookkeeping that must stay paired with the state change. Work that can fail goes in `WithAction`, which runs *before* the state changes and aborts the transition on error. The ordering is fixed:

```
lookup → guards → action → exit(from) → *st = to → enter(to)
```

If the lookup, a guard, or the action fails, the state is untouched and no hook runs.

**`Gauge` is the hook pattern with a name.** A counter of "how many things are currently in state *s*" is otherwise a `+= 1` and a `-= 1` at every call site that changes the state, and it drifts the first time a site is missed. `Gauge(s, inc, dec)` binds the pair to the state itself.

**Fire does not allocate.** Guards and actions are combined at build time, where the payload type is still known, and stored as a concrete `func(context.Context, A) error`. `Fire` asserts to that type and calls it with the payload directly, so nothing is boxed.

```
BenchmarkFire-12    30166227    39.39 ns/op    0 B/op    0 allocs/op
```

(One round trip, two fires, one with an action.)

## Introspection

`States`, `Edges`, `Terminals`, and `Unreachable` report the machine's shape in declaration order — never map order — so they are usable in tests:

```go
func TestRecordingHasOneTerminalState(t *testing.T) {
    if got := recordingFSM.Terminals(); len(got) != 1 || got[0] != recUploaded {
        t.Errorf("terminals %v, want [uploaded]", got)
    }
    if got := recordingFSM.Unreachable(recActive); len(got) != 0 {
        t.Errorf("unreachable states: %v", got)
    }
}
```

`DOT()` renders a Graphviz digraph, with terminal states drawn as double circles and guards shown on the edge labels. Output is stable across runs, so it can be committed next to the code and reviewed in a diff when the machine changes.

## Limitations

- **Generic methods do not satisfy interfaces.** `Machine[S]` cannot be hidden behind an `interface{ Fire(...) }`, because a method with its own type parameter never matches an interface method:
  ```
  have M[A any](A) A
  want M(int) int
  ```
  In practice a machine is a package-level variable, not a dependency you inject, so this has not been a problem — but it rules out mocking the machine itself. Test against the states instead.
- **Flat states only.** No hierarchical states, no substates, no orthogonal regions.
- **No built-in async.** No trigger queue, no run-to-completion mode. `Fire` is synchronous and reentrant-unsafe by design: if your state is already serialized behind a queue or a mutex, a second one inside the machine is pure overhead.
- **`S` must be `comparable`.** Integer-backed enums are the intended shape; that keeps states usable as protobuf fields.
- **Guards must be pure.** They are also evaluated by `Can`.

## Status

Prototype. The API is not settled — in particular `Option[A]`, the hook signature, and whether guards should return an error instead of a `(bool, string)` pair.

## License

MIT — see [LICENSE](LICENSE).
