package changes

import (
	"context"
	"testing"
	"time"
)

func received(t *testing.T, s *Subscription) (Notice, bool) {
	t.Helper()
	select {
	case n := <-s.C():
		return n, true
	default:
		return Notice{}, false
	}
}

func TestBatchPublishesOnlyAfterCommit(t *testing.T) {
	hub := NewHub()
	jobs := hub.Subscribe("ws", Job)
	defer jobs.Close()
	batch := hub.Begin()
	ctx := WithBatch(context.Background(), batch)
	Raise(ctx, hub, Notice{Workspace: "ws", Kind: Job})
	Raise(ctx, hub, Notice{Workspace: "ws", Kind: Job})
	if _, ok := received(t, jobs); ok {
		t.Fatal("notice delivered before commit")
	}
	if got := batch.Notices(); len(got) != 1 {
		t.Fatalf("held notices=%v, want one after duplicate suppression", got)
	}
	batch.Commit()
	if n, ok := received(t, jobs); !ok || n.Workspace != "ws" || n.Kind != Job {
		t.Fatalf("after commit notice=%+v ok=%t", n, ok)
	}
	batch.Discard()
	batch.Commit()
	if _, ok := received(t, jobs); ok {
		t.Fatal("a committed batch published twice")
	}
}

func TestBatchDiscardOnRollback(t *testing.T) {
	hub := NewHub()
	tasks := hub.SubscribeTask("ws", "t1")
	defer tasks.Close()
	batch := hub.Begin()
	batch.Raise(Notice{Workspace: "ws", Kind: Task, TaskID: "t1"})
	batch.Discard()
	batch.Commit()
	if _, ok := received(t, tasks); ok {
		t.Fatal("rolled-back notice delivered")
	}
}

func TestSavepointBatchHandsNoticesToParent(t *testing.T) {
	hub := NewHub()
	tasks := hub.Subscribe("ws", Task)
	defer tasks.Close()
	parent := hub.Begin()
	kept := parent.Savepoint()
	kept.Raise(Notice{Workspace: "ws", Kind: Task, TaskID: "kept"})
	kept.Commit()
	dropped := parent.Savepoint()
	dropped.Raise(Notice{Workspace: "ws", Kind: Task, TaskID: "dropped"})
	dropped.Discard()
	if _, ok := received(t, tasks); ok {
		t.Fatal("savepoint release published before the outer commit")
	}
	parent.Commit()
	n, ok := received(t, tasks)
	if !ok || n.TaskID != "kept" {
		t.Fatalf("notice=%+v ok=%t, want only the released savepoint's", n, ok)
	}
}

func TestRaiseWithoutBatchPublishesAtOnce(t *testing.T) {
	hub := NewHub()
	spaces := hub.Subscribe("ws", Workspace)
	defer spaces.Close()
	Raise(context.Background(), hub, Notice{Workspace: "ws", Kind: Workspace})
	if _, ok := received(t, spaces); !ok {
		t.Fatal("unbatched notice not published")
	}
}

func TestFullBufferCoalescesAndPublishNeverBlocks(t *testing.T) {
	hub := NewHub()
	alarms := hub.Subscribe("ws", Deadline)
	defer alarms.Close()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			hub.Publish(Notice{Workspace: "ws", Kind: Deadline, At: base.Add(time.Duration(1000-i) * time.Second)})
		}
		hub.Publish(Notice{Workspace: "ws", Kind: Deadline, At: base.Add(time.Hour)})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a subscriber that never reads")
	}
	n, ok := received(t, alarms)
	if !ok || !n.At.Equal(base.Add(time.Second)) {
		t.Fatalf("coalesced notice=%+v ok=%t, want the earliest deadline", n, ok)
	}
	if _, ok := received(t, alarms); ok {
		t.Fatal("more than one pending notice")
	}
	hub.Publish(Notice{Workspace: "ws", Kind: Deadline, At: base})
	hub.Publish(Notice{Workspace: "ws", Kind: Deadline})
	if n, _ := received(t, alarms); !n.At.IsZero() {
		t.Fatalf("unknown deadline time lost: %+v", n)
	}
}

func TestRoutingAndWildcards(t *testing.T) {
	hub := NewHub()
	wsJobs := hub.Subscribe("ws", Job)
	otherJobs := hub.Subscribe("other", Job)
	taskOne := hub.SubscribeTask("ws", "t1")
	wsTasks := hub.Subscribe("ws", Task)
	for _, s := range []*Subscription{wsJobs, otherJobs, taskOne, wsTasks} {
		defer s.Close()
	}
	hub.Publish(Notice{Workspace: "ws", Kind: Job})
	if _, ok := received(t, wsJobs); !ok {
		t.Fatal("workspace job notice missed")
	}
	if _, ok := received(t, otherJobs); ok {
		t.Fatal("job notice crossed workspaces")
	}
	hub.Publish(Notice{Workspace: "ws", Kind: Task, TaskID: "t2"})
	if _, ok := received(t, taskOne); ok {
		t.Fatal("another task's notice reached a task subscriber")
	}
	if n, ok := received(t, wsTasks); !ok || n.TaskID != "t2" {
		t.Fatalf("workspace task subscriber notice=%+v ok=%t", n, ok)
	}
	hub.Publish(Notice{Kind: Job})
	hub.Publish(Notice{Kind: Task})
	if _, ok := received(t, wsJobs); !ok {
		t.Fatal("every-workspace job notice missed")
	}
	if _, ok := received(t, otherJobs); !ok {
		t.Fatal("every-workspace job notice missed in the other workspace")
	}
	if _, ok := received(t, taskOne); !ok {
		t.Fatal("every-workspace task notice missed by a task subscriber")
	}
	wsJobs.Close()
	wsJobs.Close()
	hub.Publish(Notice{Workspace: "ws", Kind: Job})
	if _, ok := received(t, wsJobs); ok {
		t.Fatal("closed subscription received a notice")
	}
	var nilHub *Hub
	nilHub.Publish(Notice{Workspace: "ws", Kind: Job})
	quiet := nilHub.Subscribe("ws", Job)
	quiet.Close()
	if _, ok := received(t, quiet); ok {
		t.Fatal("nil hub delivered")
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	for _, n := range []Notice{
		{Workspace: "demo", Kind: Job},
		{Workspace: "demo", Kind: Task, TaskID: "261101-668fd5"},
		{Workspace: "demo", Kind: Deadline},
		{Workspace: "demo", Kind: Workspace},
	} {
		got, ok := ParsePayload(n.Payload())
		if !ok || got != n {
			t.Fatalf("round trip %q = %+v %t", n.Payload(), got, ok)
		}
	}
	if got := (Notice{Workspace: "demo", Kind: Job, TaskID: "ignored"}).Payload(); got != "demo:job" {
		t.Fatalf("job payload=%q", got)
	}
	for _, bad := range []string{"", "demo", ":job", "demo:nope", "demo:job:t1", "demo:task:"} {
		if _, ok := ParsePayload(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}
