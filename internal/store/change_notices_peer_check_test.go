package store

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// TestVolatilePeerMarks: the volatile backend's peer check read moves a
// workspace's log position on an enqueue and its task-event count and
// highest ID on a task event, and nothing else (component-persistence,
// Change notices).
func TestVolatilePeerMarks(t *testing.T) {
	st := NewVolatileBackend()
	reader, ok := st.(changes.PeerReader)
	if !ok {
		t.Fatal("the volatile backend offers no PeerMarks")
	}
	base := context.Background()
	marks, err := reader.PeerMarks(base)
	if err != nil || len(marks) != 0 {
		t.Fatalf("empty store marks=%v err=%v", marks, err)
	}
	tasks := map[string]string{}
	for _, ws := range []string{"a", "b"} {
		ctx := WithActor(WithWorkspace(base, ws), SystemActor())
		id := core.NewTaskID()
		tasks[ws] = id
		if err = st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := reader.PeerMarks(base)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reader.PeerMarks(base)
	if err != nil || again["a"] != before["a"] || again["b"] != before["b"] {
		t.Fatalf("a read without a change moved the marks: %v then %v (err %v)", before, again, err)
	}

	// An enqueue in a moves only a's log position.
	if _, err = logqueue.Enqueue(WithActor(WithWorkspace(base, "a"), SystemActor()), st.Log(), "a", "peer_probe", "one", struct{}{}, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	afterJob, err := reader.PeerMarks(base)
	if err != nil {
		t.Fatal(err)
	}
	if afterJob["a"].Log <= before["a"].Log || afterJob["a"].Events != before["a"].Events || afterJob["a"].LastEvent != before["a"].LastEvent {
		t.Fatalf("enqueue in a: %+v -> %+v", before["a"], afterJob["a"])
	}
	if afterJob["b"] != before["b"] {
		t.Fatalf("enqueue in a moved b: %+v -> %+v", before["b"], afterJob["b"])
	}

	// A task event in b moves only b's count and highest ID.
	if err = st.AppendEvent(WithActor(WithWorkspace(base, "b"), SystemActor()), core.Event{TaskID: tasks["b"], Kind: "peer.probe"}); err != nil {
		t.Fatal(err)
	}
	afterEvent, err := reader.PeerMarks(base)
	if err != nil {
		t.Fatal(err)
	}
	if afterEvent["b"].Events != afterJob["b"].Events+1 || afterEvent["b"].LastEvent <= afterJob["b"].LastEvent || afterEvent["b"].Log != afterJob["b"].Log {
		t.Fatalf("event in b: %+v -> %+v", afterJob["b"], afterEvent["b"])
	}
	if afterEvent["a"] != afterJob["a"] {
		t.Fatalf("event in b moved a: %+v -> %+v", afterJob["a"], afterEvent["a"])
	}

	cancelled, cancel := context.WithCancel(base)
	cancel()
	if marks, err = reader.PeerMarks(cancelled); err == nil || marks != nil {
		t.Fatalf("cancelled read marks=%v err=%v", marks, err)
	}
}
