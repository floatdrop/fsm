package fsm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Rule is one declaration a machine is made of: a transition, a hook, a group
// or the initial state. Rules are values, so a set of them can be built up in
// a loop, stored, and shared between machines.
//
// Only this package implements Rule. Declare one with [From], [FromEach],
// [FromGroup], [OnEnter], [OnExit], [OnEnterVia], [OnExitVia], [OnEnterWith],
// [OnExitWith], [OnTransition], [NewGroup], [Initial] or [Rules].
type Rule[S comparable] interface {
	applyTo(*builder[S])
}

// Rules bundles several rules into one, so a set shared between machines can
// be passed to [New] beside other rules without splicing slices.
func Rules[S comparable](rules ...Rule[S]) Rule[S] {
	rules = slices.Clone(rules)
	return ruleFunc[S](func(b *builder[S]) { b.apply("bundled rule", rules) })
}

// apply runs each rule, reporting a nil one as "<what> <index> is nil".
func (b *builder[S]) apply(what string, rules []Rule[S]) {
	for i, r := range rules {
		if r == nil {
			b.errs = append(b.errs, fmt.Errorf("%s %d is nil", what, i))
			continue
		}
		r.applyTo(b)
	}
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
	// carries. [OnEnterWith] needs to see the whole table, so it is recorded
	// here and read in a second pass.
	decls []decl[S]

	// Group transitions, which add edges and so must run before the rules
	// that read the finished table.
	expand []func(*builder[S])

	// Rules that must run once every transition is known.
	deferred []func(*builder[S])

	// Whether the definition had errors before the deferred phase, so a
	// deferred check can stay quiet about rows a broken rule dropped.
	broken bool

	// Which group each inherited row came from, so that two groups claiming
	// the same (state, event) is reported rather than read as an override.
	inherited map[edge[S]]string

	// Targets of the group transitions declared so far, so that one declared
	// twice reads as a duplicate rather than as a group clashing with itself.
	groupEdges map[groupEdge]string // what the first declaration does

	// Group hooks in declaration order, attached to the rows that cross
	// their group's boundary once the table is finished.
	groupHooks []groupHookDecl[S]

	// Hooks as declared, which buildPlans turns into each row's plan.
	onEnter, onExit       map[S][]Hook[S]
	onEnterVia, onExitVia map[edge[S]][]any // keyed by (state, event); func(context.Context, Transition[S], A)
	onAll                 []Hook[S]
	groupEnter, groupExit map[edge[S]][]call[S] // keyed by row, in the order they run
	internal              map[edge[S]]bool      // the rows declared with Stay
}

// groupHookDecl is one declared group hook, before it is attached to rows.
type groupHookDecl[S comparable] struct {
	index   int    // in declaration order
	what    string // the declaring function, for errors
	group   Group[S]
	enter   bool
	hook    call[S]
	payload any // (*A)(nil) for a typed hook, nil for a plain one
}

// groupEdge identifies one group transition, before it expands to a row per
// member.
type groupEdge struct {
	group string
	ev    *eventDef
}

type decl[S comparable] struct {
	key      edge[S]
	to       S
	internal bool // an internal transition enters and leaves nothing
	payload  any  // (*A)(nil) for the event's payload type A
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
			name:    name,
			table:   make(map[edge[S]]S),
			guards:  make(map[edge[S]]any),
			actions: make(map[edge[S]]any),
			plans:   make(map[edge[S]]*plan[S]),
		},
		onEnter:    make(map[S][]Hook[S]),
		onExit:     make(map[S][]Hook[S]),
		onEnterVia: make(map[edge[S]][]any),
		onExitVia:  make(map[edge[S]][]any),
		internal:   make(map[edge[S]]bool),
		groupEnter: make(map[edge[S]][]call[S]),
		groupExit:  make(map[edge[S]][]call[S]),
		seen:       make(map[S]bool),
		inherited:  make(map[edge[S]]string),
		groupEdges: make(map[groupEdge]string),
	}

	b.apply("rule", rules)

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
	// OnEnterWith can be declared before the edges it applies to.
	for _, e := range b.expand {
		e(b)
	}
	b.broken = len(b.errs) > 0
	for _, d := range b.deferred {
		d(b)
	}
	b.attachGroupHooks()
	b.buildPlans()

	if len(b.m.table) == 0 {
		b.errs = append(b.errs, errors.New("no transitions declared"))
	}
	b.m.hasHooks = len(b.m.plans) > 0
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
// entry hooks. That is UML's external self-transition, and it means a counter
// kept by those hooks decrements and increments back to where it was. For an
// event handled without leaving the state, see [OnStep.Stay].
func (o OnStep[S, A]) To(to S) *ToStep[S, A] {
	return &ToStep[S, A]{froms: o.froms, group: o.group, ev: o.ev, to: to}
}

// Stay completes the transition as an internal one: the state takes the
// event and stays where it is, UML's internal transition.
//
//	fsm.From(follower).On(evHeartbeat).Stay().Action(recordLeader)
//
// Its guards and actions run as on any transition, and [Machine.Fire]
// returns a Transition from the state to itself; but the state is not left,
// so no hook runs, [OnTransition] included. That is what an external
// self-transition, To naming its own source, cannot do. From [FromEach] or
// [FromGroup], each source stays in itself.
//
// An internal transition is still a transition: it is what the state does
// with the event, rather than refuse it. It is not a way out, so it does not
// keep the state out of [Machine.Terminals].
func (o OnStep[S, A]) Stay() *ToStep[S, A] {
	return &ToStep[S, A]{froms: o.froms, group: o.group, ev: o.ev, internal: true}
}

// ToStep is a complete transition, optionally carrying guards and actions.
// It satisfies [Rule].
//
// Its methods mutate and return the same value, so a guard attached to a
// stored ToStep takes effect whether or not the result is reassigned.
type ToStep[S comparable, A any] struct {
	froms    []S
	group    *Group[S]
	to       S
	internal bool // declared with Stay: each source keeps its state
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
	where := t.describe()
	for _, err := range t.errs {
		b.errs = append(b.errs, fmt.Errorf("%s: %w", where, err))
	}

	if t.ev.def == nil {
		b.errs = append(b.errs, fmt.Errorf("%s: zero Event; declare it with fsm.Define or fsm.Signal", where))
		return
	}

	r := row[S]{
		to:       t.to,
		internal: t.internal,
		ev:       t.ev.def,
		payload:  payloadToken[A](),
		desc:     joinDescs(t.descs),
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
		if _, dup := b.m.table[edge[S]{from: s, ev: r.ev}]; dup {
			b.errs = append(b.errs, fmt.Errorf("duplicate transition from %v on %s: already %s, redeclared to %s",
				s, r.ev.name, b.does(edge[S]{from: s, ev: r.ev}), t.redeclared()))
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
			"duplicate transition from group %s on %s: already %s, redeclared to %s",
			g.name, r.ev.name, prev, t.redeclared()))
		return
	}
	b.groupEdges[gk] = t.does()

	// The target is a state of the machine whether or not any member ends up
	// inheriting the edge, and declaring it now lets member validation see it.
	if !t.internal {
		b.declare(t.to)
	}

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

// describe names the rule in error messages: one source prints as a -> b,
// several as [a b] -> c, and an internal transition as internal transition a.
func (t *ToStep[S, A]) describe() string {
	var from string
	switch {
	case t.group != nil:
		from = "group " + t.group.name
	case len(t.froms) == 1:
		from = fmt.Sprint(t.froms[0])
	default:
		from = fmt.Sprint(t.froms)
	}
	if t.internal {
		return "internal transition " + from
	}
	return fmt.Sprintf("transition %s -> %v", from, t.to)
}

// does and redeclared say what the transition does, for a duplicate's error
// message: "already goes to b, redeclared to c", "already stays, redeclared
// to c", "already goes to b, redeclared to stay".
func (t *ToStep[S, A]) does() string {
	if t.internal {
		return "stays"
	}
	return fmt.Sprintf("goes to %v", t.to)
}

func (t *ToStep[S, A]) redeclared() string {
	if t.internal {
		return "stay"
	}
	return fmt.Sprint(t.to)
}

// does says what the registered row e does, as ToStep.does.
func (b *builder[S]) does(e edge[S]) string {
	if b.internal[e] {
		return "stays"
	}
	return fmt.Sprintf("goes to %v", b.m.table[e])
}

// row is one line of the transition table. Its fields are named because a
// positional call with three states and three any values would be unreadable
// and easy to transpose.
type row[S comparable] struct {
	from, to S
	internal bool // to is from, and no hook runs
	ev       *eventDef
	guard    any // func(context.Context, A) (string, error)
	action   any // func(context.Context, A) error
	payload  any // (*A)(nil) for the event's payload type
	desc     string
	group    string // the group it was inherited from, empty if declared directly
}

// register adds one row to the transition table.
func (b *builder[S]) register(r row[S]) {
	if r.internal {
		r.to = r.from
	}
	e := edge[S]{from: r.from, ev: r.ev}
	b.m.table[e] = r.to
	if r.internal {
		b.internal[e] = true
	}
	b.decls = append(b.decls, decl[S]{key: e, to: r.to, internal: r.internal, payload: r.payload})
	b.declare(r.from)
	b.declare(r.to)

	if r.guard != nil {
		b.m.guards[e] = r.guard
	}
	if r.action != nil {
		b.m.actions[e] = r.action
	}
	b.m.edges = append(b.m.edges, Edge[S]{
		From: r.from, To: r.to, Guard: r.desc, Group: r.group, Internal: r.internal, trigger: r.ev,
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
		b.onEnter[s] = append(b.onEnter[s], h)
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
		b.onExit[s] = append(b.onExit[s], h)
		b.declare(s)
	})
}

// OnTransition declares a hook that runs after every transition that leaves
// a state — every one but an internal transition ([OnStep.Stay]) — just after
// the assignment and before the entry hooks of the new state, so an entry
// hook that fires the machine again is logged after the transition that
// caused it. It is where an audit log or a trace that must see every change
// goes, instead of one [OnEnter] per state.
//
// A transition hook observes. It must not fire the machine on the same
// state: the entry hooks of the new state have not run yet, so a nested
// transition would leave that state before entering it.
func OnTransition[S comparable](h Hook[S]) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if h == nil {
			b.errs = append(b.errs, errors.New("nil OnTransition hook"))
			return
		}
		b.onAll = append(b.onAll, h)
	})
}

// Initial declares the state a fresh instance starts in. The machine still
// holds no state, so nothing changes at fire time; it lets [New] reject a
// machine in which some state cannot be reached from the start, or whose
// start has no way out, and lets [Machine.DOT] mark where the machine begins.
//
// Entering the initial state is not a transition, so no hook runs for it and
// the first increment of a counter kept by entry hooks is the caller's.
func Initial[S comparable](s S) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if b.m.hasInitial {
			b.errs = append(b.errs, fmt.Errorf("initial state declared twice: %v and %v", b.m.initial, s))
			return
		}
		b.m.initial, b.m.hasInitial = s, true
		b.declare(s)

		b.deferred = append(b.deferred, func(b *builder[S]) {
			// A definition with mistakes has states that exist only because of
			// them; reachability is assessed once it builds.
			if len(b.errs) > 0 || len(b.m.table) == 0 {
				return
			}
			// A dead start makes every other state unreachable; report the cause.
			if slices.Contains(b.m.Terminals(), s) {
				if slices.ContainsFunc(b.m.edges, func(e Edge[S]) bool { return e.From == s }) {
					b.errs = append(b.errs, fmt.Errorf("initial state %v has no way out: its transitions are all internal", s))
				} else {
					b.errs = append(b.errs, fmt.Errorf("initial state %v has no outgoing transition", s))
				}
				return
			}
			if u := b.m.Unreachable(s); len(u) > 0 {
				b.errs = append(b.errs, fmt.Errorf("states %v cannot be reached from initial state %v", u, s))
			}
		})
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
// runs after the plain entry hooks of the same state. [New] rejects it when
// no transition on ev enters s, since it could never run.
func OnEnterVia[S comparable, A any](s S, ev Event[A], h func(context.Context, Transition[S], A)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		if !b.checkVia("OnEnterVia", s, ev.def, h == nil) {
			return
		}
		k := edge[S]{from: s, ev: ev.def}
		b.onEnterVia[k] = append(b.onEnterVia[k], h)
		b.requireVia("OnEnterVia", "enters", s, ev.def, func(d decl[S]) bool { return d.to == s && !d.internal })
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
		b.onExitVia[k] = append(b.onExitVia[k], h)
		b.requireVia("OnExitVia", "leaves", s, ev.def, func(d decl[S]) bool { return d.key.from == s && !d.internal })
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

// requireVia defers a check that some transition on def matches, so a Via
// hook that can never run is reported rather than silently dead. The hook does
// not declare s: an edge does, so a mistyped state is reported here alone and
// not also as unreachable.
func (b *builder[S]) requireVia(what, verb string, s S, def *eventDef, match func(decl[S]) bool) {
	b.deferred = append(b.deferred, func(b *builder[S]) {
		// A broken rule may be what dropped the row; report it alone.
		if b.broken {
			return
		}
		if !slices.ContainsFunc(b.decls, func(d decl[S]) bool { return d.key.ev == def && match(d) }) {
			b.errs = append(b.errs, fmt.Errorf(
				"%s for state %v: no transition on %s %s it, so the hook would never run", what, s, def.name, verb))
		}
	})
}

// OnEnterWith declares a hook that runs just after the machine enters s, on
// every event that enters it, and hands the hook that event's payload:
//
//	fsm.OnEnterWith(jobRunning, func(_ context.Context, _ fsm.Transition[jobState], j *job) {
//		running.WithLabelValues(j.tenant).Inc()
//	})
//
// It is [OnEnterVia] for every such event at once. The events are read from
// the finished transition table, group-inherited edges included, so one added
// later is covered without another declaration. That requires every event
// entering s to carry payload type A: [New] reports one that does not, rather
// than let the hook silently miss it. Where the payloads differ, name each
// event with OnEnterVia instead.
//
// Paired with [OnExitWith] it keeps a count of what is in s, labelled by the
// payload. Hooks cannot fail and run only once the transition is certain, so
// the increment and its decrement cannot come apart. Entering the initial
// state is not a transition, so the first increment is the caller's.
//
// [New] rejects a With hook no transition can trigger, so a terminal state,
// which nothing leaves, takes OnEnterWith without the OnExitWith half.
//
// It runs after the plain and the [OnEnterVia] entry hooks of the same state.
func OnEnterWith[S comparable, A any](s S, h func(context.Context, Transition[S], A)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		b.attachWith("OnEnterWith", "enters", s, h, b.onEnterVia, func(d decl[S]) bool { return d.to == s && !d.internal })
	})
}

// OnExitWith declares a hook that runs just before the machine leaves s, on
// every event that leaves it, and hands the hook that event's payload. See
// [OnEnterWith]. It runs after the plain and the [OnExitVia] exit hooks of
// the same state, and still before the state changes.
func OnExitWith[S comparable, A any](s S, h func(context.Context, Transition[S], A)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		b.attachWith("OnExitWith", "leaves", s, h, b.onExitVia, func(d decl[S]) bool { return d.key.from == s && !d.internal })
	})
}

// attachWith attaches h to every event on a row that match selects, once the
// table is finished, so declaration order does not matter.
func (b *builder[S]) attachWith[A any](what, verb string, s S, h func(context.Context, Transition[S], A),
	hooks map[edge[S]][]any, match func(decl[S]) bool) {
	if h == nil {
		b.errs = append(b.errs, fmt.Errorf("nil %s hook for state %v", what, s))
		return
	}
	b.deferred = append(b.deferred, func(b *builder[S]) {
		want := payloadToken[A]()
		var touched bool
		// Rows fanning in on one event share its (s, event) key and its
		// payload type: one hook and at most one error per event.
		seen := make(map[*eventDef]bool)
		for _, d := range b.decls {
			if !match(d) || seen[d.key.ev] {
				continue
			}
			seen[d.key.ev] = true
			touched = true
			if d.payload != want {
				b.errs = append(b.errs, fmt.Errorf(
					"%s for state %v: transition %v --%s--> %v carries a different payload type, so the hook would miss it",
					what, s, d.key.from, d.key.ev.name, d.to))
				continue
			}
			k := edge[S]{from: s, ev: d.key.ev}
			hooks[k] = append(hooks[k], h)
		}
		// A broken rule may be what dropped the row; report it alone.
		if !touched && !b.broken {
			b.errs = append(b.errs, fmt.Errorf(
				"%s for state %v: no transition %s it, so the hook would never run", what, s, verb))
		}
	})
}

// OnEnterGroup declares a hook that runs when the machine enters g from a
// state outside it: just after the [OnTransition] hooks, before the entry
// hooks of the state entered. A move between members of g crosses no
// boundary and does not run it, nor does an external self-transition of a
// member, so a count of what is in the group, kept by it and
// [OnExitGroup], never dips on an intra-group move.
//
// When a transition enters several groups at once, the larger group's hooks
// run first, as a superstate's entry runs before its substate's; a group's
// own hooks run in declaration order. Entering the initial state is not a
// transition, so no group hook runs for it.
//
// A group entry hook must not fire the machine on the same state: the state
// entered has not run its entry hooks yet. [New] rejects one that no
// transition could run.
func OnEnterGroup[S comparable](g Group[S], h Hook[S]) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		b.declareGroupHook("OnEnterGroup", g, true, call[S]{plain: h}, nil, h == nil)
	})
}

// OnExitGroup declares a hook that runs when the machine leaves g for a
// state outside it: just after the exit hooks of the state left, before the
// assignment. When a transition leaves several groups, the smaller group's
// hooks run first. See [OnEnterGroup].
func OnExitGroup[S comparable](g Group[S], h Hook[S]) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		b.declareGroupHook("OnExitGroup", g, false, call[S]{plain: h}, nil, h == nil)
	})
}

// OnEnterGroupWith is [OnEnterGroup] with the payload of the event that
// entered g, which every event entering g must carry: [New] reports one
// that does not, as for [OnEnterWith].
func OnEnterGroupWith[S comparable, A any](g Group[S], h func(context.Context, Transition[S], A)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		b.declareGroupHook("OnEnterGroupWith", g, true, call[S]{typed: h}, payloadToken[A](), h == nil)
	})
}

// OnExitGroupWith is [OnExitGroup] with the payload of the event that left
// g, which every event leaving g must carry.
func OnExitGroupWith[S comparable, A any](g Group[S], h func(context.Context, Transition[S], A)) Rule[S] {
	return ruleFunc[S](func(b *builder[S]) {
		b.declareGroupHook("OnExitGroupWith", g, false, call[S]{typed: h}, payloadToken[A](), h == nil)
	})
}

// declareGroupHook records a group hook, declaring its group, for
// attachGroupHooks to attach once the table is finished.
func (b *builder[S]) declareGroupHook(what string, g Group[S], enter bool, h call[S], payload any, nilHook bool) {
	if nilHook {
		b.errs = append(b.errs, fmt.Errorf("nil %s hook for group %s", what, g.name))
		return
	}
	if !b.declareGroup(g) {
		return
	}
	b.groupHooks = append(b.groupHooks, groupHookDecl[S]{
		index: len(b.groupHooks), what: what, group: g, enter: enter, hook: h, payload: payload,
	})
}

// attachGroupHooks gives every row the group hooks it runs: those of the
// groups it leaves, smallest first, and of the groups it enters, largest
// first, each group's in declaration order. It runs last, against the
// finished table, so group-inherited rows count and declaration order does
// not matter.
func (b *builder[S]) attachGroupHooks() {
	if len(b.groupHooks) == 0 {
		return
	}
	// Groups from outermost to innermost: larger first, and of two the same
	// size the one declared first, as DOT nests them. Entering runs their
	// hooks in that order and leaving in its reverse, a group's own hooks in
	// declaration order either way.
	rank := func(g Group[S]) int {
		return slices.IndexFunc(b.m.groups, func(h Group[S]) bool { return h.name == g.name })
	}
	byNesting := func(outerFirst bool) []groupHookDecl[S] {
		out := slices.Clone(b.groupHooks)
		slices.SortStableFunc(out, func(x, y groupHookDecl[S]) int {
			c := cmp.Or(cmp.Compare(len(y.group.members), len(x.group.members)), cmp.Compare(rank(x.group), rank(y.group)))
			if !outerFirst {
				c = -c
			}
			return c
		})
		return out
	}
	touched := make([]bool, len(b.groupHooks))
	// One error per hook and event: rows fanning in on an event share its
	// payload type.
	type hookEvent struct {
		hook int
		ev   *eventDef
	}
	reported := make(map[hookEvent]bool)
	attach := func(decls []groupHookDecl[S], enter bool) {
		for _, d := range b.decls {
			if d.internal {
				continue
			}
			for _, h := range decls {
				if h.enter != enter || h.group.Has(d.key.from) == enter || h.group.Has(d.to) != enter {
					continue
				}
				touched[h.index] = true
				if h.payload != nil && h.payload != d.payload {
					if k := (hookEvent{h.index, d.key.ev}); !reported[k] {
						reported[k] = true
						b.errs = append(b.errs, fmt.Errorf(
							"%s for group %s: transition %v --%s--> %v carries a different payload type, so the hook would miss it",
							h.what, h.group.name, d.key.from, d.key.ev.name, d.to))
					}
					continue
				}
				hooks := b.groupExit
				if enter {
					hooks = b.groupEnter
				}
				hooks[d.key] = append(hooks[d.key], h.hook)
			}
		}
	}
	attach(byNesting(false), false)
	attach(byNesting(true), true)
	if b.broken {
		return // a broken rule may be what dropped the rows
	}
	for i, h := range b.groupHooks {
		if !touched[i] {
			verb := "leaves"
			if h.enter {
				verb = "enters"
			}
			b.errs = append(b.errs, fmt.Errorf(
				"%s for group %s: no transition %s it, so the hook would never run", h.what, h.group.name, verb))
		}
	}
}

// buildPlans gives every row the hooks it runs, in order: the exit hooks of
// its source, plain, Via and With, then of the groups it leaves; and after
// the assignment the transition hooks, the entry hooks of the groups it
// enters, then of its target, plain, Via and With. An internal row runs
// none.
func (b *builder[S]) buildPlans() {
	plain := func(hs []Hook[S]) []call[S] {
		out := make([]call[S], len(hs))
		for i, h := range hs {
			out[i] = call[S]{plain: h}
		}
		return out
	}
	typed := func(hs []any) []call[S] {
		out := make([]call[S], len(hs))
		for i, h := range hs {
			out[i] = call[S]{typed: h}
		}
		return out
	}
	for _, d := range b.decls {
		if d.internal {
			continue
		}
		p := &plan[S]{
			exit: slices.Concat(plain(b.onExit[d.key.from]), typed(b.onExitVia[d.key]), b.groupExit[d.key]),
			enter: slices.Concat(plain(b.onAll), b.groupEnter[d.key],
				plain(b.onEnter[d.to]), typed(b.onEnterVia[edge[S]{from: d.to, ev: d.key.ev}])),
		}
		if len(p.exit) > 0 || len(p.enter) > 0 {
			b.m.plans[d.key] = p
		}
	}
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
// pays for it beyond the hooks declared on it ([OnEnterGroup], [OnExitGroup]),
// which run only on the transitions that cross its boundary.
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

	Guard string // guard description, empty when unguarded

	// Group names the group this edge was inherited from, and is empty for a
	// directly declared one. A group transition expands to one edge per
	// member, so this is what tells the expansion apart from N hand-written
	// rows.
	Group string

	// Internal is set on an internal transition, declared with
	// [OnStep.Stay]: To is From, and no hook runs.
	Internal bool

	trigger *eventDef
}

// Event returns the trigger's name, for display. Names are not unique;
// compare with [Edge.Is] to identify the trigger.
func (e Edge[S]) Event() string {
	if e.trigger == nil {
		return ""
	}
	return e.trigger.name
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

// joinDescs is the edge's label: the non-empty guard descriptions, joined.
func joinDescs(descs []string) string {
	kept := slices.DeleteFunc(slices.Clone(descs), func(d string) bool { return d == "" })
	return strings.Join(kept, " && ")
}
