// Package fsm provides a small finite state machine for Go.
//
// The machine is immutable configuration; the state itself is owned by the
// caller and passed to [Machine.Fire] as a pointer. That split is deliberate:
// it lets a state live inside a struct that is serialized (a protobuf field, a
// snapshot, a workflow variable) and replayed, without the machine holding a
// second, divergent copy.
//
// Events carry typed payloads. An [Event] declared as Event[time.Time] can only
// be fired with a time.Time, and its action only ever sees a time.Time — there
// is no ...any in the public API, and no type assertions in user code.
//
// Nothing in this package panics at fire time. Configuration mistakes are
// reported by [New]; unknown transitions are reported by
// [Machine.Fire] as errors. This matters when a machine is driven by a
// replicated log: a panic on a malformed event would take down every replica
// replaying it, not just one.
package fsm

import (
	"context"
	"fmt"
)

// Unit is the payload type of events that carry no data. See [Signal].
type Unit = struct{}

// eventDef is an event's runtime identity. Events are compared by the
// identity of this pointer rather than by name, so two events declared with
// the same name in different machines never collide.
type eventDef struct{ name string }

// Event is a trigger carrying a payload of type A.
//
// Events are declared once, typically as package-level variables:
//
//	var (
//		evStop   = fsm.Define[time.Time]("stop")
//		evFinish = fsm.Signal("finish")
//	)
type Event[A any] struct{ def *eventDef }

// Define declares an event carrying a payload of type A.
func Define[A any](name string) Event[A] {
	return Event[A]{def: &eventDef{name: name}}
}

// Signal declares an event with no payload. It is shorthand for
// Define[Unit] and is fired with [Machine.Send].
func Signal(name string) Event[Unit] { return Define[Unit](name) }

// Name returns the event's declared name. The zero Event reports "<invalid>".
func (e Event[A]) Name() string {
	if e.def == nil {
		return "<invalid>"
	}
	return e.def.name
}

func (e Event[A]) String() string { return e.Name() }

// Transition describes a state change that is about to happen or has just
// happened. It is passed to the hooks declared with [OnEnter] and [OnExit].
type Transition[S comparable] struct {
	From  S
	To    S
	Event string
}

// edge is the key of the transition table.
type edge[S comparable] struct {
	from S
	ev   *eventDef
}

// Hook is a state entry/exit callback. Hooks cannot fail: they exist for
// bookkeeping that must stay paired with the state change, such as
// incrementing and decrementing a gauge. Work that can fail belongs in an
// action registered with [WithAction], which runs before the state changes and
// aborts the transition on error.
type Hook[S comparable] func(context.Context, Transition[S])

// Machine is an immutable state machine built by [New] from a set of [Rule]s.
//
// A Machine holds no state and is safe for concurrent use.
type Machine[S comparable] struct {
	name    string
	table   map[edge[S]]S
	guards  map[edge[S]]any // func(context.Context, A) (bool, string)
	actions map[edge[S]]any // func(context.Context, A) error
	onEnter map[S][]Hook[S]
	onExit  map[S][]Hook[S]

	// Declaration order, kept so that introspection and DOT output are
	// deterministic rather than map-iteration order.
	states []S
	edges  []Edge[S]
}

// Name returns the machine's name, used in error messages and DOT output.
func (m *Machine[S]) Name() string { return m.name }

// Fire applies ev to the state pointed to by st.
//
// The order of operations is: look up the transition, evaluate guards, run the
// action, run the exit hooks of the old state, assign the new state, run the
// entry hooks of the new state. If the lookup fails, a guard rejects, or the
// action returns an error, *st is left untouched and no hook runs.
//
// Fire is a generic method: A is inferred from ev, so the payload is checked
// at compile time.
func (m *Machine[S]) Fire[A any](ctx context.Context, st *S, ev Event[A], arg A) error {
	if st == nil {
		return fmt.Errorf("fsm %s: nil state pointer", m.name)
	}
	if ev.def == nil {
		return fmt.Errorf("fsm %s: fired the zero Event; declare it with fsm.Define or fsm.Signal", m.name)
	}

	e := edge[S]{from: *st, ev: ev.def}
	to, ok := m.table[e]
	if !ok {
		return &NoTransitionError[S]{Machine: m.name, From: *st, Event: ev.def.name}
	}

	// The assertions below are safe by construction: an edge is only ever
	// registered through On[A] with this same event, so the stored closure's
	// payload type is exactly A.
	if raw, ok := m.guards[e]; ok {
		if desc, err := raw.(func(context.Context, A) (string, error))(ctx, arg); err != nil {
			return &GuardError[S]{Machine: m.name, From: *st, To: to, Event: ev.def.name, Guard: desc, Err: err}
		}
	}

	if raw, ok := m.actions[e]; ok {
		if err := raw.(func(context.Context, A) error)(ctx, arg); err != nil {
			return fmt.Errorf("fsm %s: action for %v --%s--> %v: %w", m.name, *st, ev.def.name, to, err)
		}
	}

	t := Transition[S]{From: *st, To: to, Event: ev.def.name}
	for _, h := range m.onExit[t.From] {
		h(ctx, t)
	}
	*st = to
	for _, h := range m.onEnter[to] {
		h(ctx, t)
	}
	return nil
}

// Send fires an event that carries no payload.
func (m *Machine[S]) Send(ctx context.Context, st *S, ev Event[Unit]) error {
	return m.Fire(ctx, st, ev, Unit{})
}

// Check reports why firing ev from state from would fail, evaluating guards
// but running no action and no hook. It returns nil when the transition would
// be allowed, and otherwise the same error [Machine.Fire] would return.
//
// Use Check over [Machine.Can] when the reason matters — to report it, or to
// match a sentinel with errors.Is.
func (m *Machine[S]) Check[A any](ctx context.Context, from S, ev Event[A], arg A) error {
	if ev.def == nil {
		return fmt.Errorf("fsm %s: checked the zero Event; declare it with fsm.Define or fsm.Signal", m.name)
	}

	e := edge[S]{from: from, ev: ev.def}
	to, ok := m.table[e]
	if !ok {
		return &NoTransitionError[S]{Machine: m.name, From: from, Event: ev.def.name}
	}
	if raw, ok := m.guards[e]; ok {
		if desc, err := raw.(func(context.Context, A) (string, error))(ctx, arg); err != nil {
			return &GuardError[S]{Machine: m.name, From: from, To: to, Event: ev.def.name, Guard: desc, Err: err}
		}
	}
	return nil
}

// Can reports whether firing ev from state from would succeed. It is
// [Machine.Check] with the reason discarded.
func (m *Machine[S]) Can[A any](ctx context.Context, from S, ev Event[A], arg A) bool {
	return m.Check(ctx, from, ev, arg) == nil
}

// To returns the state that ev leads to from state from, ignoring guards.
func (m *Machine[S]) To[A any](from S, ev Event[A]) (S, bool) {
	var zero S
	if ev.def == nil {
		return zero, false
	}
	to, ok := m.table[edge[S]{from: from, ev: ev.def}]
	return to, ok
}

// NoTransitionError reports an event fired from a state that does not accept
// it. Match it with errors.AsType.
type NoTransitionError[S comparable] struct {
	Machine string
	From    S
	Event   string
}

func (e *NoTransitionError[S]) Error() string {
	return fmt.Sprintf("fsm %s: no transition from %v on %s", e.Machine, e.From, e.Event)
}

// GuardError reports a transition that exists but was rejected by a guard.
//
// Err is the error the guard returned. GuardError unwraps to it, so a guard
// can reject with a sentinel and the caller can match it directly:
//
//	if errors.Is(err, ErrChunksPending) { ... }
type GuardError[S comparable] struct {
	Machine string
	From    S
	To      S
	Event   string
	Guard   string // the guard's static description, empty when unnamed
	Err     error
}

func (e *GuardError[S]) Error() string {
	if e.Guard != "" {
		return fmt.Sprintf("fsm %s: transition %v --%s--> %v rejected by guard %q: %v",
			e.Machine, e.From, e.Event, e.To, e.Guard, e.Err)
	}
	return fmt.Sprintf("fsm %s: transition %v --%s--> %v rejected: %v",
		e.Machine, e.From, e.Event, e.To, e.Err)
}

// Unwrap returns the error the guard rejected with.
func (e *GuardError[S]) Unwrap() error { return e.Err }
