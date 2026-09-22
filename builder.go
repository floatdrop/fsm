package fsm

import (
	"context"
	"errors"
	"fmt"
)

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

// Builder accumulates a machine definition. Configuration errors are collected
// rather than panicked, and returned together by [Builder.Build].
type Builder[S comparable] struct {
	m    *Machine[S]
	errs []error
	seen map[S]bool
}

// New starts building a machine. name appears in errors and DOT output.
func New[S comparable](name string) *Builder[S] {
	return &Builder[S]{
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
}

// On registers a transition from --ev--> to.
//
// On is a generic method: A is inferred from ev, and every option must carry
// the same payload type, so a guard or action with the wrong signature is a
// compile error rather than a runtime surprise.
func (b *Builder[S]) On[A any](ev Event[A], from, to S, opts ...Option[A]) *Builder[S] {
	if ev.def == nil {
		b.errs = append(b.errs, fmt.Errorf("transition %v -> %v: zero Event; declare it with fsm.Define or fsm.Signal", from, to))
		return b
	}

	e := edge[S]{from: from, ev: ev.def}
	if prev, dup := b.m.table[e]; dup {
		b.errs = append(b.errs, fmt.Errorf("duplicate transition from %v on %s: already goes to %v, redeclared to %v",
			from, ev.def.name, prev, to))
		return b
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
		// Guards are evaluated in registration order and the first rejection
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
	return b
}

// OnEnter registers a hook that runs just after the machine enters s.
// Hooks run in registration order.
func (b *Builder[S]) OnEnter(s S, h Hook[S]) *Builder[S] {
	if h == nil {
		b.errs = append(b.errs, fmt.Errorf("nil OnEnter hook for state %v", s))
		return b
	}
	b.m.onEnter[s] = append(b.m.onEnter[s], h)
	b.declare(s)
	return b
}

// OnExit registers a hook that runs just before the machine leaves s.
func (b *Builder[S]) OnExit(s S, h Hook[S]) *Builder[S] {
	if h == nil {
		b.errs = append(b.errs, fmt.Errorf("nil OnExit hook for state %v", s))
		return b
	}
	b.m.onExit[s] = append(b.m.onExit[s], h)
	b.declare(s)
	return b
}

// Gauge pairs an increment on entering s with a decrement on leaving it.
//
// This is the hook pattern worth having a name for: a counter that tracks
// "how many things are currently in state s" is otherwise maintained by hand
// at every call site that changes the state, and drifts the moment one of them
// is missed.
func (b *Builder[S]) Gauge(s S, inc, dec func(context.Context)) *Builder[S] {
	if inc == nil || dec == nil {
		b.errs = append(b.errs, fmt.Errorf("Gauge for state %v needs both inc and dec", s))
		return b
	}
	b.OnEnter(s, func(ctx context.Context, _ Transition[S]) { inc(ctx) })
	b.OnExit(s, func(ctx context.Context, _ Transition[S]) { dec(ctx) })
	return b
}

// Build validates the definition and returns the machine.
func (b *Builder[S]) Build() (*Machine[S], error) {
	if len(b.m.table) == 0 {
		b.errs = append(b.errs, errors.New("no transitions declared"))
	}
	if len(b.errs) > 0 {
		return nil, fmt.Errorf("fsm %s: %w", b.m.name, errors.Join(b.errs...))
	}
	return b.m, nil
}

// MustBuild is Build for package-level variables. It panics on a bad
// definition, which surfaces at process start rather than at fire time.
func (b *Builder[S]) MustBuild() *Machine[S] {
	m, err := b.Build()
	if err != nil {
		panic(err)
	}
	return m
}

// declare records a state in first-seen order, so that introspection output is
// stable across runs without requiring S to be ordered.
func (b *Builder[S]) declare(s S) {
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
