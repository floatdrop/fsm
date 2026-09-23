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
//
// # Groups
//
// States are flat. A [Group] gives a set of them shared transitions — a
// transition declared with [FromGroup] applies to every member that does not
// declare that event itself — but it is a declaration-time grouping that
// expands to ordinary rows in the transition table before [New] returns.
// [Machine.Fire] neither knows about groups nor pays for them.
//
// That expansion is most of what a hierarchical state machine's substates are
// used for, and deliberately not all of it. A group has no entry or exit
// hooks, because the reason to want them is the one thing expansion cannot
// reproduce: in a real hierarchy, moving between two substates of the same
// superstate does not run the superstate's hooks, and there is nowhere to
// record that when every row is flat. So there is also no group [Gauge] — it
// would decrement and increment on a move that a hierarchy would treat as
// staying put. Count the member states individually and sum them where the
// counters are read.
//
// Groups do not nest, and entering a group does not select an initial member.
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
type Event[A any] struct {
	def *eventDef
	_   [0]*A // makes Event[A] and Event[B] inconvertible, so Fire's assertions hold
}

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
	From S
	To   S

	trigger *eventDef
}

// Event returns the trigger's name, for logging. Names are not unique;
// compare with [Transition.Is] to identify the trigger.
func (t Transition[S]) Event() string {
	if t.trigger == nil {
		return ""
	}
	return t.trigger.name
}

// Is reports whether ev triggered the transition.
//
// Branch on this rather than on [Transition.Event]: events are identified by
// declaration, not by name, so two events declared with the same name are
// different triggers that Event cannot tell apart.
func (t Transition[S]) Is[A any](ev Event[A]) bool {
	return t.trigger != nil && t.trigger == ev.def
}

// edge is the key of the transition table.
type edge[S comparable] struct {
	from S
	ev   *eventDef
}

// Hook is a state entry/exit callback. Hooks cannot fail: they exist for
// bookkeeping that must stay paired with the state change, such as
// incrementing and decrementing a gauge. Work that can fail belongs in an
// action registered with [ToStep.Action], which runs before the state changes and
// aborts the transition on error.
//
// A Hook does not see the event's payload, because a state can be entered by
// events carrying different types. When the payload is what the hook is for,
// declare it with [OnEnterVia] or [OnExitVia], which name the event and so
// know its type.
type Hook[S comparable] func(context.Context, Transition[S])

// Machine is an immutable state machine built by [New] from a set of [Rule]s.
//
// A Machine holds no state and is safe for concurrent use.
type Machine[S comparable] struct {
	name    string
	table   map[edge[S]]S
	guards  map[edge[S]]any // func(context.Context, A) (string, error)
	actions map[edge[S]]any // func(context.Context, A) error
	onEnter map[S][]Hook[S]
	onExit  map[S][]Hook[S]

	// Payload-aware hooks, keyed by (state, event) rather than (from, event).
	// Each holds func(context.Context, Transition[S], A) for the A of that
	// event. Kept separate so a machine that declares none pays one length
	// check per fire.
	onEnterVia map[edge[S]][]any
	onExitVia  map[edge[S]][]any

	// Hooks that run after every transition, declared with [OnTransition].
	onAll []Hook[S]

	// True when any hook is declared. A machine that declares none skips the
	// hook block and its lookups.
	hasHooks bool

	// The state declared with [Initial], if any.
	initial    S
	hasInitial bool

	// Declaration order, kept so that introspection and DOT output are
	// deterministic rather than map-iteration order.
	states []S
	edges  []Edge[S]
	groups []Group[S]
}

// Name returns the machine's name, used in error messages and DOT output.
func (m *Machine[S]) Name() string { return m.name }

// Fire applies ev to the state pointed to by st and returns the transition
// it made.
//
// The order of operations is: look up the transition, evaluate guards, run the
// action, run the exit hooks of the old state, assign the new state, run the
// [OnTransition] hooks, then the entry hooks of the new state. If the lookup
// fails, a guard rejects, or the action returns an error, Fire assigns
// nothing, runs no hook, and returns the zero Transition.
//
// The payload often aliases the state. A guard or action that writes *st, or
// fires this machine on it, is reported as a [StateChangedError], again with
// nothing assigned and no hook run. An exit hook that does so is overwritten
// by the assignment. A transition hook runs between the assignment and the
// entry hooks and must not fire either, since the new state is not fully
// entered. Entry hooks run last and may fire.
//
// Fire is a generic method: A is inferred from ev, so the payload is checked
// at compile time.
func (m *Machine[S]) Fire[A any](ctx context.Context, st *S, ev Event[A], arg A) (Transition[S], error) {
	var none Transition[S]
	if st == nil {
		return none, fmt.Errorf("fsm %s: nil state pointer", m.name)
	}
	if ev.def == nil {
		return none, fmt.Errorf("fsm %s: fired the zero Event; declare it with fsm.Define or fsm.Signal", m.name)
	}

	// Read once: the payload may alias the state, and an action may write it.
	from := *st
	e := edge[S]{from: from, ev: ev.def}
	to, ok := m.table[e]
	if !ok {
		return none, &NoTransitionError[S]{Machine: m.name, From: from, Event: ev.def.name}
	}

	// The assertions below are safe by construction: an edge is only ever
	// registered through On[A] with this same event, so the stored closure's
	// payload type is exactly A.
	if raw, ok := m.guards[e]; ok {
		desc, err := raw.(func(context.Context, A) (string, error))(ctx, arg)
		if *st != from {
			return none, &StateChangedError[S]{Machine: m.name, From: from, To: to, Found: *st, Event: ev.def.name, Guard: desc, Err: err}
		}
		if err != nil {
			return none, &GuardError[S]{Machine: m.name, From: from, To: to, Event: ev.def.name, Guard: desc, Err: err}
		}
	}

	if raw, ok := m.actions[e]; ok {
		// A write is reported ahead of a failure, as for guards: the caller
		// must learn that *st no longer holds from.
		err := raw.(func(context.Context, A) error)(ctx, arg)
		if *st != from {
			return none, &StateChangedError[S]{Machine: m.name, From: from, To: to, Found: *st, Event: ev.def.name, Err: err}
		}
		if err != nil {
			return none, &ActionError[S]{Machine: m.name, From: from, To: to, Event: ev.def.name, Err: err}
		}
	}

	t := Transition[S]{From: from, To: to, trigger: ev.def}
	if !m.hasHooks {
		*st = to
		return t, nil
	}

	for _, h := range m.onExit[from] {
		h(ctx, t)
	}
	if len(m.onExitVia) > 0 {
		for _, raw := range m.onExitVia[e] {
			raw.(func(context.Context, Transition[S], A))(ctx, t, arg)
		}
	}

	*st = to

	// Before the entry hooks, so an entry hook that fires again logs after
	// this one. That is why a transition hook must not fire itself.
	for _, h := range m.onAll {
		h(ctx, t)
	}
	for _, h := range m.onEnter[to] {
		h(ctx, t)
	}
	if len(m.onEnterVia) > 0 {
		for _, raw := range m.onEnterVia[edge[S]{from: to, ev: ev.def}] {
			raw.(func(context.Context, Transition[S], A))(ctx, t, arg)
		}
	}
	return t, nil
}

// Send fires an event that carries no payload. It is [Machine.Fire] with the
// payload fixed to Unit{}.
func (m *Machine[S]) Send(ctx context.Context, st *S, ev Event[Unit]) (Transition[S], error) {
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

// ActionError reports a transition whose action failed. The state is
// untouched and no hook ran.
//
// Err is the error the action returned, and ActionError unwraps to it.
type ActionError[S comparable] struct {
	Machine string
	From    S
	To      S
	Event   string
	Err     error
}

func (e *ActionError[S]) Error() string {
	return fmt.Sprintf("fsm %s: action for %v --%s--> %v: %v", e.Machine, e.From, e.Event, e.To, e.Err)
}

// Unwrap returns the error the action failed with.
func (e *ActionError[S]) Unwrap() error { return e.Err }

// StateChangedError reports that a guard or action wrote the state, which
// happens when the payload aliases it. The write is left in place; the
// machine assigns nothing and runs no hook. It takes precedence over a
// rejection or failure from the same callback, whose error is kept in Err.
//
// It deliberately does not unwrap to Err: a guard's sentinel means "state
// untouched, retry", and here the state was touched.
type StateChangedError[S comparable] struct {
	Machine string
	From    S // the state the transition started from
	To      S // the state it was going to
	Found   S // the state the guard or action left behind
	Event   string
	Guard   string // the rejecting guard's description, empty otherwise
	Err     error  // what the guard or action also returned, nil if it succeeded
}

func (e *StateChangedError[S]) Error() string {
	if e.Guard != "" {
		return fmt.Sprintf("fsm %s: state changed to %v during %v --%s--> %v: guard %q: %v",
			e.Machine, e.Found, e.From, e.Event, e.To, e.Guard, e.Err)
	}
	if e.Err != nil {
		return fmt.Sprintf("fsm %s: state changed to %v during %v --%s--> %v: %v",
			e.Machine, e.Found, e.From, e.Event, e.To, e.Err)
	}
	return fmt.Sprintf("fsm %s: state changed to %v during %v --%s--> %v",
		e.Machine, e.Found, e.From, e.Event, e.To)
}
