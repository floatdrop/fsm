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

// Events are prefixed ev so they never read as states. Without it the pair
// "stop" and "stopped" is one tense apart, which is not a difference worth
// relying on.
//
// The payload is the aggregate itself, so guards and actions read the same
// data the caller has, with no package-level state and no type assertions.
var (
	evRecStop   = fsm.Define[*recording]("stop")
	evRecFinish = fsm.Define[*recording]("finish")
	evRecUpload = fsm.Signal("uploaded")
)

// A guard rejects with a sentinel, so a caller can tell "not finishable yet,
// retry later" apart from "not finishable at all" without parsing strings.
var errUploadPending = errors.New("still uploading")

func markStopped(_ context.Context, r *recording) error {
	r.stoppedAt = time.Unix(1700000000, 0).UTC()
	return nil
}

func uploadsSettled(_ context.Context, r *recording) error {
	if r.inProgressChunks != 0 || r.inProgressTracks != 0 {
		return fmt.Errorf("%w: %d chunks, %d tracks",
			errUploadPending, r.inProgressChunks, r.inProgressTracks)
	}
	return nil
}

// Gauges tracking how many recordings sit in each state. In hand-written form
// these are a += and a -= at every site that changes the state, and they drift
// as soon as one site is missed.
var gauge = map[recState]int{}

func incr(s recState) func(context.Context) { return func(context.Context) { gauge[s]++ } }
func decr(s recState) func(context.Context) { return func(context.Context) { gauge[s]-- } }

// A machine is a set of rules. Each transition names its source and target in
// separate calls, so the two states cannot be swapped the way two adjacent
// arguments can.
var recordingFSM = fsm.MustNew("recording",
	fsm.Initial(recActive),
	fsm.Gauge(recActive, incr(recActive), decr(recActive)),
	fsm.Gauge(recStopped, incr(recStopped), decr(recStopped)),
	fsm.Gauge(recFinished, incr(recFinished), decr(recFinished)),
	fsm.Gauge(recUploaded, incr(recUploaded), decr(recUploaded)),

	fsm.From(recActive).On(evRecStop).To(recStopped).Action(markStopped),
	fsm.From(recStopped).On(evRecFinish).To(recFinished).
		Guard("all chunks and tracks uploaded", uploadsSettled),
	fsm.From(recFinished).On(evRecUpload).To(recUploaded),
)

// A recording moves active -> stopped -> finished -> uploaded. The order is
// declared once; every caller goes through Fire, so no call site can skip a
// step or forget a gauge.
func Example_recording() {
	ctx := context.Background()
	r := &recording{inProgressChunks: 2}
	gauge[recActive]++ // the initial state is entered by construction

	fmt.Println("state:", r.state)

	if _, err := recordingFSM.Fire(ctx, &r.state, evRecStop, r); err != nil {
		fmt.Println("unexpected:", err)
	}
	fmt.Println("after stop:", r.state, "at", r.stoppedAt.Format(time.RFC3339))

	// Finishing early is refused by the guard, and the state does not move.
	_, err := recordingFSM.Fire(ctx, &r.state, evRecFinish, r)
	if ge, ok := errors.AsType[*fsm.GuardError[recState]](err); ok {
		fmt.Printf("finish refused by %q: %v\n", ge.Guard, ge.Err)
		fmt.Println("is upload pending?", errors.Is(err, errUploadPending), "| state still", r.state)
	}

	r.inProgressChunks = 0
	if _, err := recordingFSM.Fire(ctx, &r.state, evRecFinish, r); err != nil {
		fmt.Println("unexpected:", err)
	}
	if _, err := recordingFSM.Send(ctx, &r.state, evRecUpload); err != nil {
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
	evPcpDrop      = fsm.Define[disconnect]("disconnect")
	evPcpReconnect = fsm.Signal("reconnect")
	evPcpKick      = fsm.Define[disconnect]("kick")
)

func unintentional(_ context.Context, d disconnect) error {
	if d.intentional {
		return fmt.Errorf("intentional disconnect (%s) must be a kick, not a drop", d.reason)
	}
	return nil
}

// A participant is kicked the same way whether it is connected or
// reconnecting. Naming the two as a group says that once: a third live state
// inherits the kick instead of needing the rule copied, and the group is still
// the place to look for what "live" means.
var pcpLive = fsm.NewGroup("live", pcpConnected, pcpReconnecting)

// Modelling a participant makes deletion an explicit terminal state rather
// than "absent from the map", so the last transition is expressible and can
// carry a reason.
var participantFSM = fsm.MustNew("participant",
	fsm.Initial(pcpConnected),
	fsm.From(pcpConnected).On(evPcpDrop).To(pcpReconnecting).
		Guard("disconnect was not intentional", unintentional),
	fsm.From(pcpReconnecting).On(evPcpReconnect).To(pcpConnected),
	fsm.FromGroup(pcpLive).On(evPcpKick).To(pcpDeleted),
)

func Example_participant() {
	ctx := context.Background()
	st := pcpConnected

	_, _ = participantFSM.Fire(ctx, &st, evPcpDrop, disconnect{reason: "ice failed"})
	fmt.Println("after drop:", st)

	_, _ = participantFSM.Send(ctx, &st, evPcpReconnect)
	fmt.Println("after reconnect:", st)

	_, _ = participantFSM.Fire(ctx, &st, evPcpKick, disconnect{intentional: true, reason: "left the call"})
	fmt.Println("after kick:", st)

	// Nothing leaves a terminal state, so a kicked participant cannot be
	// resurrected by a late event arriving out of order.
	_, err := participantFSM.Send(ctx, &st, evPcpReconnect)
	fmt.Println("late reconnect:", err)
	fmt.Println("terminals:", participantFSM.Terminals())

	// A group is a build-time grouping, so the kick is two ordinary rows in
	// the table — but each remembers where it came from.
	for _, e := range participantFSM.Edges() {
		if e.Group != "" {
			fmt.Printf("%v --%s--> %v inherited from %q\n", e.From, e.Event(), e.To, e.Group)
		}
	}
	fmt.Println("is reconnecting live?", pcpLive.Has(pcpReconnecting))

	// Output:
	// after drop: reconnecting
	// after reconnect: connected
	// after kick: deleted
	// late reconnect: fsm participant: no transition from deleted on reconnect
	// terminals: [deleted]
	// connected --kick--> deleted inherited from "live"
	// reconnecting --kick--> deleted inherited from "live"
	// is reconnecting live? true
}

// A group is drawn as a cluster, and a transition every member inherited is
// drawn once from the cluster boundary rather than once per member — which is
// the whole visual point of grouping the states in the first place.
func ExampleMachine_DOT_group() {
	fmt.Print(participantFSM.DOT())

	// Output:
	// digraph "participant" {
	// 	rankdir=LR;
	// 	compound=true;
	// 	"__start" [shape=point];
	// 	subgraph "cluster_live" {
	// 		label="live";
	// 		style=rounded;
	// 		"connected" [shape=box];
	// 		"reconnecting" [shape=box];
	// 	}
	// 	"deleted" [shape=doublecircle];
	// 	"__start" -> "connected";
	// 	"connected" -> "reconnecting" [label="disconnect\n[disconnect was not intentional]"];
	// 	"reconnecting" -> "connected" [label="reconnect"];
	// 	"connected" -> "deleted" [label="kick", ltail="cluster_live"];
	// }
}

// DOT output is stable across runs, so it can be committed next to the code
// and reviewed as part of a diff when the machine changes.
func ExampleMachine_DOT() {
	fmt.Print(recordingFSM.DOT())

	// Output:
	// digraph "recording" {
	// 	rankdir=LR;
	// 	"__start" [shape=point];
	// 	"active" [shape=box];
	// 	"stopped" [shape=box];
	// 	"finished" [shape=box];
	// 	"uploaded" [shape=doublecircle];
	// 	"__start" -> "active";
	// 	"active" -> "stopped" [label="stop"];
	// 	"stopped" -> "finished" [label="finish\n[all chunks and tracks uploaded]"];
	// 	"finished" -> "uploaded" [label="uploaded"];
	// }
}
