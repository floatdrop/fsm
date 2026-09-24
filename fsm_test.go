package fsm_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// The payload may alias the state. An action that writes it is reported,
// nothing is assigned and no hook runs.
func TestActionWritingTheStateIsAnError(t *testing.T) {
	t.Parallel()

	st := running
	var exited []state
	errBoom := errors.New("boom")

	m, err := fsm.New("job",
		fsm.OnExit(running, func(_ context.Context, tr fsm.Transition[state]) { exited = append(exited, tr.From) }),
		fsm.From(running).On(evFinish).To(done).Action(func(context.Context, int) error {
			st = cancelled
			return nil
		}),
		fsm.From(running).On(evCancel).To(cancelled).Action(func(context.Context, fsm.Unit) error {
			st = idle
			return errBoom
		}),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := t.Context()

	_, err = m.Fire(ctx, &st, evFinish, 1)
	sce, ok := errors.AsType[*fsm.StateChangedError[state]](err)
	if !ok || sce.From != running || sce.To != done || sce.Found != cancelled {
		t.Fatalf("got %v, want a StateChangedError for running --finish--> done finding cancelled", err)
	}
	if st != cancelled || len(exited) != 0 {
		t.Errorf("state %v, exited %v; want the action's write kept and no hook run", st, exited)
	}

	// Writing the target is no exception: a nested fire that lands there would
	// otherwise run every hook twice.
	st = running
	m2, err := fsm.New("job",
		fsm.From(running).On(evFinish).To(done).Action(func(context.Context, int) error { st = done; return nil }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := m2.Fire(ctx, &st, evFinish, 1); !errors.As(err, new(*fsm.StateChangedError[state])) {
		t.Errorf("got %v, want a StateChangedError for a write of the target", err)
	}

	// A guard that writes the state is reported ahead of its own rejection.
	st = running
	m3, err := fsm.New("job",
		fsm.From(running).On(evFinish).To(done).Guard("never", func(context.Context, int) error {
			st = idle
			return errBoom
		}),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = m3.Fire(ctx, &st, evFinish, 1)
	if err == nil || !strings.Contains(err.Error(), "state changed to idle during running --finish--> done") {
		t.Errorf("got %v, want the guard's write reported", err)
	}
	// The rejection is kept but not unwrapped: matching its sentinel would
	// read as "state untouched, retry".
	if sce, ok := errors.AsType[*fsm.StateChangedError[state]](err); !ok || sce.Guard != "never" || sce.Err != errBoom || errors.Is(err, errBoom) {
		t.Errorf("error %q: want the guard and its rejection kept, not unwrapped", err)
	}
	if !strings.Contains(err.Error(), `guard "never": boom`) {
		t.Errorf("error %q does not name the rejecting guard", err)
	}

	// So is an action's write ahead of its failure: an ActionError would claim
	// the state untouched.
	st = running
	_, err = m.Send(ctx, &st, evCancel)
	sce, ok = errors.AsType[*fsm.StateChangedError[state]](err)
	if !ok || sce.From != running || sce.To != cancelled || sce.Found != idle || sce.Event != "cancel" {
		t.Fatalf("got %v, want a StateChangedError for running --cancel--> cancelled finding idle", err)
	}
	if sce.Err != errBoom || errors.Is(err, errBoom) || !strings.Contains(err.Error(), "running --cancel--> cancelled: boom") {
		t.Errorf("error %q: want the action's error in Err and the message, not unwrapped", err)
	}
	if st != idle || len(exited) != 0 {
		t.Errorf("state %v, exited %v; want the action's write kept and no hook run", st, exited)
	}
}

func TestFireReturnsTheTransition(t *testing.T) {
	t.Parallel()

	m := linear(t)
	ctx := t.Context()
	st := idle

	tr, err := m.Send(ctx, &st, evStart)
	if err != nil {
		t.Fatal(err)
	}
	if tr.From != idle || tr.To != running || tr.Event() != "start" || !tr.Is(evStart) {
		t.Errorf("transition %+v, want idle --start--> running", tr)
	}

	// A refused fire returns the zero Transition.
	tr, err = m.Send(ctx, &st, evStart)
	if err == nil || tr != (fsm.Transition[state]{}) {
		t.Errorf("got %+v, %v; want the zero transition and an error", tr, err)
	}
}

func TestOnTransitionRunsAfterEntryHooks(t *testing.T) {
	t.Parallel()

	var order []string
	note := func(what string) fsm.Hook[state] {
		return func(context.Context, fsm.Transition[state]) { order = append(order, what) }
	}
	m, err := fsm.New("job",
		fsm.OnTransition(func(_ context.Context, tr fsm.Transition[state]) {
			order = append(order, fmt.Sprintf("%v->%v", tr.From, tr.To))
		}),
		fsm.OnExit(idle, note("exit idle")),
		fsm.OnEnter(running, note("enter running")),
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := t.Context()
	st := idle

	if _, err := m.Send(ctx, &st, evStart); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fire(ctx, &st, evFinish, 1); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Send(ctx, &st, evStart) // refused: no hook runs

	want := []string{"exit idle", "idle->running", "enter running", "running->done"}
	if !slices.Equal(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
}

func TestOnTransitionRejectsNil(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job", fsm.OnTransition[state](nil), fsm.From(idle).On(evStart).To(running))
	if err == nil || !strings.Contains(err.Error(), "nil OnTransition hook") {
		t.Fatalf("got %v, want a nil hook error", err)
	}
}

func TestRulesBundlesRules(t *testing.T) {
	t.Parallel()

	shared := fsm.Rules(
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evCancel).To(cancelled),
	)
	m, err := fsm.New("job", shared, fsm.From(running).On(evFinish).To(done))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := len(m.Edges()); got != 3 {
		t.Errorf("%d edges, want 3", got)
	}

	_, err = fsm.New("job", fsm.Rules(fsm.From(idle).On(evStart).To(running), nil))
	if err == nil || !strings.Contains(err.Error(), "bundled rule 1 is nil") {
		t.Fatalf("got %v, want the nil rule reported", err)
	}
}

func TestEventsListsEachNameOnce(t *testing.T) {
	t.Parallel()

	twin := fsm.Signal("start") // same name, different event
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
		fsm.From(running).On(evStart).To(running),
		fsm.From(done).On(twin).To(idle),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, want := m.Events(), []string{"start", "finish"}; !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// --- Initial ---------------------------------------------------------------

func TestInitialIsReportedAndDrawn(t *testing.T) {
	t.Parallel()

	if _, ok := linear(t).Initial(); ok {
		t.Error("a machine without Initial reports one")
	}
	if strings.Contains(linear(t).DOT(), "__start") {
		t.Error("a machine without Initial draws a start marker")
	}

	m, err := fsm.New("job",
		fsm.Initial(idle),
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
		fsm.From(running).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if s, ok := m.Initial(); !ok || s != idle {
		t.Errorf("Initial() = %v, %v; want idle, true", s, ok)
	}
	want := `digraph "job" {
	rankdir=LR;
	"__start" [shape=point];
	"idle" [shape=box];
	"running" [shape=box];
	"done" [shape=doublecircle];
	"cancelled" [shape=doublecircle];
	"__start" -> "idle";
	"idle" -> "running" [label="start"];
	"running" -> "done" [label="finish"];
	"running" -> "cancelled" [label="cancel"];
}
`
	if got := m.DOT(); got != want {
		t.Errorf("DOT:\n%s\nwant:\n%s", got, want)
	}
}

func TestInitialStartMarkerAvoidsAStateName(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.Initial("__start"),
		fsm.From("__start").On(evStart).To("going"),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	dot := m.DOT()
	for _, want := range []string{`"__start_" [shape=point];`, `"__start_" -> "__start";`} {
		if !strings.Contains(dot, want) {
			t.Errorf("DOT lacks %s:\n%s", want, dot)
		}
	}
}

// An entry hook that fires again is logged after the transition it came from.
func TestOnTransitionKeepsCausalOrder(t *testing.T) {
	t.Parallel()

	var log []string
	st := idle
	ctx := t.Context()
	var m *fsm.Machine[state]
	m, err := fsm.New("job",
		fsm.OnTransition(func(_ context.Context, tr fsm.Transition[state]) {
			log = append(log, fmt.Sprintf("%v->%v", tr.From, tr.To))
		}),
		fsm.OnEnter(running, func(ctx context.Context, _ fsm.Transition[state]) { _, _ = m.Fire(ctx, &st, evFinish, 1) }),
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := m.Send(ctx, &st, evStart); err != nil {
		t.Fatal(err)
	}
	if want := []string{"idle->running", "running->done"}; st != done || !slices.Equal(log, want) {
		t.Errorf("state %v, log %v; want done and %v", st, log, want)
	}
}

func TestInitialDoesNotReportAMistakeTwice(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.Initial(idle),
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(fsm.NewGroup("g", running, state(99))).On(evCancel).To(cancelled),
	)
	if err == nil || !strings.Contains(err.Error(), "member unknown is not a state") {
		t.Fatalf("got %v, want the mistyped member reported", err)
	}
	if strings.Contains(err.Error(), "cannot be reached") {
		t.Errorf("error %q also reports the typo's state as unreachable", err)
	}
}

func TestInitialRejectsUnreachableStates(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.Initial(idle),
		fsm.From(idle).On(evStart).To(running),
		fsm.From(done).On(evCancel).To(cancelled), // nothing reaches done
	)
	if err == nil || !strings.Contains(err.Error(), "states [done cancelled] cannot be reached from initial state idle") {
		t.Fatalf("got %v, want the unreachable states reported", err)
	}
}

func TestInitialRejectsADeadStart(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job", fsm.Initial(done), fsm.From(idle).On(evStart).To(done))
	if err == nil || !strings.Contains(err.Error(), "initial state done has no outgoing transition") {
		t.Fatalf("got %v, want a dead start reported", err)
	}
	if strings.Contains(err.Error(), "cannot be reached") {
		t.Errorf("error %q also reports the consequence of the dead start", err)
	}
}

func TestInitialDeclaredTwice(t *testing.T) {
	t.Parallel()

	for _, second := range []state{idle, running} {
		_, err := fsm.New("job", fsm.Initial(idle), fsm.Initial(second), fsm.From(idle).On(evStart).To(running))
		want := fmt.Sprintf("initial state declared twice: idle and %v", second)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want %q", err, want)
		}
	}
}

func TestInitialWithNoTransitionsReportsOneError(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job", fsm.Initial(idle))
	if err == nil || !strings.Contains(err.Error(), "no transitions declared") {
		t.Fatalf("got %v, want the empty machine reported", err)
	}
	if strings.Contains(err.Error(), "no outgoing transition") {
		t.Errorf("error %q also reports the initial state as a dead start", err)
	}
}

func TestFireAdvancesState(t *testing.T) {
	t.Parallel()

	m := linear(t)
	ctx := t.Context()

	st := idle
	if _, err := m.Send(ctx, &st, evStart); err != nil {
		t.Fatalf("start: %v", err)
	}
	if st != running {
		t.Fatalf("after start: got %v, want running", st)
	}
	if _, err := m.Fire(ctx, &st, evFinish, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if st != done {
		t.Fatalf("after finish: got %v, want done", st)
	}
}

func TestFireRejectsUnknownTransition(t *testing.T) {
	t.Parallel()

	m := linear(t)
	st := idle

	_, err := m.Fire(t.Context(), &st, evFinish, 0)
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
	t.Parallel()

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
	if _, err := m.Fire(t.Context(), &st, evFinish, 42); err != nil {
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
	t.Parallel()

	m := guarded(t)
	ctx := t.Context()

	st := running
	_, err := m.Fire(ctx, &st, evFinish, 1)

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

	if _, err := m.Fire(ctx, &st, evFinish, 0); err != nil {
		t.Fatalf("finish with passing guard: %v", err)
	}
	if st != done {
		t.Errorf("got %v, want done", st)
	}
}

// The point of returning an error rather than a bool: the caller can match
// the guard's own reason, not just learn that something was refused.
func TestGuardErrorUnwrapsToTheGuardsError(t *testing.T) {
	t.Parallel()

	m := guarded(t)
	ctx := t.Context()

	st := running
	_, err := m.Fire(ctx, &st, evFinish, 3)

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
	t.Parallel()

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
	t.Parallel()

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
	if _, err := m.Fire(t.Context(), &st, evFinish, 1); !errors.Is(err, errNonZeroExit) {
		t.Fatalf("got %v, want errNonZeroExit", err)
	}
	if acted {
		t.Error("action ran behind a rejecting guard")
	}
}

// With several guards the first rejection wins, and it is reported under the
// description it was registered with.
func TestFirstRejectingGuardWins(t *testing.T) {
	t.Parallel()

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

	_, err = m.Fire(ctx, &st, evFinish, -1)
	if ge, ok := errors.AsType[*fsm.GuardError[state]](err); ok {
		if ge.Guard != "first" || !errors.Is(err, errNonZeroExit) {
			t.Errorf("got guard %q / %v, want the first guard to reject", ge.Guard, err)
		}
	} else {
		t.Fatalf("got %v, want *fsm.GuardError", err)
	}

	_, err = m.Fire(ctx, &st, evFinish, 1)
	if ge, ok := errors.AsType[*fsm.GuardError[state]](err); ok {
		if ge.Guard != "second" || !errors.Is(err, errSecond) {
			t.Errorf("got guard %q / %v, want the second guard to reject", ge.Guard, err)
		}
	} else {
		t.Fatalf("got %v, want *fsm.GuardError", err)
	}
}

func TestActionErrorAbortsTransition(t *testing.T) {
	t.Parallel()

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
	_, err = m.Fire(t.Context(), &st, evFinish, 0)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want it to wrap boom", err)
	}
	ae, ok := errors.AsType[*fsm.ActionError[state]](err)
	if !ok || ae.From != running || ae.To != done || ae.Event != "finish" {
		t.Errorf("got %v, want an ActionError for running --finish--> done", err)
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
	t.Parallel()

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
	if _, err := m.Fire(t.Context(), &st, evFinish, 0); err != nil {
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
	t.Parallel()

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
		if _, err := m.Send(ctx, &st, evStart); err != nil {
			t.Fatal(err)
		}
		// Firing an event the state does not accept must not move a gauge.
		_, _ = m.Send(ctx, &st, evStart)
		if _, err := m.Fire(ctx, &st, evFinish, 0); err != nil {
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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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

// Two events can share a name, so a hook that branches on Transition.Event
// cannot tell them apart. Is compares the trigger's identity.
func TestTransitionIsIdentifiesTheTrigger(t *testing.T) {
	t.Parallel()

	first, second := fsm.Signal("tick"), fsm.Signal("tick")

	var names []string
	var matchedFirst, matchedSecond int
	m, err := fsm.New("job",
		fsm.OnEnter(running, func(_ context.Context, tr fsm.Transition[state]) {
			names = append(names, tr.Event())
			if tr.Is(first) {
				matchedFirst++
			}
			if tr.Is(second) {
				matchedSecond++
			}
		}),
		fsm.From(idle).On(first).To(running),
		fsm.From(done).On(second).To(running),
		fsm.From(running).On(evFinish).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx := t.Context()
	st := idle
	if _, err := m.Send(ctx, &st, first); err != nil { // idle -> running
		t.Fatal(err)
	}
	if _, err := m.Fire(ctx, &st, evFinish, 0); err != nil { // running -> done
		t.Fatal(err)
	}
	if _, err := m.Send(ctx, &st, second); err != nil { // done -> running
		t.Fatal(err)
	}

	// The name cannot distinguish them; the identity can.
	if got := strings.Join(names, ","); got != "tick,tick" {
		t.Errorf("hook saw events %q, want \"tick,tick\"", got)
	}
	if matchedFirst != 1 || matchedSecond != 1 {
		t.Errorf("Is matched first=%d second=%d, want 1 and 1", matchedFirst, matchedSecond)
	}
}

func TestTransitionIsRejectsTheZeroEvent(t *testing.T) {
	t.Parallel()

	var zero fsm.Event[int]
	if (fsm.Transition[state]{}).Is(zero) {
		t.Error("the zero Transition matched the zero Event")
	}
}

// A state can be entered by events carrying different payloads, so a plain
// hook cannot be typed. Naming the event fixes A and hands the hook the
// payload that caused the transition.
func TestOnEnterViaSeesThePayload(t *testing.T) {
	t.Parallel()

	var got []int
	var plain int
	m, err := fsm.New("job",
		fsm.OnEnter(done, func(context.Context, fsm.Transition[state]) { plain++ }),
		fsm.OnEnterVia(done, evFinish, func(_ context.Context, tr fsm.Transition[state], code int) {
			if tr.To != done {
				t.Errorf("hook ran for a transition into %v", tr.To)
			}
			got = append(got, code)
		}),
		fsm.From(running).On(evFinish).To(done),
		fsm.From(running).On(evCancel).To(done), // same target, different event
		fsm.From(done).On(evStart).To(running),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx := t.Context()
	st := running
	if _, err := m.Fire(ctx, &st, evFinish, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(ctx, &st, evStart); err != nil {
		t.Fatal(err)
	}
	// Entering done by another event must not run the evFinish hook.
	if _, err := m.Send(ctx, &st, evCancel); err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0] != 7 {
		t.Errorf("payload hook saw %v, want [7]", got)
	}
	if plain != 2 {
		t.Errorf("plain hook ran %d times, want 2", plain)
	}
}

func TestOnExitViaRunsBeforeTheStateChanges(t *testing.T) {
	t.Parallel()

	var seen []string
	m, err := fsm.New("job",
		fsm.OnExit(running, func(context.Context, fsm.Transition[state]) { seen = append(seen, "plain") }),
		fsm.OnExitVia(running, evFinish, func(_ context.Context, tr fsm.Transition[state], code int) {
			seen = append(seen, fmt.Sprintf("via:%d:%v->%v", code, tr.From, tr.To))
		}),
		fsm.From(running).On(evFinish).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	st := running
	if _, err := m.Fire(t.Context(), &st, evFinish, 3); err != nil {
		t.Fatal(err)
	}
	want := "plain,via:3:running->done"
	if got := strings.Join(seen, ","); got != want {
		t.Errorf("hooks ran %q, want %q", got, want)
	}
}

func TestBuildRejectsBadViaHooks(t *testing.T) {
	t.Parallel()

	var zero fsm.Event[int]

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.OnEnterVia(done, evFinish, nil),
		fsm.OnExitVia(done, zero, func(context.Context, fsm.Transition[state], int) {}),
	)
	if err == nil {
		t.Fatal("expected errors for a nil hook and a zero Event")
	}
	for _, want := range []string{"nil OnEnterVia hook", "OnExitVia", "zero Event"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A self-transition is UML's external kind: it runs exit and entry, so a
// gauge on the state dips and comes back rather than standing still.
func TestSelfTransitionRunsExitAndEntry(t *testing.T) {
	t.Parallel()

	var seq []string
	gauge, low := 0, 0
	m, err := fsm.New("job",
		fsm.Gauge(running,
			func(context.Context) { gauge++ },
			func(context.Context) { gauge--; low = min(low, gauge) },
		),
		fsm.OnExit(running, func(context.Context, fsm.Transition[state]) { seq = append(seq, "exit") }),
		fsm.OnEnter(running, func(context.Context, fsm.Transition[state]) { seq = append(seq, "enter") }),
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evCancel).To(running), // self-transition
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx := t.Context()
	st := idle
	if _, err := m.Send(ctx, &st, evStart); err != nil {
		t.Fatal(err)
	}
	seq = nil // drop the entry from idle -> running

	if _, err := m.Send(ctx, &st, evCancel); err != nil {
		t.Fatalf("self-transition: %v", err)
	}
	if st != running {
		t.Errorf("state is %v, want running", st)
	}
	if got := strings.Join(seq, ","); got != "exit,enter" {
		t.Errorf("self-transition ran %q, want \"exit,enter\"", got)
	}
	if gauge != 1 {
		t.Errorf("gauge settled at %d, want 1", gauge)
	}
	if low != 0 {
		t.Errorf("gauge dipped to %d during the self-transition, want it to reach 0", low)
	}
}

// A counter labelled by the thing the transition is about cannot be closed
// over when the machine is built. GaugeWith takes it from the payload and is
// still declared once per state, so the pair stays bound together.
func TestGaugeWithCountsPerPayload(t *testing.T) {
	t.Parallel()

	type tally struct{ n map[state]int }
	ev := fsm.Define[*tally]("go")
	back := fsm.Define[*tally]("back")

	m, err := fsm.New("job",
		fsm.GaugeWith(running,
			func(_ context.Context, x *tally) { x.n[running]++ },
			func(_ context.Context, x *tally) { x.n[running]-- },
		),
		fsm.From(idle).On(ev).To(running),
		fsm.From(running).On(back).To(idle),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx := t.Context()
	// Two independent instances share one machine and keep separate counts.
	a, b := &tally{n: map[state]int{}}, &tally{n: map[state]int{}}
	sa, sb := idle, idle

	for range 3 {
		if _, err := m.Fire(ctx, &sa, ev, a); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Fire(ctx, &sa, back, a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Fire(ctx, &sb, ev, b); err != nil {
		t.Fatal(err)
	}

	if a.n[running] != 0 {
		t.Errorf("a settled at %d after balanced trips, want 0", a.n[running])
	}
	if b.n[running] != 1 {
		t.Errorf("b is %d while in running, want 1", b.n[running])
	}
}

// The increment and decrement come from one declaration, so no call site can
// supply one without the other.
func TestGaugeWithStaysPairedAcrossEveryEdge(t *testing.T) {
	t.Parallel()

	type tally struct{ n int }
	in1 := fsm.Define[*tally]("in1")
	in2 := fsm.Define[*tally]("in2")
	out := fsm.Define[*tally]("out")

	m, err := fsm.New("job",
		fsm.GaugeWith(running,
			func(_ context.Context, x *tally) { x.n++ },
			func(_ context.Context, x *tally) { x.n-- },
		),
		fsm.From(idle).On(in1).To(running),
		fsm.From(done).On(in2).To(running), // a second way in
		fsm.From(running).On(out).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx := t.Context()
	x := &tally{}
	st := idle
	for _, step := range []struct {
		ev   fsm.Event[*tally]
		want int
	}{{in1, 1}, {out, 0}, {in2, 1}, {out, 0}} {
		if _, err := m.Fire(ctx, &st, step.ev, x); err != nil {
			t.Fatalf("fire %s: %v", step.ev.Name(), err)
		}
		if x.n != step.want {
			t.Errorf("after %s count is %d, want %d", step.ev.Name(), x.n, step.want)
		}
	}
}

// A state reached by events carrying different payloads cannot have its
// counter kept paired, so New says so instead of skipping an edge.
func TestGaugeWithRejectsMixedPayloads(t *testing.T) {
	t.Parallel()

	type tally struct{}
	typed := fsm.Define[*tally]("typed")
	other := fsm.Define[int]("other")

	_, err := fsm.New("job",
		fsm.GaugeWith(running,
			func(context.Context, *tally) {},
			func(context.Context, *tally) {},
		),
		fsm.From(idle).On(typed).To(running),
		fsm.From(done).On(other).To(running), // different payload type
	)
	if err == nil {
		t.Fatal("expected an error for a state entered by two payload types")
	}
	for _, want := range []string{"GaugeWith for state running", "other", "different payload type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A mismatch is one error, not also "no transition enters or leaves it".
func TestGaugeWithMismatchReportsOneError(t *testing.T) {
	t.Parallel()

	type tally struct{}
	other := fsm.Define[int]("other")

	_, err := fsm.New("job",
		fsm.GaugeWith(running,
			func(context.Context, *tally) {},
			func(context.Context, *tally) {},
		),
		fsm.From(idle).On(other).To(running),
	)
	if err == nil || !strings.Contains(err.Error(), "different payload type") {
		t.Fatalf("got %v, want a payload mismatch", err)
	}
	if strings.Contains(err.Error(), "no transition enters or leaves it") {
		t.Errorf("error %q also reports the state as isolated", err)
	}
}

func TestGaugeWithRejectsAnIsolatedState(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.GaugeWith(cancelled,
			func(context.Context, int) {},
			func(context.Context, int) {},
		),
		fsm.From(idle).On(evFinish).To(running),
	)
	if err == nil || !strings.Contains(err.Error(), "no transition enters or leaves it") {
		t.Fatalf("got %v, want a complaint about an isolated state", err)
	}
}

// A Via hook no transition can trigger is a mistyped state or event.
func TestViaHookThatCanNeverRunIsAnError(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
		fsm.OnEnterVia(done, evStart, func(context.Context, fsm.Transition[state], fsm.Unit) {}),
		fsm.OnExitVia(idle, evFinish, func(context.Context, fsm.Transition[state], int) {}),
	)
	if err == nil {
		t.Fatal("built a machine with hooks that can never run")
	}
	for _, want := range []string{
		"OnEnterVia for state done: no transition on start enters it",
		"OnExitVia for state idle: no transition on finish leaves it",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Declaration order must not matter: the edges are read in a second pass.
func TestGaugeWithMayBeDeclaredBeforeItsEdges(t *testing.T) {
	t.Parallel()

	type tally struct{ n int }
	ev := fsm.Define[*tally]("go")

	first, err := fsm.New("first",
		fsm.GaugeWith(running, func(_ context.Context, x *tally) { x.n++ }, func(_ context.Context, x *tally) { x.n-- }),
		fsm.From(idle).On(ev).To(running),
	)
	if err != nil {
		t.Fatalf("gauge first: %v", err)
	}
	last, err := fsm.New("last",
		fsm.From(idle).On(ev).To(running),
		fsm.GaugeWith(running, func(_ context.Context, x *tally) { x.n++ }, func(_ context.Context, x *tally) { x.n-- }),
	)
	if err != nil {
		t.Fatalf("gauge last: %v", err)
	}

	ctx := t.Context()
	for _, m := range []*fsm.Machine[state]{first, last} {
		x := &tally{}
		st := idle
		if _, err := m.Fire(ctx, &st, ev, x); err != nil {
			t.Fatal(err)
		}
		if x.n != 1 {
			t.Errorf("%s: count %d, want 1", m.Name(), x.n)
		}
	}
}

// A nil guard used to be dropped in silence, taking its description out of
// the DOT label with it: the machine read as guarded and ran unguarded.
func TestBuildRejectsNilGuard(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	if _, err := m.Fire(t.Context(), &st, evFinish, 0); !errors.Is(err, errNonZeroExit) {
		t.Fatalf("got %v, want the guard to reject", err)
	}
	if !blocked {
		t.Error("the guard attached to a stored transition never ran")
	}
}

func TestBuildRejectsEmptyMachine(t *testing.T) {
	t.Parallel()

	if _, err := fsm.New[state]("job"); err == nil {
		t.Fatal("expected an error for a machine with no transitions")
	}
}

func TestZeroEventIsAnErrorNotAPanic(t *testing.T) {
	t.Parallel()

	m := linear(t)
	var zero fsm.Event[int]

	st := idle
	_, err := m.Fire(t.Context(), &st, zero, 0)
	if err == nil {
		t.Fatal("expected an error for the zero Event")
	}
	if !strings.Contains(err.Error(), "zero Event") {
		t.Errorf("error %q does not explain the problem", err)
	}
	if err := m.Check(t.Context(), idle, zero, 0); err == nil || !strings.Contains(err.Error(), "zero Event") {
		t.Errorf("Check: got %v, want the zero Event reported", err)
	}
	if m.Can(t.Context(), idle, zero, 0) {
		t.Error("Can allowed the zero Event")
	}
	if to, ok := m.To(idle, zero); ok {
		t.Errorf("To resolved %v for the zero Event", to)
	}
}

// --- Zero values, nil rules and error text --------------------------------

// The zero Event, Transition and Edge are values a caller can declare, so
// their accessors report rather than panic — and never claim a trigger.
func TestZeroValuesHaveNoTrigger(t *testing.T) {
	t.Parallel()

	var (
		ev fsm.Event[int]
		tr fsm.Transition[state]
		ed fsm.Edge[state]
	)

	if got := ev.Name(); got != "<invalid>" {
		t.Errorf("zero Event.Name() = %q, want %q", got, "<invalid>")
	}
	if got := ev.String(); got != "<invalid>" {
		t.Errorf("zero Event.String() = %q, want %q", got, "<invalid>")
	}
	if got := evStart.String(); got != "start" {
		t.Errorf("Event.String() = %q, want %q", got, "start")
	}
	if got := tr.Event(); got != "" {
		t.Errorf("zero Transition.Event() = %q, want empty", got)
	}
	if got := ed.Event(); got != "" {
		t.Errorf("zero Edge.Event() = %q, want empty", got)
	}
	if tr.Is(evStart) || ed.Is(evStart) {
		t.Error("a zero Transition or Edge claims a trigger")
	}
}

func TestFireRejectsANilStatePointer(t *testing.T) {
	t.Parallel()

	m := linear(t)

	_, err := m.Send(t.Context(), nil, evStart)
	if err == nil {
		t.Fatal("expected an error for a nil state pointer")
	}
	if !strings.Contains(err.Error(), "nil state pointer") {
		t.Errorf("error %q does not explain the problem", err)
	}
}

// The error text is what a caller reads in a log, so every fire-time error
// names the machine and the edge it happened on.
func TestFireErrorsNameTheirEdge(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	var st state

	m, err := fsm.New("job",
		fsm.From(running).On(evFinish).To(done).Action(func(context.Context, int) error { return errBoom }),
		// An unnamed guard drops the "guard %q" clause.
		fsm.From(running).On(evCancel).To(cancelled).Guard("", func(context.Context, fsm.Unit) error { return errBoom }),
		// A write with no error of its own drops the trailing reason.
		fsm.From(idle).On(evStart).To(running).Action(func(context.Context, fsm.Unit) error {
			st = done
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := m.Name(); got != "job" {
		t.Errorf("machine name %q, want %q", got, "job")
	}
	ctx := t.Context()

	for _, tc := range []struct {
		name string
		from state
		fire func(*state) error
		want string
	}{
		{"NoTransitionError", done, func(s *state) error { _, err := m.Send(ctx, s, evStart); return err },
			"fsm job: no transition from done on start"},
		{"ActionError", running, func(s *state) error { _, err := m.Fire(ctx, s, evFinish, 0); return err },
			"fsm job: action for running --finish--> done: boom"},
		{"GuardError", running, func(s *state) error { _, err := m.Send(ctx, s, evCancel); return err },
			"fsm job: transition running --cancel--> cancelled rejected: boom"},
		{"StateChangedError", idle, func(s *state) error { _, err := m.Send(ctx, s, evStart); return err },
			"fsm job: state changed to done during idle --start--> running"},
	} {
		// Not parallel: the write-the-state action closes over st.
		t.Run(tc.name, func(t *testing.T) {
			st = tc.from
			err := tc.fire(&st)
			if err == nil {
				t.Fatalf("want %q, got no error", tc.want)
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("error text:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// A nil hook is a build error, not a nil call at fire time. A gauge needs
// both halves, or the counter it exists to keep honest could only drift.
func TestBuildRejectsNilHooks(t *testing.T) {
	t.Parallel()

	tick := func(context.Context) {}
	tickWith := func(context.Context, int) {}

	for _, tc := range []struct {
		name string
		rule fsm.Rule[state]
		want string
	}{
		{"OnEnter", fsm.OnEnter[state](running, nil), "nil OnEnter hook for state running"},
		{"OnExit", fsm.OnExit[state](running, nil), "nil OnExit hook for state running"},
		{"Gauge without inc", fsm.Gauge(running, nil, tick), "Gauge for state running needs both inc and dec"},
		{"Gauge without dec", fsm.Gauge(running, tick, nil), "Gauge for state running needs both inc and dec"},
		{"GaugeWith without inc", fsm.GaugeWith(running, nil, tickWith), "GaugeWith for state running needs both inc and dec"},
		{"GaugeWith without dec", fsm.GaugeWith(running, tickWith, nil), "GaugeWith for state running needs both inc and dec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := fsm.New("job", fsm.From(idle).On(evStart).To(running), tc.rule)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// MustNew is for package-level variables: a bad definition must fail at
// process start rather than reach Fire.
func TestMustNewPanicsOnABadDefinition(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MustNew accepted a duplicate transition")
		}
		err, ok := r.(error)
		if !ok || !strings.Contains(err.Error(), "duplicate transition") {
			t.Errorf("panicked with %v, want the build error", r)
		}
	}()

	fsm.MustNew("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(idle).On(evStart).To(cancelled),
	)
}

func TestTerminalsAndReachability(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
		// Payload hooks run in the same path and must not box the payload.
		fsm.OnEnterVia(running, evStart, func(context.Context, fsm.Transition[state], fsm.Unit) {}),
		fsm.OnExitVia(running, evFinish, func(context.Context, fsm.Transition[state], int) {}),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := t.Context()
	st := idle

	avg := testing.AllocsPerRun(1000, func() {
		_, _ = m.Send(ctx, &st, evStart)
		_, _ = m.Fire(ctx, &st, evFinish, 7)
	})
	if avg != 0 {
		t.Errorf("Fire allocates %.1f times per round trip, want 0", avg)
	}
}

// The companion to BenchmarkFire: what hooks cost when a machine uses them.
func BenchmarkFireWithHooks(b *testing.B) {
	m, err := fsm.New("job",
		fsm.OnEnter(running, func(context.Context, fsm.Transition[state]) {}),
		fsm.OnExit(running, func(context.Context, fsm.Transition[state]) {}),
		fsm.OnEnterVia(running, evStart, func(context.Context, fsm.Transition[state], fsm.Unit) {}),
		fsm.OnExitVia(running, evFinish, func(context.Context, fsm.Transition[state], int) {}),
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(idle),
	)
	if err != nil {
		b.Fatal(err)
	}
	ctx := b.Context()
	st := idle

	b.ReportAllocs()
	for b.Loop() {
		_, _ = m.Send(ctx, &st, evStart)
		_, _ = m.Fire(ctx, &st, evFinish, 1)
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
		_, _ = m.Send(ctx, &st, evStart)
		_, _ = m.Fire(ctx, &st, evFinish, 1)
	}
}

// --- FromEach -------------------------------------------------------------

func TestFromEachRegistersOneEdgePerSource(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromEach(idle, running).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, from := range []state{idle, running} {
		st := from
		if _, err := m.Send(t.Context(), &st, evCancel); err != nil {
			t.Fatalf("cancel from %v: %v", from, err)
		}
		if st != cancelled {
			t.Errorf("cancel from %v left state %v, want cancelled", from, st)
		}
	}
	if got := len(m.Edges()); got != 3 {
		t.Errorf("Edges() reports %d edges, want 3", got)
	}
}

// The reason FromEach is variadic rather than From growing a second parameter:
// a group of sources computed elsewhere has to be spreadable.
func TestFromEachSpreadsASlice(t *testing.T) {
	t.Parallel()

	live := []state{idle, running}
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromEach(live...).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := m.To(running, evCancel); !ok {
		t.Error("no cancel transition from running")
	}
}

// Rules are values, so the slice a rule was built from must not keep affecting
// it afterwards.
func TestFromEachCopiesItsSources(t *testing.T) {
	t.Parallel()

	live := []state{idle, running}
	rule := fsm.FromEach(live...).On(evCancel).To(cancelled)
	live[1] = done

	m, err := fsm.New("job", fsm.From(idle).On(evStart).To(running), rule)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := m.To(running, evCancel); !ok {
		t.Error("mutating the source slice changed the rule")
	}
	if _, ok := m.To(done, evCancel); ok {
		t.Error("rule picked up a source written after it was declared")
	}
}

func TestFromEachWithNoSourcesIsAnError(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromEach[state]().On(evCancel).To(cancelled),
	)
	if err == nil {
		t.Fatal("expected an error for FromEach with no sources")
	}
	if !strings.Contains(err.Error(), "no source states") {
		t.Errorf("error does not mention the empty source list: %v", err)
	}
}

func TestFromEachReportsARepeatedSource(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job", fsm.FromEach(idle, idle).On(evStart).To(running))
	if err == nil {
		t.Fatal("expected an error for a source listed twice")
	}
	if !strings.Contains(err.Error(), "duplicate transition") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFromEachSharesGuardsAndActions(t *testing.T) {
	t.Parallel()

	var calls int
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromEach(idle, running).On(evCancel).To(cancelled).
			Guard("cancellable", func(context.Context, fsm.Unit) error { return nil }).
			Action(func(context.Context, fsm.Unit) error { calls++; return nil }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, from := range []state{idle, running} {
		st := from
		if _, err := m.Send(t.Context(), &st, evCancel); err != nil {
			t.Fatalf("cancel from %v: %v", from, err)
		}
	}
	if calls != 2 {
		t.Errorf("action ran %d times, want 2", calls)
	}
	for _, e := range m.Edges() {
		if e.Event() == "cancel" && e.Guard != "cancellable" {
			t.Errorf("edge from %v lost its guard description", e.From)
		}
	}
}

// --- Groups ---------------------------------------------------------------

var live = fsm.NewGroup("live", idle, running)

func TestGroupTransitionAppliesToEveryMember(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, from := range live.Members() {
		st := from
		if _, err := m.Send(t.Context(), &st, evCancel); err != nil {
			t.Fatalf("cancel from %v: %v", from, err)
		}
		if st != cancelled {
			t.Errorf("cancel from %v left state %v, want cancelled", from, st)
		}
	}
}

// The thing FromEach cannot do: a member handles the event its own way and the
// group's transition applies to the rest.
func TestGroupMemberOverridesInheritedEdge(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
		fsm.From(running).On(evCancel).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if to, _ := m.To(running, evCancel); to != done {
		t.Errorf("running cancels to %v, want done from its own rule", to)
	}
	if to, _ := m.To(idle, evCancel); to != cancelled {
		t.Errorf("idle cancels to %v, want cancelled from the group", to)
	}
}

// Declaration order must not matter: the override is found whether it is
// written before or after the group transition.
func TestGroupOverrideOrderDoesNotMatter(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(running).On(evCancel).To(done),
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if to, _ := m.To(running, evCancel); to != done {
		t.Errorf("running cancels to %v, want done", to)
	}
}

func TestGroupEdgesCarryTheirProvenance(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, e := range m.Edges() {
		want := ""
		if e.Event() == "cancel" {
			want = "live"
		}
		if e.Group != want {
			t.Errorf("edge %v --%s--> %v has Group %q, want %q", e.From, e.Event(), e.To, e.Group, want)
		}
	}
	if groups := m.Groups(); len(groups) != 1 || groups[0].Name() != "live" {
		t.Errorf("Groups() = %v, want one group named live", groups)
	}
}

func TestGroupHasReportsMembership(t *testing.T) {
	t.Parallel()

	if !live.Has(idle) || !live.Has(running) {
		t.Error("live should contain idle and running")
	}
	if live.Has(done) {
		t.Error("live should not contain done")
	}
}

// A group is not a state: it expands away at build time and never appears
// anywhere a state value does.
func TestGroupIsNotAState(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := len(m.States()); got != 3 {
		t.Errorf("States() reports %d states, want 3 (idle, running, cancelled)", got)
	}
	if got := m.Terminals(); len(got) != 1 || got[0] != cancelled {
		t.Errorf("terminals %v, want [cancelled]", got)
	}
}

func TestGroupRejectsUnknownMember(t *testing.T) {
	t.Parallel()

	typo := fsm.NewGroup("live", idle, done)
	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(typo).On(evCancel).To(cancelled),
	)
	if err == nil {
		t.Fatal("expected an error for a member that is not a state of the machine")
	}
	if !strings.Contains(err.Error(), "member done is not a state") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGroupRejectsTwoGroupsClaimingTheSameEvent(t *testing.T) {
	t.Parallel()

	other := fsm.NewGroup("other", running)
	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
		fsm.FromGroup(other).On(evCancel).To(done),
	)
	if err == nil {
		t.Fatal("expected an error for overlapping groups claiming one event")
	}
	if !strings.Contains(err.Error(), "both give running a transition on cancel") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGroupRejectsNameReusedForDifferentMembers(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
		fsm.FromGroup(fsm.NewGroup("live", idle)).On(evFinish).To(done),
	)
	if err == nil {
		t.Fatal("expected an error for a group name reused with different members")
	}
	if !strings.Contains(err.Error(), "declared twice with different members") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGroupWithNoMembersIsAnError(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(fsm.NewGroup[state]("empty")).On(evCancel).To(cancelled),
	)
	if err == nil {
		t.Fatal("expected an error for a group with no members")
	}
	if !strings.Contains(err.Error(), "has no members") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The name is the group's identity in error messages and in DOT's cluster
// label, so an unnamed group is rejected before it can expand.
func TestGroupWithNoNameIsAnError(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(fsm.NewGroup("", idle, running)).On(evCancel).To(cancelled),
	)
	if err == nil {
		t.Fatal("expected an error for a group with no name")
	}
	if !strings.Contains(err.Error(), "group declared with no name") {
		t.Errorf("unexpected error: %v", err)
	}
	if got := strings.Count(err.Error(), "\n") + 1; got != 1 {
		t.Errorf("reported %d errors, want 1:\n%v", got, err)
	}
}

// A group transition every member overrides is dead configuration, and saying
// so is cheaper than leaving someone to notice the group does nothing.
func TestGroupTransitionFullyOverriddenIsAnError(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(idle).On(evCancel).To(done),
		fsm.From(running).On(evCancel).To(done),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
	)
	if err == nil {
		t.Fatal("expected an error for a group transition every member overrides")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGroupGuardRejectsEveryInheritedEdge(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled).
			Guard("never", func(context.Context, fsm.Unit) error { return errNonZeroExit }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, from := range live.Members() {
		err := m.Check(t.Context(), from, evCancel, fsm.Unit{})
		ge, ok := errors.AsType[*fsm.GuardError[state]](err)
		if !ok {
			t.Fatalf("check from %v: got %T, want *fsm.GuardError", from, err)
		}
		if ge.Guard != "never" {
			t.Errorf("guard from %v named %q, want \"never\"", from, ge.Guard)
		}
	}
}

// Group expansion has to run before GaugeWith reads the table, or a gauge
// silently misses every edge a group contributed.
func TestGaugeWithSeesGroupInheritedEdges(t *testing.T) {
	t.Parallel()

	var delta int
	m, err := fsm.New("job",
		fsm.GaugeWith(cancelled,
			func(_ context.Context, _ int) { delta++ },
			func(_ context.Context, _ int) { delta-- },
		),
		fsm.From(idle).On(evFinish).To(running),
		fsm.FromGroup(fsm.NewGroup("live", idle, running)).On(evFinish).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	st := running
	if _, err := m.Fire(t.Context(), &st, evFinish, 1); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if delta != 1 {
		t.Errorf("gauge moved by %d on a group-inherited edge, want 1", delta)
	}
}

// Several sources entering on one event share the (state, event) hook key; the
// gauge must still move once per entry.
func TestGaugeWithCountsFanInOnce(t *testing.T) {
	t.Parallel()

	var delta int
	m, err := fsm.New("job",
		fsm.GaugeWith(cancelled,
			func(_ context.Context, _ int) { delta++ },
			func(_ context.Context, _ int) { delta-- },
		),
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(fsm.NewGroup("live", idle, running)).On(evFinish).To(cancelled),
		fsm.FromEach(done, cancelled).On(evFinish).To(idle),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, from := range []state{idle, running} {
		st := from
		if _, err := m.Fire(t.Context(), &st, evFinish, 1); err != nil {
			t.Fatalf("cancel from %v: %v", from, err)
		}
		if delta != 1 {
			t.Errorf("gauge moved by %d entering from %v, want 1", delta, from)
		}
		if _, err := m.Fire(t.Context(), &st, evFinish, 1); err != nil {
			t.Fatalf("leave cancelled: %v", err)
		}
		if delta != 0 {
			t.Errorf("gauge at %d after leaving, want 0", delta)
		}
	}
}

// Groups are a build-time expansion, so the fire path must be unchanged.
func TestGroupFireDoesNotAllocate(t *testing.T) {
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(cancelled).On(evStart).To(idle),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := t.Context()
	st := running

	avg := testing.AllocsPerRun(1000, func() {
		_, _ = m.Send(ctx, &st, evCancel)
		_, _ = m.Send(ctx, &st, evStart)
	})
	if avg != 0 {
		t.Errorf("Fire allocates %.1f times per round trip on a grouped machine, want 0", avg)
	}
}

func TestGroupDOTDrawsAClusterAndOneBoundaryEdge(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	got := m.DOT()
	for _, want := range []string{
		"compound=true;",
		`subgraph "cluster_live" {`,
		`label="live";`,
		`ltail="cluster_live"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DOT output missing %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, `[label="cancel"`); n != 1 {
		t.Errorf("cancel drawn %d times, want once from the cluster boundary:\n%s", n, got)
	}
}

// With a member overriding the event, a single boundary arrow would claim to
// cover it, so each inherited edge is drawn on its own.
func TestGroupDOTKeepsPerMemberEdgesWhenOverridden(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromEach(idle, running).On(evFinish).To(done),
		fsm.FromGroup(fsm.NewGroup("live", idle, running, done)).On(evCancel).To(cancelled),
		fsm.From(done).On(evCancel).To(idle),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	got := m.DOT()
	if strings.Contains(got, "ltail=") {
		t.Errorf("drew a boundary edge despite an override:\n%s", got)
	}
	if n := strings.Count(got, `[label="cancel"]`); n != 3 {
		t.Errorf("cancel drawn %d times, want 3 (two inherited, one override):\n%s", n, got)
	}
}

// A group transition's target is a state even before expansion runs, so a
// group whose member is only ever named as another group's target is fine.
func TestGroupTargetCountsAsADeclaredState(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
		fsm.FromGroup(fsm.NewGroup("finished", cancelled)).On(evStart).To(idle),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
}

func TestGroupRejectsTheSameTransitionDeclaredTwice(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
		fsm.FromGroup(live).On(evCancel).To(done),
	)
	if err == nil {
		t.Fatal("expected an error for a group transition declared twice")
	}
	if !strings.Contains(err.Error(), "duplicate transition from group live on cancel") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Rules are values and a set of them can be shared between machines, so
// applying a group rule must not leave anything behind in the rule itself.
func TestGroupRuleCanBeSharedBetweenMachines(t *testing.T) {
	t.Parallel()

	shared := []fsm.Rule[state]{
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(live).On(evCancel).To(cancelled),
	}
	first, err := fsm.New("first", shared...)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := fsm.New("second", shared...)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !slices.Equal(first.Edges(), second.Edges()) {
		t.Errorf("machines built from one rule set differ:\n%v\n%v", first.Edges(), second.Edges())
	}
}

// A Via hook on an edge the broken group would have added is not a second mistake.
func TestBrokenGroupDoesNotAlsoReportTheViaHook(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(fsm.NewGroup[state]("empty")).On(evCancel).To(cancelled),
		fsm.OnEnterVia(cancelled, evCancel, func(context.Context, fsm.Transition[state], fsm.Unit) {}),
	)
	if err == nil {
		t.Fatal("expected an error for a group with no members")
	}
	if got := strings.Count(err.Error(), "\n") + 1; got != 1 {
		t.Errorf("reported %d errors, want 1:\n%v", got, err)
	}
}

// A broken group must produce one error, not a cascade: with no members there
// is nothing to report as overridden.
func TestGroupWithNoMembersReportsOneError(t *testing.T) {
	t.Parallel()

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(fsm.NewGroup[state]("empty")).On(evCancel).To(cancelled),
	)
	if err == nil {
		t.Fatal("expected an error for a group with no members")
	}
	if got := strings.Count(err.Error(), "\n") + 1; got != 1 {
		t.Errorf("reported %d errors, want 1:\n%v", got, err)
	}
	if !strings.Contains(err.Error(), "has no members") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A Group is a Rule, so one used only for Has or for DOT output can be passed
// to New without a transition attached to it.
func TestBareGroupRuleIsRegistered(t *testing.T) {
	t.Parallel()

	m, err := fsm.New("job",
		live,
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if groups := m.Groups(); len(groups) != 1 || groups[0].Name() != "live" {
		t.Fatalf("Groups() = %v, want one group named live", groups)
	}
	if got := m.DOT(); !strings.Contains(got, `subgraph "cluster_live"`) {
		t.Errorf("DOT output has no cluster for a group with no transition:\n%s", got)
	}
	// No edge came from the group, so nothing is drawn from its boundary.
	if strings.Contains(m.DOT(), "ltail=") {
		t.Error("drew a boundary edge for a group with no transition")
	}
}

// --- Group DOT and validation regressions ---------------------------------

// Two events can share a name, so the boundary collapse is keyed on the
// trigger itself. Counting by name merged two partial expansions into one
// arrow that covered neither.
func TestGroupDOTDistinguishesSameNamedEvents(t *testing.T) {
	t.Parallel()

	goA, goB := fsm.Signal("go"), fsm.Signal("go")
	g := fsm.NewGroup("g", idle, running, done)

	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
		fsm.FromGroup(g).On(goA).To(cancelled),
		fsm.FromGroup(g).On(goB).To(cancelled),
		fsm.From(idle).On(goA).To(done),    // overrides goA
		fsm.From(running).On(goB).To(idle), // overrides goB
		fsm.From(done).On(goB).To(idle),    // overrides goB
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Every member overrides one of the two events, so neither group
	// transition covers the whole group and nothing may be collapsed.
	if got := m.DOT(); strings.Contains(got, "ltail=") {
		t.Errorf("collapsed a partial expansion of same-named events:\n%s", got)
	}
	// goA is inherited by two members and goB by one, plus the three
	// overrides: six rows, none of them merged with another.
	if got := strings.Count(m.DOT(), `[label="go"`); got != 6 {
		t.Errorf("drew %d \"go\" arrows, want 6\n%s", got, m.DOT())
	}
}

func TestEdgeIsIdentifiesTheTrigger(t *testing.T) {
	t.Parallel()

	sameName := fsm.Signal("start")
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(sameName).To(done),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, e := range m.Edges() {
		if e.Event() != "start" {
			t.Fatalf("unexpected event name %q", e.Event())
		}
		switch e.From {
		case idle:
			if !e.Is(evStart) || e.Is(sameName) {
				t.Error("edge from idle did not identify evStart")
			}
		case running:
			if !e.Is(sameName) || e.Is(evStart) {
				t.Error("edge from running did not identify the same-named event")
			}
		}
	}
}

// A state can only sit in one Graphviz cluster, so an overlapping group that
// lost a member cannot be an edge tail: Graphviz would drop the ltail and the
// other members' rows were already skipped.
func TestGroupDOTKeepsPerMemberEdgesForOverlappingGroups(t *testing.T) {
	t.Parallel()

	first := fsm.NewGroup("first", idle, running)
	second := fsm.NewGroup("second", running, done)

	// first is declared first, so its cluster takes running and second is
	// left holding only part of its membership.
	m, err := fsm.New("job",
		first,
		fsm.From(idle).On(evStart).To(running),
		fsm.From(running).On(evFinish).To(done),
		fsm.FromGroup(second).On(evCancel).To(cancelled),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	got := m.DOT()
	if strings.Contains(got, "ltail=") {
		t.Errorf("collapsed an overlapping group that does not hold all its members:\n%s", got)
	}
	// Both inherited rows must still appear.
	for _, want := range []string{`"running" -> "cancelled"`, `"done" -> "cancelled"`} {
		if !strings.Contains(got, want) {
			t.Errorf("DOT output lost %s:\n%s", want, got)
		}
	}
	// And a state in two groups is drawn once.
	if n := strings.Count(got, `"running" [shape=`); n != 1 {
		t.Errorf("running declared %d times, want 1:\n%s", n, got)
	}
}

// A target inside the group would make the boundary arrow a self-loop out of
// its own cluster, which Graphviz refuses.
func TestGroupDOTKeepsPerMemberEdgesWhenTargetIsAMember(t *testing.T) {
	t.Parallel()

	g := fsm.NewGroup("g", idle, running)
	m, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(g).On(evCancel).To(idle),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	got := m.DOT()
	if strings.Contains(got, "ltail=") {
		t.Errorf("collapsed a transition whose target is a member:\n%s", got)
	}
	for _, want := range []string{`"idle" -> "idle"`, `"running" -> "idle"`} {
		if !strings.Contains(got, want) {
			t.Errorf("DOT output lost %s:\n%s", want, got)
		}
	}
}

// A member lost to another group is already reported; claiming it overrode
// the event itself would not be true.
func TestGroupClashDoesNotAlsoReportUnreachable(t *testing.T) {
	t.Parallel()

	first := fsm.NewGroup("first", running)
	second := fsm.NewGroup("second", running)

	_, err := fsm.New("job",
		fsm.From(idle).On(evStart).To(running),
		fsm.FromGroup(first).On(evCancel).To(cancelled),
		fsm.FromGroup(second).On(evCancel).To(done),
	)
	if err == nil {
		t.Fatal("expected an error for two groups claiming one event")
	}
	if !strings.Contains(err.Error(), "both give running a transition on cancel") {
		t.Errorf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "unreachable") {
		t.Errorf("clash also reported as unreachable:\n%v", err)
	}
}

func TestGroupRejectsARepeatedMember(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		rules []fsm.Rule[state]
	}{
		{"with a transition", []fsm.Rule[state]{
			fsm.From(idle).On(evStart).To(running),
			fsm.FromGroup(fsm.NewGroup("g", idle, idle)).On(evCancel).To(cancelled),
		}},
		{"bare", []fsm.Rule[state]{
			fsm.From(idle).On(evStart).To(running),
			fsm.NewGroup("g", idle, idle),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := fsm.New("job", tc.rules...)
			if err == nil {
				t.Fatal("expected an error for a group listing a member twice")
			}
			if !strings.Contains(err.Error(), "lists idle twice") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// --- Fuzz ------------------------------------------------------------------

// Whatever table it is handed, a machine New accepted must keep the contract
// the introspection API advertises, and Fire must agree with To rather than
// panic or move a state it said it could not move. The named tests above pin
// the cases the design was reasoned about; this covers the ones nobody wrote
// down, group expansion and the deferred Initial check in particular.
func FuzzMachineInvariants(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 1, 0, 2, 1, 1, 3})       // a linear machine
	f.Add([]byte{1, 4, 1, 3, 0, 0, 1})                // a group transition plus an Initial
	f.Add([]byte{0, 4, 0, 2, 5, 1, 3, 1, 2, 0})       // both groups, overlapping on running
	f.Add([]byte{0, 4, 0, 2, 0, 0, 1})                // a member overriding its group
	f.Add([]byte{0, 0, 0, 1, 0, 0, 2, 4, 2, 0, 5, 2}) // two edges out of idle both named "a"
	f.Add([]byte{})

	states := []state{idle, running, done, cancelled}
	// Two of these share a name: the trigger is an identity, never a string.
	events := []fsm.Event[int]{fsm.Define[int]("a"), fsm.Define[int]("b"), fsm.Define[int]("a")}
	groups := []fsm.Group[state]{
		fsm.NewGroup("g0", idle, running),
		fsm.NewGroup("g1", running, done), // overlaps g0 on running
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) == 0 {
			return
		}
		head, rows := in[0], in[1:]

		var rules []fsm.Rule[state]
		if head&1 == 1 {
			rules = append(rules, fsm.Initial(states[int(head>>1)%len(states)]))
		}
		// Drop the three mistakes a random table makes systematically: a
		// source claiming an event twice, two groups claiming one event, and a
		// group transition every member overrides. Without this, any input
		// long enough to be interesting is rejected by New and none of the
		// invariants below ever runs — at 200 bytes the acceptance rate goes
		// from 0% to ~87%. The named tests above cover all three errors.
		type row struct{ src, ev, to int } // src indexes states first, then groups
		var picked []row
		seen := map[[2]int]bool{}
		claimedBy := map[int]int{} // event -> the group source holding it
		for i := 0; i+2 < len(rows); i += 3 {
			r := row{int(rows[i]) % (len(states) + len(groups)), int(rows[i+1]) % len(events), int(rows[i+2]) % len(states)}
			if seen[[2]int{r.src, r.ev}] {
				continue
			}
			if r.src >= len(states) {
				if prev, ok := claimedBy[r.ev]; ok && prev != r.src {
					continue
				}
				claimedBy[r.ev] = r.src
			}
			seen[[2]int{r.src, r.ev}] = true
			picked = append(picked, r)
		}
		picked = slices.DeleteFunc(picked, func(r row) bool {
			return r.src >= len(states) &&
				!slices.ContainsFunc(groups[r.src-len(states)].Members(), func(m state) bool {
					return !seen[[2]int{slices.Index(states, m), r.ev}]
				})
		})
		for _, r := range picked {
			ev, to := events[r.ev], states[r.to]
			if r.src < len(states) {
				rules = append(rules, fsm.From(states[r.src]).On(ev).To(to))
			} else {
				rules = append(rules, fsm.FromGroup(groups[r.src-len(states)]).On(ev).To(to))
			}
		}

		m, err := fsm.New("fuzz", rules...)
		if err != nil {
			return // a rejected definition has no invariants to keep
		}

		if dot := m.DOT(); dot != m.DOT() {
			t.Fatalf("DOT output varies between calls:\n%s", dot)
		}

		known, edges := m.States(), m.Edges()
		outgoing := map[state]bool{}
		for _, e := range edges {
			if !slices.Contains(known, e.From) || !slices.Contains(known, e.To) {
				t.Fatalf("edge %v --%s--> %v has an endpoint outside States() %v", e.From, e.Event(), e.To, known)
			}
			// An inherited edge names the group that owns its source.
			if e.Group != "" {
				i := slices.IndexFunc(m.Groups(), func(g fsm.Group[state]) bool { return g.Name() == e.Group })
				if i < 0 || !m.Groups()[i].Has(e.From) {
					t.Fatalf("edge %v --%s--> %v claims group %q, which does not hold %v", e.From, e.Event(), e.To, e.Group, e.From)
				}
			}
			outgoing[e.From] = true
		}

		// Events deduplicates by name; States and Edges do not repeat a row.
		names := m.Events()
		if slices.Contains(names, "") || len(slices.Compact(slices.Sorted(slices.Values(names)))) != len(names) {
			t.Fatalf("Events() %v repeats a name or reports an unnamed trigger", names)
		}

		for _, s := range known {
			if got := slices.Contains(m.Terminals(), s); got == outgoing[s] {
				t.Fatalf("state %v: terminal=%v, has outgoing edges=%v", s, got, outgoing[s])
			}
			// Reachability is a fixpoint: nothing a reached state points at
			// may be reported unreachable.
			stuck := m.Unreachable(s)
			for _, e := range edges {
				if !slices.Contains(stuck, e.From) && slices.Contains(stuck, e.To) {
					t.Fatalf("Unreachable(%v) = %v, but %v reaches %v", s, stuck, e.From, e.To)
				}
			}
		}

		// New rejects a start nothing reaches from, or one with no way out.
		if init, ok := m.Initial(); ok {
			if stuck := m.Unreachable(init); len(stuck) != 0 {
				t.Fatalf("New accepted initial %v leaving %v unreachable", init, stuck)
			}
			if slices.Contains(m.Terminals(), init) {
				t.Fatalf("New accepted initial %v with no way out", init)
			}
		}

		// To is the table; Fire must agree with it, including from a state
		// the definition never mentioned.
		ctx := t.Context()
		for _, from := range states {
			for _, ev := range events {
				want, ok := m.To(from, ev)
				st := from
				tr, err := m.Fire(ctx, &st, ev, 0)
				switch {
				case ok && err != nil:
					t.Fatalf("Fire %s from %v: %v, but To resolves %v", ev, from, err, want)
				case ok && (st != want || tr.From != from || tr.To != want || !tr.Is(ev)):
					t.Fatalf("Fire %s from %v left %v (%v -> %v), want %v", ev, from, st, tr.From, tr.To, want)
				case !ok && err == nil:
					t.Fatalf("Fire %s from %v succeeded with no transition declared", ev, from)
				case !ok && st != from:
					t.Fatalf("Fire %s from %v failed but moved the state to %v", ev, from, st)
				}
			}
		}
	})
}

// --- Concurrency -----------------------------------------------------------

// The headline claim: a Machine is immutable after New, so one value serves
// every goroutine that owns a state and needs no lock. Nothing else in the
// suite fires a shared machine concurrently, so -race never saw this path.
// Hooks are shared, so keeping them safe is the caller's job — hence the
// atomic counter.
func TestAMachineIsSafeToShareAcrossGoroutines(t *testing.T) {
	t.Parallel()

	var inRunning atomic.Int64
	m, err := fsm.New("job",
		fsm.Gauge(running, func(context.Context) { inRunning.Add(1) }, func(context.Context) { inRunning.Add(-1) }),
		fsm.From(idle).On(evStart).To(running).Guard("always", func(context.Context, fsm.Unit) error { return nil }),
		fsm.From(running).On(evFinish).To(done).Action(func(context.Context, int) error { return nil }),
		fsm.From(done).On(evCancel).To(idle),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := t.Context()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			st := idle // each goroutine owns its own state
			for range 200 {
				// A round trip is idle -> running -> done -> idle, so the
				// gauge nets to zero only if every leg ran.
				_, start := m.Send(ctx, &st, evStart)
				// Introspection reads the same tables Fire does.
				_, _, _ = m.DOT(), m.Terminals(), m.Edges()
				_, finish := m.Fire(ctx, &st, evFinish, 0)
				_, cancel := m.Send(ctx, &st, evCancel)
				if err := errors.Join(start, finish, cancel); err != nil {
					t.Errorf("round trip: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()

	// Only meaningful if every round trip completed; otherwise the drift is
	// the abandoned cycle, not a lost decrement.
	if got := inRunning.Load(); got != 0 && !t.Failed() {
		t.Errorf("gauge drifted to %d across goroutines, want 0", got)
	}
}
