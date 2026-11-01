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
	t.Run("NextDeadline", func(t *testing.T) { runNextDeadline(t, x) })

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
		// A configuration update raises a workspace notice, so a monitor
		// enabled in configuration reaches the timed GitHub fallback
		// (component-persistence, Change notices).
		current, err := st.WorkspaceConfig(systemCtx)
		if err != nil {
			t.Fatal(err)
		}
		next := &config.Config{Workspace: created}
		next.Monitor.Enabled = true
		if _, err := st.UpdateWorkspaceConfig(systemCtx, current.Version, next); err != nil {
			t.Fatal(err)
		}
		if n := awaitNotice(t, notices, "workspace notice after UpdateWorkspaceConfig"); n.Kind != changes.Workspace || n.Workspace != created {
			t.Fatalf("workspace notice=%+v", n)
		}
		updated, err := st.WorkspaceConfig(systemCtx)
		if err != nil || updated.Version != current.Version+1 {
			t.Fatalf("configuration not readable when its notice arrived: version=%d err=%v", updated.Version, err)
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

// runNextDeadline proves NextDeadline returns the earliest stored deadline
// strictly after its bound for the context's workspace only, and that a
// claim raises a deadline notice carrying its lease once the claim is
// readable (component-persistence, Deadlines; req-quiet-idle-runtime
// AC-2.4).
func runNextDeadline(t *testing.T, x Fixture) {
	st, ctx, ws := x.Backend, x.Context, x.Workspace
	hub := st.Changes()
	// Deadlines far from now keep the clock out of the comparison; times are
	// microseconds, as every backend stores them.
	base := time.Now().UTC().Add(240 * time.Hour).Truncate(time.Second)
	if _, ok, err := st.NextDeadline(ctx, time.Time{}); err != nil || ok {
		t.Fatalf("empty workspace NextDeadline ok=%t err=%v", ok, err)
	}
	order := func(queueDeadline time.Time) core.WorkOrder {
		t.Helper()
		taskID := createTask(t, st, ctx, ws, core.TaskRunning, core.StageImplement)
		id := taskID + "-implement-1"
		now := time.Now().UTC()
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: queueDeadline, CreatedAt: now}))
		stored, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		return stored
	}
	queued := order(base.Add(3 * time.Hour))
	next, ok, err := st.NextDeadline(ctx, time.Time{})
	if err != nil || !ok || !next.Equal(queued.QueueDeadline) {
		t.Fatalf("queued NextDeadline=%s ok=%t err=%v, want queue deadline %s", next, ok, err, queued.QueueDeadline)
	}

	// A claim records the lease and the execution deadline; the earlier one
	// wins, and the claim's notice carries it.
	claimed := order(base.Add(5 * time.Hour))
	notices := hub.Subscribe(ws, changes.Deadline)
	defer notices.Close()
	session := "deadline-" + core.NewTaskID()
	claimedOrder, err := ClaimWorkOrder(ctx, st, claimed.ID, core.WorkOrderClaim{ClaimantID: "worker-deadline", SessionID: session, ClientToken: session + "-token", Lease: 30 * time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	claimedOrder, err = st.GetWorkOrder(ctx, claimed.ID)
	requireOK(t, err)
	lease := claimedOrder.LeaseExpiresAt
	if lease.IsZero() || !claimedOrder.ExecutionDeadline.After(lease) {
		t.Fatalf("claimed order lease=%s execution=%s", lease, claimedOrder.ExecutionDeadline)
	}
	gotLease := false
	for deadline := time.After(2 * time.Second); !gotLease; {
		select {
		case n := <-notices.C():
			if n.Kind != changes.Deadline || n.Workspace != ws {
				t.Fatalf("deadline notice=%+v", n)
			}
			gotLease = n.At.Equal(lease) || n.At.IsZero()
		case <-deadline:
			t.Fatal("no deadline notice carrying the lease after a claim")
		}
	}
	next, ok, err = st.NextDeadline(ctx, time.Time{})
	if err != nil || !ok || !next.Equal(lease) {
		t.Fatalf("claimed NextDeadline=%s ok=%t err=%v, want lease %s", next, ok, err, lease)
	}
	// after excludes the earliest: the next one is the execution deadline,
	// then the queued order's queue deadline.
	next, ok, err = st.NextDeadline(ctx, lease)
	if err != nil || !ok || !next.Equal(claimedOrder.ExecutionDeadline) {
		t.Fatalf("NextDeadline after the lease=%s ok=%t err=%v, want execution deadline %s", next, ok, err, claimedOrder.ExecutionDeadline)
	}
	next, ok, err = st.NextDeadline(ctx, claimedOrder.ExecutionDeadline)
	if err != nil || !ok || !next.Equal(queued.QueueDeadline) {
		t.Fatalf("NextDeadline after the execution deadline=%s ok=%t err=%v, want %s", next, ok, err, queued.QueueDeadline)
	}

	// A cancelled order records no deadline.
	cancelled := order(base.Add(time.Hour))
	cancelled.State = core.WorkOrderCancelled
	requireOK(t, UpdateWorkOrder(ctx, st, cancelled, core.WorkOrderCmdCancel))
	next, ok, err = st.NextDeadline(ctx, time.Time{})
	if err != nil || !ok || !next.Equal(lease) {
		t.Fatalf("NextDeadline with a cancelled order=%s ok=%t err=%v, want lease %s", next, ok, err, lease)
	}

	// Another workspace reads none of these.
	other := ws + "-deadline"
	systemCtx := store.WithActor(store.WithWorkspace(context.Background(), other), store.SystemActor())
	if _, err := st.CreateWorkspace(systemCtx, other, "Deadline", &config.Config{Workspace: other}); err != nil {
		t.Fatal(err)
	}
	if next, ok, err := st.NextDeadline(systemCtx, time.Time{}); err != nil || ok {
		t.Fatalf("other workspace NextDeadline=%s ok=%t err=%v", next, ok, err)
	}
}
