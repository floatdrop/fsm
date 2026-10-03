// Package fsm provides a small finite state machine for Go.
//
// The machine is immutable configuration; the state itself is owned by the
// caller and passed to [Machine.Fire] as a pointer. That split is deliberate:
// it lets a state live inside a struct that is serialized (a protobuf field, a
// snapshot, a workflow variable) and replayed, without the machine holding a
// second, divergent copy.
//
// A [Machine] is immutable once [New] returns, so one value serves every
// goroutine that owns a state and needs no lock. Hooks are shared, though, so
// whatever they touch is the caller's to synchronize.
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
// used for. A group also has entry and exit hooks ([OnEnterGroup],
// [OnExitGroup]), which run as a superstate's do: only on a transition that
// crosses the group's boundary, never on a move between members. Each row
// learns, once the table is finished, which groups it leaves and enters, so
// a count of what is in a group, kept by those hooks, is exact.
//
// Groups do not nest as states do — a group inside another is drawn inside
// it, and its hooks run inside the other's, but nothing more — and entering
// a group does not select an initial member.
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

// call is one hook as a row runs it: plain, or typed in the payload of the
// row's event, func(context.Context, Transition[S], A).
type call[S comparable] struct {
	plain Hook[S]
	typed any
}

// plan is every hook a row runs, in the order it runs them: exit before
// the assignment; enter after it, the transition hooks first.
type plan[S comparable] struct {
	exit, enter []call[S]
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
// know its type, or with [OnEnterWith] or [OnExitWith], which require every
// event entering or leaving the state to carry the one type.
type Hook[S comparable] func(context.Context, Transition[S])

// Machine is an immutable state machine built by [New] from a set of [Rule]s.
//
// A Machine holds no state and is safe for concurrent use.
type Machine[S comparable] struct {
	name    string
	table   map[edge[S]]S
	guards  map[edge[S]]any // func(context.Context, A) (string, error)
	actions map[edge[S]]any // func(context.Context, A) error

	// plans holds the hooks of every row that runs any, worked out by New.
	// A row with none, an internal one included, has no plan.
	plans map[edge[S]]*plan[S]

	// True when some row has a plan; a machine with none skips the lookup.
	hasHooks bool

	// The state declared with [Initial], if any.
	initial    S
	hasInitial bool

	// Declaration order, kept so that introspection and the diagrams are
	// deterministic rather than map-iteration order.
	states []S
	edges  []Edge[S]
	groups []Group[S]
}

// Name returns the machine's name, used in error messages and diagrams.
func (m *Machine[S]) Name() string { return m.name }

// Fire applies ev to the state pointed to by st and returns the transition
// it made.
//
// The order of operations is: look up the transition, evaluate guards, run the
// action, run the exit hooks of the old state, assign the new state, run the
// [OnTransition] hooks, then the entry hooks of the new state. If the lookup
// fails, a guard rejects, or the action returns an error, Fire assigns
// nothing, runs no hook, and returns the zero Transition. An internal
// transition, declared with [OnStep.Stay], stops after the action: the state
// is not left, so no hook runs.
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
	p := m.plans[e]
	if p == nil { // a row with no hooks, an internal one included
		*st = to
		return t, nil
	}
	for _, c := range p.exit {
		if c.plain != nil {
			c.plain(ctx, t)
		} else {
			c.typed.(func(context.Context, Transition[S], A))(ctx, t, arg)
		}
	}
	*st = to
	for _, c := range p.enter {
		if c.plain != nil {
			c.plain(ctx, t)
		} else {
			c.typed.(func(context.Context, Transition[S], A))(ctx, t, arg)
		}
	}
	return t, nil
}

// TryFire is [Machine.Fire] for a caller that fires whenever an event might
// apply — a controller that reconciles on every turn, say — to which a
// refusal is no news. A state that does not accept ev, or a guard that
// rejects it, reports false with a nil error and allocates nothing; the
// state is untouched and no hook runs. A failed action and a state written
// by a guard or action are still errors, as from Fire, and so are a nil
// state pointer and the zero Event.
//
// Use Fire, or [Machine.Check], when the reason for a refusal matters.
func (m *Machine[S]) TryFire[A any](ctx context.Context, st *S, ev Event[A], arg A) (Transition[S], bool, error) {
	var none Transition[S]
	if err := m.callerBug(st, ev.def, "fired"); err != nil {
		return none, false, err
	}
	from := *st
	to, ok, desc, err := m.lookup(ctx, from, ev, arg)
	switch {
	case !ok:
		return none, false, nil
	case *st != from:
		return none, false, &StateChangedError[S]{Machine: m.name, From: from, To: to, Found: *st, Event: ev.def.name, Guard: desc, Err: err}
	case err != nil:
		return none, false, nil
	}
	t, err := m.apply(ctx, st, from, to, ev, arg)
	return t, err == nil, err
}

// callerBug reports a nil state pointer or the zero Event, which are a
// caller's mistakes rather than machine events.
func (m *Machine[S]) callerBug(st *S, def *eventDef, verb string) error {
	if st == nil {
		return fmt.Errorf("fsm %s: nil state pointer", m.name)
	}
	if def == nil {
		return fmt.Errorf("fsm %s: %s the zero Event; declare it with fsm.Define or fsm.Signal", m.name, verb)
	}
	return nil
}

// lookup finds where ev leads from from, and runs its guards. desc names the
// guard that rejected, if one did. ev's A is the one the row's guard was
// registered with, which is what makes the assertion safe.
func (m *Machine[S]) lookup[A any](ctx context.Context, from S, ev Event[A], arg A) (to S, ok bool, desc string, err error) {
	e := edge[S]{from: from, ev: ev.def}
	if to, ok = m.table[e]; !ok {
		return to, false, "", nil
	}
	if raw, guarded := m.guards[e]; guarded {
		desc, err = raw.(func(context.Context, A) (string, error))(ctx, arg)
	}
	return to, true, desc, err
}

// apply carries out a transition its guards allowed: the action, then the
// row's plan around the assignment. It is the tail of Fire, which keeps its
// own copy.
func (m *Machine[S]) apply[A any](ctx context.Context, st *S, from, to S, ev Event[A], arg A) (Transition[S], error) {
	var none Transition[S]
	e := edge[S]{from: from, ev: ev.def}
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
	p := m.plans[e]
	if p == nil { // a row with no hooks, an internal one included
		*st = to
		return t, nil
	}
	run(ctx, p.exit, t, arg)
	*st = to
	run(ctx, p.enter, t, arg)
	return t, nil
}

// run runs calls with the payload, for apply. Fire keeps the loop inline:
// the call would cost it.
func run[S comparable, A any](ctx context.Context, calls []call[S], t Transition[S], arg A) {
	for _, c := range calls {
		if c.plain != nil {
			c.plain(ctx, t)
		} else {
			c.typed.(func(context.Context, Transition[S], A))(ctx, t, arg)
		}
	}
}

// Send fires an event that carries no payload. It is [Machine.Fire] with the
// payload fixed to Unit{}.
func (m *Machine[S]) Send(ctx context.Context, st *S, ev Event[Unit]) (Transition[S], error) {
	return m.Fire(ctx, st, ev, Unit{})
}

// TrySend is [Machine.TryFire] for an event that carries no payload.
func (m *Machine[S]) TrySend(ctx context.Context, st *S, ev Event[Unit]) (Transition[S], bool, error) {
	return m.TryFire(ctx, st, ev, Unit{})
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
	to, ok, desc, err := m.lookup(ctx, from, ev, arg)
	switch {
	case !ok:
		return &NoTransitionError[S]{Machine: m.name, From: from, Event: ev.def.name}
	case err != nil:
		return &GuardError[S]{Machine: m.name, From: from, To: to, Event: ev.def.name, Guard: desc, Err: err}
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
