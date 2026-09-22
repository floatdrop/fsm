package fsm

import (
	"context"
	"errors"
	"fmt"
)

// Rule is one declaration a machine is made of: a transition, a gauge or a
// hook. Rules are values, so a set of them can be built up in a loop, stored,
// and shared between machines.
//
// Rule is not [Option]. A Rule declares part of a machine and goes to [New];
// an Option configures a single transition and goes to [OnStep.To].
type Rule[S comparable] func(*builder[S])

// builder accumulates the machine as rules are applied to it.
type builder[S comparable] struct {
	m    *Machine[S]
	errs []error
	seen map[S]bool
}

// New builds a machine from its rules. name appears in errors and DOT output.
//
// S is inferred from the rules, so it rarely has to be written out. A machine
// with no rules cannot infer it, and is an error in any case.
func New[S comparable](name string, rules ...Rule[S]) (*Machine[S], error) {
	b := &builder[S]{
		m: &Machine[S]{
			name:    name,
			table:   make(map[edge[S]]S),
			guards:  make(map[edge[S]]any),
			actions: make(map[edge[S]]any),
			onEnter: make(map[S][]Hook[S]),
			onExit:  make(map[S][]Hook[S]),
		},
		seen: make(map[S]bool),
	}

	for _, r := range rules {
		r(b)
	}

	if len(b.m.table) == 0 {
		b.errs = append(b.errs, errors.New("no transitions declared"))
	}
	if len(b.errs) > 0 {
		return nil, fmt.Errorf("fsm %s: %w", name, errors.Join(b.errs...))
	}
	return b.m, nil
}

// MustNew is [New] for package-level variables. It panics on a bad
// definition, which surfaces at process start rather than at fire time.
func MustNew[S comparable](name string, rules ...Rule[S]) *Machine[S] {
	m, err := New(name, rules...)
	if err != nil {
		panic(err)
	}
	return m
}

// From begins a transition declaration:
//
//	fsm.From(recStopped).On(evRecFinish).To(recFinished)
//
// The source and target states are named by separate calls rather than passed
// as two adjacent arguments of the same type, so they cannot be swapped by
// mistake.
func From[S comparable](s S) FromStep[S] { return FromStep[S]{from: s} }

// FromStep is a transition with its source state fixed. It is a transient
// value in a [From] chain; call [FromStep.On] to continue.
type FromStep[S comparable] struct{ from S }

// On names the event that triggers the transition.
//
// On is a generic method: A is inferred from ev and carried through to
// [OnStep.To], so an option whose payload type does not match the event is a
// compile error rather than a runtime surprise.
func (f FromStep[S]) On[A any](ev Event[A]) OnStep[S, A] {
	return OnStep[S, A]{from: f.from, ev: ev}
}

// OnStep is a transition with its source state and event fixed. It is a
// transient value in a [From] chain; call [OnStep.To] to complete it.
type OnStep[S comparable, A any] struct {
	from S
	ev   Event[A]
}

// To completes the transition and returns it as a [Rule].
//
// The payload type is erased here, after the options have been checked
// against it, so the resulting Rule is an ordinary Rule[S] that [New] can take
// alongside transitions carrying any other payload.
func (o OnStep[S, A]) To(to S, opts ...Option[A]) Rule[S] {
	return func(b *builder[S]) { b.add(o.ev, o.from, to, opts...) }
}

// OnEnter declares a hook that runs just after the machine enters s.
// Hooks run in declaration order.
func OnEnter[S comparable](s S, h Hook[S]) Rule[S] {
	return func(b *builder[S]) {
		if h == nil {
			b.errs = append(b.errs, fmt.Errorf("nil OnEnter hook for state %v", s))
			return
		}
		b.m.onEnter[s] = append(b.m.onEnter[s], h)
		b.declare(s)
	}
}

// OnExit declares a hook that runs just before the machine leaves s.
func OnExit[S comparable](s S, h Hook[S]) Rule[S] {
	return func(b *builder[S]) {
		if h == nil {
			b.errs = append(b.errs, fmt.Errorf("nil OnExit hook for state %v", s))
			return
		}
		b.m.onExit[s] = append(b.m.onExit[s], h)
		b.declare(s)
	}
}

// Gauge pairs an increment on entering s with a decrement on leaving it.
//
// This is the hook pattern worth having a name for: a counter that tracks
// "how many things are currently in state s" is otherwise maintained by hand
// at every call site that changes the state, and drifts the moment one of them
// is missed.
func Gauge[S comparable](s S, inc, dec func(context.Context)) Rule[S] {
	return func(b *builder[S]) {
		if inc == nil || dec == nil {
			b.errs = append(b.errs, fmt.Errorf("Gauge for state %v needs both inc and dec", s))
			return
		}
		b.m.onEnter[s] = append(b.m.onEnter[s], func(ctx context.Context, _ Transition[S]) { inc(ctx) })
		b.m.onExit[s] = append(b.m.onExit[s], func(ctx context.Context, _ Transition[S]) { dec(ctx) })
		b.declare(s)
	}
}

// Option configures a single transition. See [WithGuard] and [WithAction].
type Option[A any] struct {
	guard     func(context.Context, A) error
	guardDesc string
	action    func(context.Context, A) error
}

// WithGuard rejects the transition when f returns a non-nil error. The error
// is wrapped in a [GuardError], which unwraps to it, so a guard can reject
// with a sentinel the caller matches using errors.Is.
//
// desc is the guard's static description: it labels the edge in DOT output
// and appears in the error, so it should read as the condition being
// enforced — "all chunks and tracks uploaded". The returned error carries the
// dynamic reason the condition did not hold this time.
//
// A guard must be pure: it is also evaluated by [Machine.Check] and
// [Machine.Can].
func WithGuard[A any](desc string, f func(context.Context, A) error) Option[A] {
	return Option[A]{guard: f, guardDesc: desc}
}

// WithAction runs f as part of the transition. If f returns an error the
// transition is aborted and the state is left unchanged.
func WithAction[A any](f func(context.Context, A) error) Option[A] {
	return Option[A]{action: f}
}

// Edge is a registered transition, reported by [Machine.Edges].
type Edge[S comparable] struct {
	From  S
	To    S
	Event string
	Guard string // guard description, empty when unguarded
}

// add records a transition from --ev--> to.
func (b *builder[S]) add[A any](ev Event[A], from, to S, opts ...Option[A]) {
	if ev.def == nil {
		b.errs = append(b.errs, fmt.Errorf("transition %v -> %v: zero Event; declare it with fsm.Define or fsm.Signal", from, to))
		return
	}

	e := edge[S]{from: from, ev: ev.def}
	if prev, dup := b.m.table[e]; dup {
		b.errs = append(b.errs, fmt.Errorf("duplicate transition from %v on %s: already goes to %v, redeclared to %v",
			from, ev.def.name, prev, to))
		return
	}

	var (
		guards  []func(context.Context, A) error
		descs   []string
		actions []func(context.Context, A) error
	)
	for _, o := range opts {
		if o.guard != nil {
			guards = append(guards, o.guard)
			descs = append(descs, o.guardDesc)
		}
		if o.action != nil {
			actions = append(actions, o.action)
		}
	}

	b.m.table[e] = to
	b.declare(from)
	b.declare(to)

	if len(guards) > 0 {
		// Combined here, where A is still known, so Fire needs a single
		// assertion to a concrete func type and never boxes the payload.
		// Guards are evaluated in declaration order and the first rejection
		// wins, reported together with the description it was declared under.
		b.m.guards[e] = func(ctx context.Context, a A) (string, error) {
			for i, g := range guards {
				if err := g(ctx, a); err != nil {
					return descs[i], err
				}
			}
			return "", nil
		}
	}
	if len(actions) > 0 {
		b.m.actions[e] = func(ctx context.Context, a A) error {
			for _, act := range actions {
				if err := act(ctx, a); err != nil {
					return err
				}
			}
			return nil
		}
	}

	b.m.edges = append(b.m.edges, Edge[S]{From: from, To: to, Event: ev.def.name, Guard: joinDescs(descs)})
}

// declare records a state in first-seen order, so that introspection output is
// stable across runs without requiring S to be ordered.
func (b *builder[S]) declare(s S) {
	if b.seen[s] {
		return
	}
	b.seen[s] = true
	b.m.states = append(b.m.states, s)
}

func joinDescs(descs []string) string {
	out := ""
	for _, d := range descs {
		if d == "" {
			continue
		}
		if out != "" {
			out += " && "
		}
		out += d
	}
	return out
}
