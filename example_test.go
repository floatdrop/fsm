package fsm_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/floatdrop/fsm"
)

// recState is int32-shaped so it can be stored directly in a protobuf enum
// field and survive a snapshot/restore round trip.
type recState int32

const (
	recActive recState = iota
	recStopped
	recFinished
	recUploaded
)

func (s recState) String() string {
	return [...]string{"active", "stopped", "finished", "uploaded"}[s]
}

type recording struct {
	state            recState
	stoppedAt        time.Time
	inProgressChunks int
	inProgressTracks int
}

// The payload is the aggregate itself, so guards and actions read the same
// data the caller has, with no package-level state and no type assertions.
var (
	recStop   = fsm.Define[*recording]("stop")
	recFinish = fsm.Define[*recording]("finish")
	recUpload = fsm.Signal("uploaded")
)

// A guard rejects with a sentinel, so a caller can tell "not finishable yet,
// retry later" apart from "not finishable at all" without parsing strings.
var errUploadPending = errors.New("still uploading")

// Gauges tracking how many recordings sit in each state. In hand-written form
// these are a += and a -= at every site that changes the state, and they drift
// as soon as one site is missed.
var gauge = map[recState]int{}

var recordingFSM = fsm.New[recState]("recording").
	Gauge(recActive, incr(recActive), decr(recActive)).
	Gauge(recStopped, incr(recStopped), decr(recStopped)).
	Gauge(recFinished, incr(recFinished), decr(recFinished)).
	Gauge(recUploaded, incr(recUploaded), decr(recUploaded)).
	On(recStop, recActive, recStopped, fsm.WithAction(func(_ context.Context, r *recording) error {
		r.stoppedAt = time.Unix(1700000000, 0).UTC()
		return nil
	})).
	On(recFinish, recStopped, recFinished,
		fsm.WithGuard("all chunks and tracks uploaded", func(_ context.Context, r *recording) error {
			if r.inProgressChunks != 0 || r.inProgressTracks != 0 {
				return fmt.Errorf("%w: %d chunks, %d tracks",
					errUploadPending, r.inProgressChunks, r.inProgressTracks)
			}
			return nil
		}),
	).
	On(recUpload, recFinished, recUploaded).
	MustBuild()

func incr(s recState) func(context.Context) { return func(context.Context) { gauge[s]++ } }
func decr(s recState) func(context.Context) { return func(context.Context) { gauge[s]-- } }

// A recording moves active -> stopped -> finished -> uploaded. The order is
// declared once; every caller goes through Fire, so no call site can skip a
// step or forget a gauge.
func Example_recording() {
	ctx := context.Background()
	r := &recording{inProgressChunks: 2}
	gauge[recActive]++ // the initial state is entered by construction

	fmt.Println("state:", r.state)

	if err := recordingFSM.Fire(ctx, &r.state, recStop, r); err != nil {
		fmt.Println("unexpected:", err)
	}
	fmt.Println("after stop:", r.state, "at", r.stoppedAt.Format(time.RFC3339))

	// Finishing early is refused by the guard, and the state does not move.
	err := recordingFSM.Fire(ctx, &r.state, recFinish, r)
	var ge *fsm.GuardError[recState]
	if errors.As(err, &ge) {
		fmt.Printf("finish refused by %q: %v\n", ge.Guard, ge.Err)
		fmt.Println("is upload pending?", errors.Is(err, errUploadPending), "| state still", r.state)
	}

	r.inProgressChunks = 0
	if err := recordingFSM.Fire(ctx, &r.state, recFinish, r); err != nil {
		fmt.Println("unexpected:", err)
	}
	if err := recordingFSM.Send(ctx, &r.state, recUpload); err != nil {
		fmt.Println("unexpected:", err)
	}
	fmt.Println("final:", r.state)

	fmt.Printf("gauges: active=%d stopped=%d finished=%d uploaded=%d\n",
		gauge[recActive], gauge[recStopped], gauge[recFinished], gauge[recUploaded])

	// Output:
	// state: active
	// after stop: stopped at 2023-11-14T22:13:20Z
	// finish refused by "all chunks and tracks uploaded": still uploading: 2 chunks, 0 tracks
	// is upload pending? true | state still stopped
	// final: uploaded
	// gauges: active=0 stopped=0 finished=0 uploaded=1
}

type pcpState int32

const (
	pcpConnected pcpState = iota
	pcpReconnecting
	pcpDeleted
)

func (s pcpState) String() string {
	return [...]string{"connected", "reconnecting", "deleted"}[s]
}

// A disconnect carries why it happened, so the reason cannot be lost between
// the caller and the action that logs it.
type disconnect struct {
	intentional bool
	reason      string
}

var (
	pcpDrop      = fsm.Define[disconnect]("disconnect")
	pcpReconnect = fsm.Signal("reconnect")
	pcpKick      = fsm.Define[disconnect]("kick")
)

// Modelling a participant makes deletion an explicit terminal state rather
// than "absent from the map", so the last transition is expressible and can
// carry a reason.
var participantFSM = fsm.New[pcpState]("participant").
	On(pcpDrop, pcpConnected, pcpReconnecting,
		fsm.WithGuard("disconnect was not intentional", func(_ context.Context, d disconnect) error {
			if d.intentional {
				return fmt.Errorf("intentional disconnect (%s) must be a kick, not a drop", d.reason)
			}
			return nil
		}),
	).
	On(pcpReconnect, pcpReconnecting, pcpConnected).
	On(pcpKick, pcpConnected, pcpDeleted).
	On(pcpKick, pcpReconnecting, pcpDeleted).
	MustBuild()

func Example_participant() {
	ctx := context.Background()
	st := pcpConnected

	_ = participantFSM.Fire(ctx, &st, pcpDrop, disconnect{reason: "ice failed"})
	fmt.Println("after drop:", st)

	_ = participantFSM.Send(ctx, &st, pcpReconnect)
	fmt.Println("after reconnect:", st)

	_ = participantFSM.Fire(ctx, &st, pcpKick, disconnect{intentional: true, reason: "left the call"})
	fmt.Println("after kick:", st)

	// Nothing leaves a terminal state, so a kicked participant cannot be
	// resurrected by a late event arriving out of order.
	err := participantFSM.Send(ctx, &st, pcpReconnect)
	fmt.Println("late reconnect:", err)
	fmt.Println("terminals:", participantFSM.Terminals())

	// Output:
	// after drop: reconnecting
	// after reconnect: connected
	// after kick: deleted
	// late reconnect: fsm participant: no transition from deleted on reconnect
	// terminals: [deleted]
}

// DOT output is stable across runs, so it can be committed next to the code
// and reviewed as part of a diff when the machine changes.
func ExampleMachine_DOT() {
	fmt.Print(recordingFSM.DOT())

	// Output:
	// digraph "recording" {
	// 	rankdir=LR;
	// 	"active" [shape=box];
	// 	"stopped" [shape=box];
	// 	"finished" [shape=box];
	// 	"uploaded" [shape=doublecircle];
	// 	"active" -> "stopped" [label="stop"];
	// 	"stopped" -> "finished" [label="finish\n[all chunks and tracks uploaded]"];
	// 	"finished" -> "uploaded" [label="uploaded"];
	// }
}
