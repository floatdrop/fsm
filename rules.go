package fsm

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

	// Every declared transition in order, with the payload type its event
	// carries. [GaugeWith] needs to see the whole table, so it is recorded
	// here and read in a second pass.
	decls []decl[S]

	// Group transitions, which add edges and so must run before the rules
	// that read the finished table.
	expand []func(*builder[S])

	// Rules that must run once every transition is known.
	deferred []func(*builder[S])

	// Which group each inherited row came from, so that two groups claiming
	// the same (state, event) is reported rather than read as an override.
	inherited map[edge[S]]string

	// Targets of the group transitions declared so far, so that one declared
	// twice reads as a duplicate rather than as a group clashing with itself.
	groupEdges map[groupEdge]S
}

// groupEdge identifies one group transition, before it expands to a row per
// member.
type groupEdge struct {
	group string
	ev    *eventDef
}

type decl[S comparable] struct {
	key     edge[S]
	to      S
	payload any // (*A)(nil) for the event's payload type A
}

// payloadToken identifies a payload type without reflect: two typed nil
// pointers compare equal exactly when their types match.
func payloadToken[A any]() any { return (*A)(nil) }

// New builds a machine from its rules. name appears in errors and DOT output.
//
// S is inferred from the rules, so it rarely has to be written out. A machine
// with no rules cannot infer it, and is an error in any case.
func New[S comparable](name string, rules ...Rule[S]) (*Machine[S], error) {
	b := &builder[S]{
		m: &Machine[S]{
			name:       name,
			table:      make(map[edge[S]]S),
			guards:     make(map[edge[S]]any),
			actions:    make(map[edge[S]]any),
			onEnter:    make(map[S][]Hook[S]),
			onExit:     make(map[S][]Hook[S]),
			onEnterVia: make(map[edge[S]][]any),
			onExitVia:  make(map[edge[S]][]any),
		},
		seen:       make(map[S]bool),
		inherited:  make(map[edge[S]]string),
		groupEdges: make(map[groupEdge]S),
	}

	for i, r := range rules {
		if r == nil {
			b.errs = append(b.errs, fmt.Errorf("rule %d is nil", i))
			continue
		}
		r.applyTo(b)
	}

	// Checked before expansion, while seen holds only explicitly declared
	// states, so a mistyped member is reported rather than quietly given
	// edges of its own.
	for _, g := range b.m.groups {
		for _, s := range g.members {
			if !b.seen[s] {
				b.errs = append(b.errs, fmt.Errorf(
					"group %s: member %v is not a state of this machine", g.name, s))
			}
		}
	}

	// Group transitions expand first, since they add edges. Rules that need
	// the whole transition table then run against the finished one, so a
	// GaugeWith can be declared before the edges it applies to.
	for _, e := range b.expand {
		e(b)
	}
	for _, d := range b.deferred {
		d(b)
	}

	if len(b.m.table) == 0 {
		b.errs = append(b.errs, errors.New("no transitions declared"))
	}
	b.m.hasHooks = len(b.m.onEnter) > 0 || len(b.m.onExit) > 0 ||
		len(b.m.onEnterVia) > 0 || len(b.m.onExitVia) > 0
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
func From[S comparable](s S) FromStep[S] { return FromStep[S]{froms: []S{s}} }

// FromEach begins a transition declaration with several source states: the
// rule it produces registers one transition per source, all on the same event
// and to the same target.
//
//	fsm.FromEach(pcpConnected, pcpReconnecting).On(evPcpKick).To(pcpDeleted)
//
// It is a fan-in shorthand, not a hierarchy — the sources are enumerated at
// this declaration and the machine learns nothing that relates them. When they
// are related, and especially when one of them needs to handle the event
// differently, declare a [Group] and use [FromGroup] instead.
//
// A source list computed elsewhere can be spread into it directly:
//
//	fsm.FromEach(liveStates...).On(evAbort).To(dead)
func FromEach[S comparable](ss ...S) FromStep[S] {
	return FromStep[S]{froms: slices.Clone(ss)}
}

// FromGroup begins a transition declaration whose source is every member of g
// that does not declare the event itself. See [Group].
func FromGroup[S comparable](g Group[S]) FromStep[S] {
	return FromStep[S]{group: &g}
}

// FromStep is a transition with its source states fixed. It is a transient
// value in a [From] chain; call [FromStep.On] to continue.
type FromStep[S comparable] struct {
	froms []S
	group *Group[S] // set by FromGroup instead of froms
}

// On names the event that triggers the transition.
//
// On is a generic method: A is inferred from ev and carried through to the
// rest of the chain, so a guard or action whose payload type does not match
// the event is a compile error rather than a runtime surprise.
func (f FromStep[S]) On[A any](ev Event[A]) OnStep[S, A] {
	return OnStep[S, A]{froms: f.froms, group: f.group, ev: ev}
}

// OnStep is a transition with its source states and event fixed. It is a
// transient value in a [From] chain; call [OnStep.To] to complete it.
type OnStep[S comparable, A any] struct {
	froms []S
	group *Group[S]
	ev    Event[A]
}

// To completes the transition. The result is a [Rule] ready for [New], and
// also the place to hang guards and actions:
//
//	fsm.From(a).On(ev).To(b).Guard("...", g).Action(f)
//
// A self-transition — To naming the state On came from — is allowed and runs
// the full sequence: the exit hooks of the state, the assignment, then its
// entry hooks. That is UML's external self-transition, and it means a [Gauge]
// on the state decrements and increments back to where it was. There is no
// internal transition that skips the hooks; use an [ToStep.Action] for that.
func (o OnStep[S, A]) To(to S) *ToStep[S, A] {
	return &ToStep[S, A]{froms: o.froms, group: o.group, ev: o.ev, to: to}
}

// ToStep is a complete transition, optionally carrying guards and actions.
// It satisfies [Rule].
//
// Its methods mutate and return the same value, so a guard attached to a
// stored ToStep takes effect whether or not the result is reassigned.
type ToStep[S comparable, A any] struct {
	froms   []S
	group   *Group[S]
	to      S
	ev      Event[A]
	guards  []func(context.Context, A) error
	descs   []string
	actions []func(context.Context, A) error
	errs    []error
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
	where := t.describe()
	for _, err := range t.errs {
		b.errs = append(b.errs, fmt.Errorf("%s: %w", where, err))
	}

	if t.ev.def == nil {
		b.errs = append(b.errs, fmt.Errorf("%s: zero Event; declare it with fsm.Define or fsm.Signal", where))
		return
	}

	r := row[S]{
		to:      t.to,
		ev:      t.ev.def,
		payload: payloadToken[A](),
		desc:    joinDescs(t.descs),
	}
	r.guard, r.action = t.combine()

	if t.group != nil {
		t.expandGroup(b, r, where)
		return
	}
	if len(t.froms) == 0 {
		b.errs = append(b.errs, fmt.Errorf("%s: no source states", where))
		return
	}

	for _, s := range t.froms {
		if prev, dup := b.m.table[edge[S]{from: s, ev: r.ev}]; dup {
			b.errs = append(b.errs, fmt.Errorf("duplicate transition from %v on %s: already goes to %v, redeclared to %v",
				s, r.ev.name, prev, t.to))
			continue
		}
		r.from = s
		b.register(r)
	}
}

// expandGroup defers the rule until every explicit transition is known, so a
// member that declares the event itself is seen and left alone.
func (t *ToStep[S, A]) expandGroup(b *builder[S], r row[S], where string) {
	g := *t.group
	if !b.declareGroup(g) {
		return
	}
	r.group = g.name

	gk := groupEdge{group: g.name, ev: r.ev}
	if prev, dup := b.groupEdges[gk]; dup {
		b.errs = append(b.errs, fmt.Errorf(
			"duplicate transition from group %s on %s: already goes to %v, redeclared to %v",
			g.name, r.ev.name, prev, t.to))
		return
	}
	b.groupEdges[gk] = t.to

	// The target is a state of the machine whether or not any member ends up
	// inheriting the edge, and declaring it now lets member validation see it.
	b.declare(t.to)

	b.expand = append(b.expand, func(b *builder[S]) {
		var inherited, clashes int
		for _, s := range g.members {
			k := edge[S]{from: s, ev: r.ev}
			if owner, clash := b.inherited[k]; clash {
				b.errs = append(b.errs, fmt.Errorf(
					"%s: groups %s and %s both give %v a transition on %s",
					where, owner, g.name, s, r.ev.name))
				clashes++
				continue
			}
			if _, override := b.m.table[k]; override {
				continue // the member declares this event itself and wins
			}
			b.inherited[k] = g.name
			r.from = s
			b.register(r)
			inherited++
		}
		// Only when every member overrode it: a member lost to another group
		// has already been reported, and saying it overrode the event itself
		// would not be true.
		if inherited == 0 && clashes == 0 {
			b.errs = append(b.errs, fmt.Errorf(
				"%s: every member of %s declares %s itself, so the group transition is unreachable",
				where, g.name, r.ev.name))
		}
	})
}

// combine folds the guards and actions into one closure each, here where A is
// still known, so Fire needs a single assertion to a concrete func type and
// never boxes the payload. A nil result means there is nothing to store.
func (t *ToStep[S, A]) combine() (guard, action any) {
	if len(t.guards) > 0 {
		guards, descs := t.guards, t.descs
		guard = func(ctx context.Context, a A) (string, error) {
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
		action = func(ctx context.Context, a A) error {
			for _, act := range actions {
				if err := act(ctx, a); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return guard, action
}

// describe names the rule in error messages. The single-source form is the
// common one and reads as it always has.
func (t *ToStep[S, A]) describe() string {
	switch {
	case t.group != nil:
		return fmt.Sprintf("transition group %s -> %v", t.group.name, t.to)
	case len(t.froms) == 1:
		return fmt.Sprintf("transition %v -> %v", t.froms[0], t.to)
	default:
		return fmt.Sprintf("transition %v -> %v", t.froms, t.to)
	}
}

// row is one line of the transition table. Its fields are named because a
// positional call with three states and three any values would be unreadable
// and easy to transpose.
type row[S comparable] struct {
	from, to S
	ev       *eventDef
	guard    any // func(context.Context, A) (string, error)
	action   any // func(context.Context, A) error
	payload  any // (*A)(nil) for the event's payload type
	desc     string
	group    string // the group it was inherited from, empty if declared directly
}

// register adds one row to the transition table.
func (b *builder[S]) register(r row[S]) {
	e := edge[S]{from: r.from, ev: r.ev}
	b.m.table[e] = r.to
	b.decls = append(b.decls, decl[S]{key: e, to: r.to, payload: r.payload})
	b.declare(r.from)
	b.declare(r.to)

	if r.guard != nil {
		b.m.guards[e] = r.guard
	}
	if r.action != nil {
		b.m.actions[e] = r.action
	}
	b.m.edges = append(b.m.edges, Edge[S]{
		From: r.from, To: r.to, Event: r.ev.name, Guard: r.desc, Group: r.group,
		trigger: r.ev,
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

// OnEnterVia declares a hook that runs just after the machine enters s, but
// only when ev is what put it there — and hands the hook that event's
// payload:
//
//	fsm.OnEnterVia(pcpDeleted, evPcpKick, func(_ context.Context, tr fsm.Transition[pcpState], d disconnect) {
//		log.Info("participant kicked", "reason", d.reason)
//	})
//
// A plain [OnEnter] hook cannot do this: a state can be entered by events
// carrying different payload types, so there is no single type to hand it.
// Naming the event fixes A, which is what makes the hook typed.
//
// Like [OnEnter], it runs after the state has changed and cannot fail. It
// runs after the plain entry hooks of the same state.
func OnEnterVia[S comparable, A any](s S, ev Event[A], h func(context.Context, Transition[S], A)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if !b.checkVia("OnEnterVia", s, ev.def, h == nil) {
			return
		}
		k := edge[S]{from: s, ev: ev.def}
		b.m.onEnterVia[k] = append(b.m.onEnterVia[k], h)
		b.declare(s)
	})
}

// OnExitVia declares a hook that runs just before the machine leaves s, but
// only when ev is what moves it — and hands the hook that event's payload.
// See [OnEnterVia]. It runs after the plain exit hooks of the same state, and
// still before the state changes.
func OnExitVia[S comparable, A any](s S, ev Event[A], h func(context.Context, Transition[S], A)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if !b.checkVia("OnExitVia", s, ev.def, h == nil) {
			return
		}
		k := edge[S]{from: s, ev: ev.def}
		b.m.onExitVia[k] = append(b.m.onExitVia[k], h)
		b.declare(s)
	})
}

func (b *builder[S]) checkVia(what string, s S, def *eventDef, nilHook bool) bool {
	if nilHook {
		b.errs = append(b.errs, fmt.Errorf("nil %s hook for state %v", what, s))
		return false
	}
	if def == nil {
		b.errs = append(b.errs, fmt.Errorf("%s for state %v: zero Event; declare it with fsm.Define or fsm.Signal", what, s))
		return false
	}
	return true
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

// GaugeWith is [Gauge] for a counter that needs something from the event's
// payload — the instance the count is labelled by, typically:
//
//	fsm.GaugeWith(recStopped,
//		func(_ context.Context, s recStep) { s.r.metrics.Add(recStopped, 1) },
//		func(_ context.Context, s recStep) { s.r.metrics.Add(recStopped, -1) },
//	)
//
// Gauge's closures are fixed when the machine is built, which is no good when
// the counter belongs to whatever the transition is about. GaugeWith is still
// declared once per state, so the increment and its decrement stay bound
// together: it expands to an entry hook on every event that enters s and an
// exit hook on every event that leaves s.
//
// That requires every event touching s to carry the same payload type A.
// [New] checks it and reports the offending transition rather than letting a
// mismatched event silently skip the counter, so a machine whose events carry
// different payloads can still use GaugeWith on the states where they agree.
//
// Entering the initial state is not a transition, so the first increment is
// the caller's, exactly as with [Gauge].
func GaugeWith[S comparable, A any](s S, inc, dec func(context.Context, A)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if inc == nil || dec == nil {
			b.errs = append(b.errs, fmt.Errorf("GaugeWith for state %v needs both inc and dec", s))
			return
		}
		b.declare(s)

		b.deferred = append(b.deferred, func(b *builder[S]) {
			want := payloadToken[A]()
			var touched int
			for _, d := range b.decls {
				entering := d.to == s
				leaving := d.key.from == s
				if !entering && !leaving {
					continue
				}
				if d.payload != want {
					b.errs = append(b.errs, fmt.Errorf(
						"GaugeWith for state %v: transition %v --%s--> %v carries a different payload type, so the counter cannot be kept paired",
						s, d.key.from, d.key.ev.name, d.to))
					continue
				}
				touched++
				if entering {
					b.m.onEnterVia[edge[S]{from: s, ev: d.key.ev}] = append(
						b.m.onEnterVia[edge[S]{from: s, ev: d.key.ev}],
						func(ctx context.Context, _ Transition[S], a A) { inc(ctx, a) })
				}
				if leaving {
					b.m.onExitVia[edge[S]{from: s, ev: d.key.ev}] = append(
						b.m.onExitVia[edge[S]{from: s, ev: d.key.ev}],
						func(ctx context.Context, _ Transition[S], a A) { dec(ctx, a) })
				}
			}
			if touched == 0 {
				b.errs = append(b.errs, fmt.Errorf(
					"GaugeWith for state %v: no transition enters or leaves it, so the counter would never move", s))
			}
		})
	})
}

// Group is a named set of states that share transitions. A transition declared
// with [FromGroup] applies to every member that does not declare that event
// itself, so adding a member inherits the group's transitions and a member can
// still specialise one:
//
//	var pcpLive = fsm.NewGroup("live", pcpConnected, pcpReconnecting)
//
//	fsm.FromGroup(pcpLive).On(evPcpKick).To(pcpDeleted)       // both members
//	fsm.From(pcpReconnecting).On(evPcpKick).To(pcpAbandoned)  // overrides it
//
// Membership is declared in one place, which is the difference from
// [FromEach]: enumerating the sources at each transition means a new member
// silently inherits nothing.
//
// A Group is not a state. A *S never holds one and it does not appear in
// [Machine.States] — it is a declaration-time grouping that expands to
// ordinary rows in the transition table, so [Machine.Fire] neither knows nor
// pays for it. Group entry and exit hooks are deliberately absent; see the
// package documentation for what that rules out.
//
// A Group is itself a [Rule], so a group used only for [Group.Has] or for DOT
// output can be passed to [New] on its own.
type Group[S comparable] struct {
	name    string
	members []S
}

// NewGroup declares a group of states. name labels it in errors and in DOT
// output, and must be unique within a machine.
func NewGroup[S comparable](name string, members ...S) Group[S] {
	return Group[S]{name: name, members: slices.Clone(members)}
}

// Name returns the group's declared name.
func (g Group[S]) Name() string { return g.name }

// Members returns the group's states, in declaration order.
func (g Group[S]) Members() []S { return slices.Clone(g.members) }

// Has reports whether s is a member of the group. It is the flat equivalent of
// a hierarchical "is the machine anywhere inside this superstate".
func (g Group[S]) Has(s S) bool { return slices.Contains(g.members, s) }

func (g Group[S]) applyTo(b *builder[S]) { _ = b.declareGroup(g) }

// declareGroup records a group once, rejecting a name reused for a different
// set of members. It reports whether the group is usable, so that a broken one
// produces a single error rather than one per rule that mentions it.
func (b *builder[S]) declareGroup(g Group[S]) bool {
	if g.name == "" {
		b.errs = append(b.errs, errors.New("group declared with no name"))
		return false
	}
	if len(g.members) == 0 {
		b.errs = append(b.errs, fmt.Errorf("group %s has no members", g.name))
		return false
	}
	// A repeated member would expand twice and read as the group clashing
	// with itself.
	for i, s := range g.members {
		if slices.Index(g.members, s) != i {
			b.errs = append(b.errs, fmt.Errorf("group %s lists %v twice", g.name, s))
			return false
		}
	}

	if i := slices.IndexFunc(b.m.groups, func(h Group[S]) bool { return h.name == g.name }); i >= 0 {
		if !slices.Equal(b.m.groups[i].members, g.members) {
			b.errs = append(b.errs, fmt.Errorf(
				"group %s declared twice with different members: %v and %v",
				g.name, b.m.groups[i].members, g.members))
			return false
		}
		return true
	}
	b.m.groups = append(b.m.groups, g)
	return true
}

// Edge is a registered transition, reported by [Machine.Edges].
type Edge[S comparable] struct {
	From S
	To   S

	// Event is the trigger's name, for display. Names are not unique —
	// compare with [Edge.Is] to identify the trigger.
	Event string

	Guard string // guard description, empty when unguarded

	// Group names the group this edge was inherited from, and is empty for a
	// directly declared one. A group transition expands to one edge per
	// member, so this is what tells the expansion apart from N hand-written
	// rows.
	Group string

	trigger *eventDef
}

// Is reports whether ev is the trigger of this edge.
//
// Branch on this rather than on [Edge.Event], for the reason given on
// [Transition.Is]: events are identified by declaration, not by name.
func (e Edge[S]) Is[A any](ev Event[A]) bool {
	return e.trigger != nil && e.trigger == ev.def
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
