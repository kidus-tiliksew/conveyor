package storetest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunChangeNoticeConformance proves every backend raises task, job, and
// workspace change notices after the change commits, so that the change is
// readable when the notice arrives, and raises none for a refused write or a
// suppressed enqueue, with peers off and again after EnablePeers
// (component-persistence, Change notices; DEC-99(1)).
func RunChangeNoticeConformance(t *testing.T, x Fixture) {
	st, ctx, ws := x.Backend, x.Context, x.Workspace
	hub := st.Changes()
	if hub == nil {
		t.Fatal("backend has no change-notice hub")
	}
	taskID := createTask(t, st, ctx, ws, core.TaskRunning, "")
	// The refused write targets its own task, so a peer listener's
	// republished copy of an earlier notice cannot be mistaken for it.
	refusedTaskID := createTask(t, st, ctx, ws, core.TaskRunning, "")
	assertRound := func(t *testing.T, round string) {
		t.Helper()
		taskNotices := hub.SubscribeTask(ws, taskID)
		defer taskNotices.Close()
		refusedNotices := hub.SubscribeTask(ws, refusedTaskID)
		defer refusedNotices.Close()
		jobNotices := hub.Subscribe(ws, changes.Job)
		defer jobNotices.Close()

		kind := "change_notice." + round
		if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind}); err != nil {
			t.Fatal(err)
		}
		n := awaitNotice(t, taskNotices, "task notice after AppendEvent")
		if n.Kind != changes.Task || n.Workspace != ws || n.TaskID != taskID {
			t.Fatalf("task notice=%+v", n)
		}
		events, err := st.ListEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(events, func(e core.Event) bool { return e.Kind == kind }) {
			t.Fatalf("event %s not readable when its notice arrived", kind)
		}
		refused := store.WithWorkspace(context.Background(), ws)
		if err := st.AppendEvent(refused, core.Event{TaskID: refusedTaskID, Kind: kind + ".refused"}); err == nil {
			t.Fatal("actorless event write accepted")
		}
		expectQuiet(t, refusedNotices, "refused event write")

		probe := "change_notice_probe"
		key := round + "-" + core.NewTaskID()
		inserted, err := logqueue.Enqueue(ctx, st.Log(), ws, probe, key, map[string]string{"key": key}, 1, time.Now().UTC())
		if err != nil || !inserted {
			t.Fatalf("enqueue inserted=%t err=%v", inserted, err)
		}
		if n := awaitNotice(t, jobNotices, "job notice after enqueue"); n.Kind != changes.Job || n.Workspace != ws {
			t.Fatalf("job notice=%+v", n)
		}
		stream := logqueue.StreamFor(probe, key)
		head, err := st.Log().Head(ctx, ws, stream)
		if err != nil || head != eventlog.Version(1) {
			t.Fatalf("job stream head=%d err=%v when its notice arrived", head, err)
		}
		// With peers on, PostgreSQL also delivers this daemon's own
		// notification to its listener, which republishes it; let that copy
		// arrive before proving the suppressed enqueue raises nothing.
		settle(jobNotices)
		inserted, err = logqueue.Enqueue(ctx, st.Log(), ws, probe, key, map[string]string{"key": key}, 1, time.Now().UTC())
		if err != nil || inserted {
			t.Fatalf("duplicate enqueue inserted=%t err=%v", inserted, err)
		}
		expectQuiet(t, jobNotices, "suppressed enqueue")
	}
	t.Run("PeersOff", func(t *testing.T) { assertRound(t, "off") })

	t.Run("Workspace", func(t *testing.T) {
		created := ws + "-notice"
		notices := hub.Subscribe(created, changes.Workspace)
		defer notices.Close()
		refusedCtx := store.WithWorkspace(context.Background(), created)
		if _, err := st.CreateWorkspace(refusedCtx, created, "Notice", &config.Config{Workspace: created}); err == nil {
			t.Fatal("actorless workspace creation accepted")
		}
		expectQuiet(t, notices, "refused workspace creation")
		systemCtx := store.WithActor(store.WithWorkspace(context.Background(), created), store.SystemActor())
		if _, err := st.CreateWorkspace(systemCtx, created, "Notice", &config.Config{Workspace: created}); err != nil {
			t.Fatal(err)
		}
		if n := awaitNotice(t, notices, "workspace notice after CreateWorkspace"); n.Kind != changes.Workspace || n.Workspace != created {
			t.Fatalf("workspace notice=%+v", n)
		}
		if _, err := st.GetWorkspace(systemCtx, created); err != nil {
			t.Fatalf("workspace not readable when its notice arrived: %v", err)
		}
	})

	t.Run("PeersOn", func(t *testing.T) {
		notifies := st.EnablePeers(ctx)
		if again := st.EnablePeers(ctx); again != notifies {
			t.Fatalf("EnablePeers changed its answer: %t then %t", notifies, again)
		}
		if !st.IsDurable() && notifies {
			t.Fatal("the volatile backend claims to notify peers")
		}
		t.Logf("EnablePeers notifies=%t", notifies)
		assertRound(t, "on")
	})
}

// awaitNotice waits for one notice. A committed change publishes before its
// write returns, so the bound only absorbs scheduling.
func awaitNotice(t *testing.T, s *changes.Subscription, what string) changes.Notice {
	t.Helper()
	select {
	case n := <-s.C():
		return n
	case <-time.After(2 * time.Second):
		t.Fatalf("no %s", what)
	}
	return changes.Notice{}
}

// settle drains s until it has been silent for 300ms, at most 3 seconds.
func settle(s *changes.Subscription) {
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-s.C():
		case <-time.After(300 * time.Millisecond):
			return
		case <-deadline:
			return
		}
	}
}

func expectQuiet(t *testing.T, s *changes.Subscription, what string) {
	t.Helper()
	select {
	case n := <-s.C():
		t.Fatalf("%s raised %+v", what, n)
	case <-time.After(50 * time.Millisecond):
	}
}
