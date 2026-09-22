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
// Only this package implements Rule. Declare one with [From], [Gauge],
// [OnEnter] or [OnExit].
type Rule[S comparable] interface {
	applyTo(*builder[S])
}

// ruleFunc adapts a plain closure to [Rule], for the declarations that need
// no further chaining.
type ruleFunc[S comparable] func(*builder[S])

func (f ruleFunc[S]) applyTo(b *builder[S]) { f(b) }

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

	for i, r := range rules {
		if r == nil {
			b.errs = append(b.errs, fmt.Errorf("rule %d is nil", i))
			continue
		}
		r.applyTo(b)
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
//	fsm.From(recStopped).On(evRecFinish).To(recFinished).
//		Guard("all chunks and tracks uploaded", uploadsSettled)
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
// On is a generic method: A is inferred from ev and carried through to the
// rest of the chain, so a guard or action whose payload type does not match
// the event is a compile error rather than a runtime surprise.
func (f FromStep[S]) On[A any](ev Event[A]) OnStep[S, A] {
	return OnStep[S, A]{from: f.from, ev: ev}
}

// OnStep is a transition with its source state and event fixed. It is a
// transient value in a [From] chain; call [OnStep.To] to complete it.
type OnStep[S comparable, A any] struct {
	from S
	ev   Event[A]
}

// To completes the transition. The result is a [Rule] ready for [New], and
// also the place to hang guards and actions:
//
//	fsm.From(a).On(ev).To(b).Guard("...", g).Action(f)
func (o OnStep[S, A]) To(to S) *ToStep[S, A] {
	return &ToStep[S, A]{from: o.from, ev: o.ev, to: to}
}

// ToStep is a complete transition, optionally carrying guards and actions.
// It satisfies [Rule].
//
// Its methods mutate and return the same value, so a guard attached to a
// stored ToStep takes effect whether or not the result is reassigned.
type ToStep[S comparable, A any] struct {
	from, to S
	ev       Event[A]
	guards   []func(context.Context, A) error
	descs    []string
	actions  []func(context.Context, A) error
	errs     []error
}

// Guard rejects the transition when f returns a non-nil error. The error is
// wrapped in a [GuardError], which unwraps to it, so a guard can reject with
// a sentinel the caller matches using errors.Is.
//
// desc is the guard's static description: it labels the edge in DOT output
// and appears in the error, so it should read as the condition being
// enforced — "all chunks and tracks uploaded". The returned error carries the
// dynamic reason the condition did not hold this time.
//
// A guard must be pure: it is also evaluated by [Machine.Check] and
// [Machine.Can]. Guards run in the order they are attached and the first
// rejection wins.
func (t *ToStep[S, A]) Guard(desc string, f func(context.Context, A) error) *ToStep[S, A] {
	if f == nil {
		t.errs = append(t.errs, fmt.Errorf("guard %q is nil", desc))
		return t
	}
	t.guards = append(t.guards, f)
	t.descs = append(t.descs, desc)
	return t
}

// Action runs f as part of the transition. If f returns an error the
// transition is aborted and the state is left unchanged. Actions run in the
// order they are attached, after every guard has passed.
func (t *ToStep[S, A]) Action(f func(context.Context, A) error) *ToStep[S, A] {
	if f == nil {
		t.errs = append(t.errs, errors.New("action is nil"))
		return t
	}
	t.actions = append(t.actions, f)
	return t
}

func (t *ToStep[S, A]) applyTo(b *builder[S]) {
	where := fmt.Sprintf("transition %v -> %v", t.from, t.to)
	for _, err := range t.errs {
		b.errs = append(b.errs, fmt.Errorf("%s: %w", where, err))
	}

	if t.ev.def == nil {
		b.errs = append(b.errs, fmt.Errorf("%s: zero Event; declare it with fsm.Define or fsm.Signal", where))
		return
	}

	e := edge[S]{from: t.from, ev: t.ev.def}
	if prev, dup := b.m.table[e]; dup {
		b.errs = append(b.errs, fmt.Errorf("duplicate transition from %v on %s: already goes to %v, redeclared to %v",
			t.from, t.ev.def.name, prev, t.to))
		return
	}

	b.m.table[e] = t.to
	b.declare(t.from)
	b.declare(t.to)

	// Combined here, where A is still known, so Fire needs a single assertion
	// to a concrete func type and never boxes the payload.
	if len(t.guards) > 0 {
		guards, descs := t.guards, t.descs
		b.m.guards[e] = func(ctx context.Context, a A) (string, error) {
			for i, g := range guards {
				if err := g(ctx, a); err != nil {
					return descs[i], err
				}
			}
			return "", nil
		}
	}
	if len(t.actions) > 0 {
		actions := t.actions
		b.m.actions[e] = func(ctx context.Context, a A) error {
			for _, act := range actions {
				if err := act(ctx, a); err != nil {
					return err
				}
			}
			return nil
		}
	}

	b.m.edges = append(b.m.edges, Edge[S]{
		From: t.from, To: t.to, Event: t.ev.def.name, Guard: joinDescs(t.descs),
	})
}

// OnEnter declares a hook that runs just after the machine enters s.
// Hooks run in declaration order.
func OnEnter[S comparable](s S, h Hook[S]) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if h == nil {
			b.errs = append(b.errs, fmt.Errorf("nil OnEnter hook for state %v", s))
			return
		}
		b.m.onEnter[s] = append(b.m.onEnter[s], h)
		b.declare(s)
	})
}

// OnExit declares a hook that runs just before the machine leaves s.
func OnExit[S comparable](s S, h Hook[S]) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if h == nil {
			b.errs = append(b.errs, fmt.Errorf("nil OnExit hook for state %v", s))
			return
		}
		b.m.onExit[s] = append(b.m.onExit[s], h)
		b.declare(s)
	})
}

// Gauge pairs an increment on entering s with a decrement on leaving it.
//
// This is the hook pattern worth having a name for: a counter that tracks
// "how many things are currently in state s" is otherwise maintained by hand
// at every call site that changes the state, and drifts the moment one of them
// is missed.
func Gauge[S comparable](s S, inc, dec func(context.Context)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if inc == nil || dec == nil {
			b.errs = append(b.errs, fmt.Errorf("Gauge for state %v needs both inc and dec", s))
			return
		}
		b.m.onEnter[s] = append(b.m.onEnter[s], func(ctx context.Context, _ Transition[S]) { inc(ctx) })
		b.m.onExit[s] = append(b.m.onExit[s], func(ctx context.Context, _ Transition[S]) { dec(ctx) })
		b.declare(s)
	})
}

// Edge is a registered transition, reported by [Machine.Edges].
type Edge[S comparable] struct {
	From  S
	To    S
	Event string
	Guard string // guard description, empty when unguarded
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
