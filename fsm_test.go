package fsm_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/floatdrop/fsm"
)

type state int

const (
	idle state = iota
	running
	done
	cancelled
)

func (s state) String() string {
	switch s {
	case idle:
		return "idle"
	case running:
		return "running"
	case done:
		return "done"
	case cancelled:
		return "cancelled"
	}
	return "unknown"
}

var (
	evStart  = fsm.Signal("start")
	evFinish = fsm.Define[int]("finish")
	evCancel = fsm.Signal("cancel")
)

func linear(t *testing.T) *fsm.Machine[state] {
	t.Helper()
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
		fsm.From(running).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return m
}

func TestFireAdvancesState(t *testing.T) {
	m := linear(t)
	ctx := t.Context()

	st := idle
	if err := m.Send(ctx, &st, evStart); err != nil {
		t.Fatalf("start: %v", err)
	}
	if st != running {
		t.Fatalf("after start: got %v, want running", st)
	}
	if err := m.Fire(ctx, &st, evFinish, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if st != done {
		t.Fatalf("after finish: got %v, want done", st)
	}
}

func TestFireRejectsUnknownTransition(t *testing.T) {
	m := linear(t)
	st := idle

	err := m.Fire(t.Context(), &st, evFinish, 0)
	if err == nil {
		t.Fatal("expected an error firing finish from idle")
	}

	nte, ok := errors.AsType[*fsm.NoTransitionError[state]](err)
	if !ok {
		t.Fatalf("got %T, want *fsm.NoTransitionError", err)
	}
	if nte.From != idle || nte.Event != "finish" {
		t.Errorf("error describes %v on %q, want idle on \"finish\"", nte.From, nte.Event)
	}
	if st != idle {
		t.Errorf("state changed to %v on a failed transition", st)
	}
}

func TestPayloadReachesAction(t *testing.T) {
	var got int
	record := func(_ context.Context, code int) error {
		got = code
		return nil
	}
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done).Action(record),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	st := running
	if err := m.Fire(t.Context(), &st, evFinish, 42); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got != 42 {
		t.Errorf("action saw %d, want 42", got)
	}
}

var errNonZeroExit = errors.New("exit code was not zero")

func zeroExit(_ context.Context, code int) error {
	if code != 0 {
		return fmt.Errorf("%w: got %d", errNonZeroExit, code)
	}
	return nil
}

func guarded(t *testing.T) *fsm.Machine[state] {
	t.Helper()
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done).Guard("exit code is zero", zeroExit),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return m
}

func TestGuardBlocksTransition(t *testing.T) {
	m := guarded(t)
	ctx := t.Context()

	st := running
	err := m.Fire(ctx, &st, evFinish, 1)

	ge, ok := errors.AsType[*fsm.GuardError[state]](err)
	if !ok {
		t.Fatalf("got %v (%T), want *fsm.GuardError", err, err)
	}
	if ge.Guard != "exit code is zero" {
		t.Errorf("guard description %q, want %q", ge.Guard, "exit code is zero")
	}
	if st != running {
		t.Errorf("state changed to %v despite a failed guard", st)
	}

	if err := m.Fire(ctx, &st, evFinish, 0); err != nil {
		t.Fatalf("finish with passing guard: %v", err)
	}
	if st != done {
		t.Errorf("got %v, want done", st)
	}
}

// The point of returning an error rather than a bool: the caller can match
// the guard's own reason, not just learn that something was refused.
func TestGuardErrorUnwrapsToTheGuardsError(t *testing.T) {
	m := guarded(t)
	ctx := t.Context()

	st := running
	err := m.Fire(ctx, &st, evFinish, 3)

	if !errors.Is(err, errNonZeroExit) {
		t.Errorf("errors.Is(err, errNonZeroExit) = false for %v", err)
	}
	if !strings.Contains(err.Error(), "got 3") {
		t.Errorf("error %q lost the guard's dynamic detail", err)
	}

	if ge, ok := errors.AsType[*fsm.GuardError[state]](err); ok && !errors.Is(ge.Unwrap(), errNonZeroExit) {
		t.Error("GuardError.Unwrap did not return the guard's error")
	}
}

func TestCheckReportsReasonWithoutFiring(t *testing.T) {
	m := guarded(t)
	ctx := t.Context()

	if err := m.Check(ctx, running, evFinish, 1); !errors.Is(err, errNonZeroExit) {
		t.Errorf("Check returned %v, want it to wrap errNonZeroExit", err)
	}
	if err := m.Check(ctx, running, evFinish, 0); err != nil {
		t.Errorf("Check on a passing guard returned %v, want nil", err)
	}

	// Check reports a missing transition the same way Fire does.
	missing := m.Check(ctx, idle, evFinish, 0)
	if _, ok := errors.AsType[*fsm.NoTransitionError[state]](missing); !ok {
		t.Errorf("Check from idle returned %v, want *fsm.NoTransitionError", missing)
	}

	if m.Can(ctx, running, evFinish, 1) {
		t.Error("Can reported true for a guard that rejects")
	}
	if !m.Can(ctx, running, evFinish, 0) {
		t.Error("Can reported false for a guard that accepts")
	}
}

// A guard that rejects must not run the action behind it.
func TestGuardRunsBeforeAction(t *testing.T) {
	acted := false
	m, err := fsm.New("job",
		fsm.From(running).On(evFinish).To(done).
			Guard("never", func(context.Context, int) error { return errNonZeroExit }).
			Action(func(context.Context, int) error { acted = true; return nil }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	st := running
	if err := m.Fire(t.Context(), &st, evFinish, 1); !errors.Is(err, errNonZeroExit) {
		t.Fatalf("got %v, want errNonZeroExit", err)
	}
	if acted {
		t.Error("action ran behind a rejecting guard")
	}
}

// With several guards the first rejection wins, and it is reported under the
// description it was registered with.
func TestFirstRejectingGuardWins(t *testing.T) {
	errSecond := errors.New("second")
	m, err := fsm.New("job",
		fsm.From(running).On(evFinish).To(done).
			Guard("first", func(_ context.Context, code int) error {
				if code < 0 {
					return errNonZeroExit
				}
				return nil
			}).
			Guard("second", func(context.Context, int) error { return errSecond }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := t.Context()
	st := running

	err = m.Fire(ctx, &st, evFinish, -1)
	if ge, ok := errors.AsType[*fsm.GuardError[state]](err); ok {
		if ge.Guard != "first" || !errors.Is(err, errNonZeroExit) {
			t.Errorf("got guard %q / %v, want the first guard to reject", ge.Guard, err)
		}
	} else {
		t.Fatalf("got %v, want *fsm.GuardError", err)
	}

	err = m.Fire(ctx, &st, evFinish, 1)
	if ge, ok := errors.AsType[*fsm.GuardError[state]](err); ok {
		if ge.Guard != "second" || !errors.Is(err, errSecond) {
			t.Errorf("got guard %q / %v, want the second guard to reject", ge.Guard, err)
		}
	} else {
		t.Fatalf("got %v, want *fsm.GuardError", err)
	}
}

func TestActionErrorAbortsTransition(t *testing.T) {
	boom := errors.New("boom")
	var hooks []string

	m, err := fsm.New("job",
		fsm.OnExit(running, func(context.Context, fsm.Transition[state]) { hooks = append(hooks, "exit") }),
		fsm.OnEnter(done, func(context.Context, fsm.Transition[state]) { hooks = append(hooks, "enter") }),
		fsm.From(running).On(evFinish).To(done).Action(func(context.Context, int) error { return boom }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	st := running
	err = m.Fire(t.Context(), &st, evFinish, 0)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want it to wrap boom", err)
	}
	if st != running {
		t.Errorf("state changed to %v after a failing action", st)
	}
	if len(hooks) != 0 {
		t.Errorf("hooks ran despite a failing action: %v", hooks)
	}
}

// Hooks must bracket the assignment: exit sees the old state, enter sees the
// new one. Anything that pairs an increment with a decrement depends on it.
func TestHooksBracketTheAssignment(t *testing.T) {
	var order []string

	m, err := fsm.New("job",
		fsm.OnExit(running, func(_ context.Context, tr fsm.Transition[state]) {
			order = append(order, "exit:"+tr.From.String()+"->"+tr.To.String())
		}),
		fsm.OnEnter(done, func(_ context.Context, tr fsm.Transition[state]) {
			order = append(order, "enter:"+tr.From.String()+"->"+tr.To.String())
		}),
		fsm.From(running).On(evFinish).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	st := running
	if err := m.Fire(t.Context(), &st, evFinish, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}

	want := []string{"exit:running->done", "enter:running->done"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("hook order %v, want %v", order, want)
	}
}

// The bug class this package exists to remove: a gauge maintained by hand at
// every call site that changes the state.
func TestGaugeStaysPaired(t *testing.T) {
	counts := map[state]int{}
	inc := func(s state) func(context.Context) { return func(context.Context) { counts[s]++ } }
	dec := func(s state) func(context.Context) { return func(context.Context) { counts[s]-- } }

	m, err := fsm.New("job",
		fsm.Gauge(running, inc(running), dec(running)),
		fsm.Gauge(done, inc(done), dec(done)),
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := t.Context()

	for range 100 {
		st := idle
		if err := m.Send(ctx, &st, evStart); err != nil {
			t.Fatal(err)
		}
		// Firing an event the state does not accept must not move a gauge.
		_ = m.Send(ctx, &st, evStart)
		if err := m.Fire(ctx, &st, evFinish, 0); err != nil {
			t.Fatal(err)
		}
	}

	if counts[running] != 0 {
		t.Errorf("running gauge drifted to %d, want 0", counts[running])
	}
	if counts[done] != 100 {
		t.Errorf("done gauge is %d, want 100", counts[done])
	}
}

func TestBuildRejectsDuplicateTransition(t *testing.T) {
	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(idle).On(evStart).To(cancelled),
	)
	if err == nil {
		t.Fatal("expected a duplicate-transition error")
	}
	if !strings.Contains(err.Error(), "duplicate transition") {
		t.Errorf("error %q does not mention the duplicate", err)
	}
}

// Rules are plain values, so a set shared by several machines can be declared
// once and reused. Applying the same rule twice must not let one machine see
// the other's edges.
func TestRulesAreReusableValues(t *testing.T) {
	shared := []fsm.Rule[state]{
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evCancel).To(cancelled),
	}

	short, err := fsm.New("short", shared...)
	if err != nil {
		t.Fatalf("build short: %v", err)
	}
	long, err := fsm.New("long", slices.Concat(shared, []fsm.Rule[state]{
		fsm.From(running).On(evFinish).To(done),
	})...)
	if err != nil {
		t.Fatalf("build long: %v", err)
	}

	if got := len(short.Edges()); got != 2 {
		t.Errorf("short has %d edges, want 2", got)
	}
	if got := len(long.Edges()); got != 3 {
		t.Errorf("long has %d edges, want 3", got)
	}

	// The extra edge belongs to long alone.
	if _, ok := short.To(running, evFinish); ok {
		t.Error("short picked up an edge declared only for long")
	}
	if _, ok := long.To(running, evFinish); !ok {
		t.Error("long is missing its own edge")
	}
}

func TestBuildRejectsZeroEvent(t *testing.T) {
	var zero fsm.Event[int]

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(zero).To(done),
	)
	if err == nil {
		t.Fatal("expected an error for a transition on the zero Event")
	}
	if !strings.Contains(err.Error(), "zero Event") {
		t.Errorf("error %q does not explain the problem", err)
	}
}

// A nil guard used to be dropped in silence, taking its description out of
// the DOT label with it: the machine read as guarded and ran unguarded.
func TestBuildRejectsNilGuard(t *testing.T) {
	var nilGuard func(context.Context, int) error

	_, err := fsm.New("job",
		fsm.From(running).On(evFinish).To(done).Guard("never runs", nilGuard),
	)
	if err == nil {
		t.Fatal("expected an error for a nil guard")
	}
	for _, want := range []string{"never runs", "nil", "running -> done"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBuildRejectsNilAction(t *testing.T) {
	var nilAction func(context.Context, int) error

	_, err := fsm.New("job",
		fsm.From(running).On(evFinish).To(done).Action(nilAction),
	)
	if err == nil {
		t.Fatal("expected an error for a nil action")
	}
	if !strings.Contains(err.Error(), "action is nil") {
		t.Errorf("error %q does not explain the problem", err)
	}
}

func TestBuildRejectsNilRule(t *testing.T) {
	_, err := fsm.New[state]("job",
		fsm.From(idle).On(evStart).To(running),
		nil,
	)
	if err == nil {
		t.Fatal("expected an error for a nil rule")
	}
	if !strings.Contains(err.Error(), "rule 1 is nil") {
		t.Errorf("error %q does not say which rule", err)
	}
}

// Guards and actions attach to the value To returns, so a stored transition
// picks them up whether or not the result is reassigned.
func TestChainedOptionsMutateInPlace(t *testing.T) {
	blocked := false
	tr := fsm.From(running).On(evFinish).To(done)
	tr.Guard("blocked", func(context.Context, int) error {
		blocked = true
		return errNonZeroExit
	})

	m, err := fsm.New("job", tr) // note: tr, not the result of Guard
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	st := running
	if err := m.Fire(t.Context(), &st, evFinish, 0); !errors.Is(err, errNonZeroExit) {
		t.Fatalf("got %v, want the guard to reject", err)
	}
	if !blocked {
		t.Error("the guard attached to a stored transition never ran")
	}
}

func TestBuildRejectsEmptyMachine(t *testing.T) {
	if _, err := fsm.New[state]("job"); err == nil {
		t.Fatal("expected an error for a machine with no transitions")
	}
}

func TestZeroEventIsAnErrorNotAPanic(t *testing.T) {
	m := linear(t)
	var zero fsm.Event[int]

	st := idle
	err := m.Fire(t.Context(), &st, zero, 0)
	if err == nil {
		t.Fatal("expected an error for the zero Event")
	}
	if !strings.Contains(err.Error(), "zero Event") {
		t.Errorf("error %q does not explain the problem", err)
	}
}

func TestTerminalsAndReachability(t *testing.T) {
	m := linear(t)

	terminals := m.Terminals()
	if len(terminals) != 2 || terminals[0] != done || terminals[1] != cancelled {
		t.Errorf("terminals %v, want [done cancelled]", terminals)
	}
	if got := m.Unreachable(idle); len(got) != 0 {
		t.Errorf("unreachable from idle: %v, want none", got)
	}
	if got := m.Unreachable(done); len(got) != 3 {
		t.Errorf("unreachable from done: %v, want 3 states", got)
	}
}

func TestDOTIsDeterministic(t *testing.T) {
	m := linear(t)

	first := m.DOT()
	for range 50 {
		if got := m.DOT(); got != first {
			t.Fatal("DOT output varies between calls")
		}
	}
	for _, want := range []string{`digraph "job"`, `"idle" -> "running"`, `"done" [shape=doublecircle]`} {
		if !strings.Contains(first, want) {
			t.Errorf("DOT output missing %q:\n%s", want, first)
		}
	}
}

// Fire is on the hot path of whatever owns the state, so it must not allocate.
// Combining guards and actions at build time — where the payload type is still
// known — is what keeps the payload off the heap.
func TestFireDoesNotAllocate(t *testing.T) {
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running).Guard("always", func(context.Context, fsm.Unit) error { return nil }),
		fsm.From(running).On(evFinish).To(idle).Action(func(context.Context, int) error { return nil }),
		fsm.OnEnter(running, func(context.Context, fsm.Transition[state]) {}),
		fsm.OnExit(running, func(context.Context, fsm.Transition[state]) {}),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := t.Context()
	st := idle

	avg := testing.AllocsPerRun(1000, func() {
		_ = m.Send(ctx, &st, evStart)
		_ = m.Fire(ctx, &st, evFinish, 7)
	})
	if avg != 0 {
		t.Errorf("Fire allocates %.1f times per round trip, want 0", avg)
	}
}

func BenchmarkFire(b *testing.B) {
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(idle).Action(func(context.Context, int) error { return nil }),
	)
	if err != nil {
		b.Fatal(err)
	}
	ctx := b.Context()
	st := idle

	b.ReportAllocs()
	for b.Loop() {
		_ = m.Send(ctx, &st, evStart)
		_ = m.Fire(ctx, &st, evFinish, 1)
	}
}
