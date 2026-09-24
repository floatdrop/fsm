// Package benchmarks compares this package's fire path with two widely used
// Go state machines on the same two-state cycle.
//
// Every benchmark does the same work per iteration: two transitions, a -> b
// and b -> a, on a machine that was built once. Read the numbers with the
// semantic differences in mind — they are listed in README.md, and the largest
// is that looplab and stateless hold the state, so one machine serves one
// entity, while a fsm.Machine is immutable configuration shared by all of them.
package benchmarks

import (
	"context"
	"testing"

	fsm "github.com/floatdrop/fsm"
	looplab "github.com/looplab/fsm"
	stateless "github.com/qmuntal/stateless"
)

type state int

const (
	a state = iota
	b
)

func (s state) String() string { return [...]string{"a", "b"}[s] }

var (
	evAB = fsm.Signal("ab")
	evBA = fsm.Signal("ba")

	evABn = fsm.Define[int]("ab")
	evBAn = fsm.Define[int]("ba")
)

// --- plain: no guards, no actions, no payload ------------------------------

func BenchmarkPlain_fsm(bench *testing.B) {
	m := fsm.MustNew("bench",
		fsm.From(a).On(evAB).To(b),
		fsm.From(b).On(evBA).To(a),
	)
	ctx, st := bench.Context(), a

	bench.ReportAllocs()
	for bench.Loop() {
		_, _ = m.Send(ctx, &st, evAB)
		_, _ = m.Send(ctx, &st, evBA)
	}
}

func BenchmarkPlain_looplab(bench *testing.B) {
	m := looplab.NewFSM("a", looplab.Events{
		{Name: "ab", Src: []string{"a"}, Dst: "b"},
		{Name: "ba", Src: []string{"b"}, Dst: "a"},
	}, nil)
	ctx := bench.Context()

	bench.ReportAllocs()
	for bench.Loop() {
		_ = m.Event(ctx, "ab")
		_ = m.Event(ctx, "ba")
	}
}

func BenchmarkPlain_stateless(bench *testing.B) {
	m := stateless.NewStateMachineWithMode("a", stateless.FiringImmediate)
	m.Configure("a").Permit("ab", "b")
	m.Configure("b").Permit("ba", "a")
	ctx := bench.Context()

	bench.ReportAllocs()
	for bench.Loop() {
		_ = m.FireCtx(ctx, "ab")
		_ = m.FireCtx(ctx, "ba")
	}
}

// --- a guard that passes and an action on each transition ------------------

func BenchmarkGuarded_fsm(bench *testing.B) {
	pass := func(context.Context, fsm.Unit) error { return nil }
	m := fsm.MustNew("bench",
		fsm.From(a).On(evAB).To(b).Guard("ok", pass).Action(pass),
		fsm.From(b).On(evBA).To(a).Guard("ok", pass).Action(pass),
	)
	ctx, st := bench.Context(), a

	bench.ReportAllocs()
	for bench.Loop() {
		_, _ = m.Send(ctx, &st, evAB)
		_, _ = m.Send(ctx, &st, evBA)
	}
}

func BenchmarkGuarded_looplab(bench *testing.B) {
	// before_<event> can cancel the transition, so it is the guard;
	// after_<event> is the action.
	noop := func(context.Context, *looplab.Event) {}
	m := looplab.NewFSM("a", looplab.Events{
		{Name: "ab", Src: []string{"a"}, Dst: "b"},
		{Name: "ba", Src: []string{"b"}, Dst: "a"},
	}, looplab.Callbacks{
		"before_ab": noop, "after_ab": noop,
		"before_ba": noop, "after_ba": noop,
	})
	ctx := bench.Context()

	bench.ReportAllocs()
	for bench.Loop() {
		_ = m.Event(ctx, "ab")
		_ = m.Event(ctx, "ba")
	}
}

func BenchmarkGuarded_stateless(bench *testing.B) {
	pass := func(context.Context, ...any) bool { return true }
	act := func(context.Context, ...any) error { return nil }
	m := stateless.NewStateMachineWithMode("a", stateless.FiringImmediate)
	m.Configure("a").Permit("ab", "b", pass).OnEntryFrom("ba", act)
	m.Configure("b").Permit("ba", "a", pass).OnEntryFrom("ab", act)
	ctx := bench.Context()

	bench.ReportAllocs()
	for bench.Loop() {
		_ = m.FireCtx(ctx, "ab")
		_ = m.FireCtx(ctx, "ba")
	}
}

// --- carrying a payload ----------------------------------------------------

func BenchmarkPayload_fsm(bench *testing.B) {
	seen := 0
	keep := func(_ context.Context, n int) error { seen = n; return nil }
	m := fsm.MustNew("bench",
		fsm.From(a).On(evABn).To(b).Action(keep),
		fsm.From(b).On(evBAn).To(a).Action(keep),
	)
	ctx, st := bench.Context(), a

	bench.ReportAllocs()
	for bench.Loop() {
		_, _ = m.Fire(ctx, &st, evABn, 7)
		_, _ = m.Fire(ctx, &st, evBAn, 7)
	}
	_ = seen
}

func BenchmarkPayload_looplab(bench *testing.B) {
	seen := 0
	keep := func(_ context.Context, e *looplab.Event) { seen = e.Args[0].(int) }
	m := looplab.NewFSM("a", looplab.Events{
		{Name: "ab", Src: []string{"a"}, Dst: "b"},
		{Name: "ba", Src: []string{"b"}, Dst: "a"},
	}, looplab.Callbacks{"after_ab": keep, "after_ba": keep})
	ctx := bench.Context()

	bench.ReportAllocs()
	for bench.Loop() {
		_ = m.Event(ctx, "ab", 7)
		_ = m.Event(ctx, "ba", 7)
	}
	_ = seen
}

func BenchmarkPayload_stateless(bench *testing.B) {
	seen := 0
	keep := func(_ context.Context, args ...any) error { seen = args[0].(int); return nil }
	m := stateless.NewStateMachineWithMode("a", stateless.FiringImmediate)
	m.Configure("a").Permit("ab", "b").OnEntryFrom("ba", keep)
	m.Configure("b").Permit("ba", "a").OnEntryFrom("ab", keep)
	ctx := bench.Context()

	bench.ReportAllocs()
	for bench.Loop() {
		_ = m.FireCtx(ctx, "ab", 7)
		_ = m.FireCtx(ctx, "ba", 7)
	}
	_ = seen
}
