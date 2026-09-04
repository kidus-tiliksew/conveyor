package postgres

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestLogQueueEnqueuesInsideStoreTransactions: with the store on the log
// queue, creating a queued task enqueues a dispatch job in the same
// transaction as the task's rows, duplicates are suppressed while the job
// is active, and the reconciliation reads see the log's answer.
func TestLogQueueEnqueuesInsideStoreTransactions(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	log := st.Log()
	kind := queue.DispatchTaskArgs{}.Kind()

	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, phase61Task(workspace, taskID, core.TaskQueued, "")); err != nil {
		t.Fatal(err)
	}
	job, err := logqueue.Load(ctx, log, workspace, logqueue.StreamFor(kind, taskID))
	if err != nil {
		t.Fatal(err)
	}
	if job.State != logqueue.StateAvailable || job.MaxAttempts != queue.DispatchTaskMaxAttempts || job.Generation != 1 {
		t.Fatalf("dispatch job after create=%+v", job)
	}
	var args queue.DispatchTaskArgs
	if err := json.Unmarshal(job.Args, &args); err != nil || args.TaskID != taskID || args.WorkspaceID != workspace {
		t.Fatalf("job args=%s err=%v", job.Args, err)
	}

	// A second enqueue while the job is active is a no-op, so the
	// reconciliation read reports nothing to repair.
	if err := st.EnsureTaskEnqueued(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if job, _ = logqueue.Load(ctx, log, workspace, job.Stream); job.Generation != 1 {
		t.Fatalf("duplicate enqueue opened generation %d", job.Generation)
	}
	repaired, err := st.ReconcileQueuedTasks(ctx)
	if err != nil || repaired != 0 {
		t.Fatalf("reconcile with active job repaired=%d err=%v", repaired, err)
	}

	// Complete the job by hand as a worker would; the task is still
	// queued, so reconciliation re-enqueues it: generation 2.
	appendQueueEvent(t, st, workspace, job.Stream, job.Head, logqueue.KindClaimed, map[string]any{"attempt": 1, "worker": "test", "claimed_at": time.Now().UTC()})
	appendQueueEvent(t, st, workspace, job.Stream, job.Head+1, logqueue.KindCompleted, map[string]any{"attempt": 1})
	repaired, err = st.ReconcileQueuedTasks(ctx)
	if err != nil || repaired != 1 {
		t.Fatalf("reconcile after completion repaired=%d err=%v", repaired, err)
	}
	if job, _ = logqueue.Load(ctx, log, workspace, job.Stream); job.Generation != 2 || job.State != logqueue.StateAvailable {
		t.Fatalf("job after reconcile=%+v", job)
	}
	if count, err := st.CountEvents(ctx, taskID, "dispatch.reconciled"); err != nil || count != 1 {
		t.Fatalf("dispatch.reconciled events=%d err=%v", count, err)
	}

	// The job events are the workspace's log: nothing else writes to it.
	tail, err := log.Tail(ctx, workspace, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) == 0 {
		t.Fatal("tail is empty")
	}
	for _, e := range tail {
		if e.Stream.Type() != logqueue.StreamType {
			t.Fatalf("unexpected stream %s on the workspace log", e.Stream)
		}
	}
}

// TestLogQueueExhaustedDispatchParksRunningTask: a running task whose
// dispatch job was discarded after its final attempt is parked by
// reconciliation with failure evidence.
func TestLogQueueExhaustedDispatchParksRunningTask(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	kind := queue.DispatchTaskArgs{}.Kind()

	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, phase61Task(workspace, taskID, core.TaskQueued, "")); err != nil {
		t.Fatal(err)
	}
	stream := logqueue.StreamFor(kind, taskID)
	job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil || job.State != logqueue.StateAvailable {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	// The worker claims and the task starts running; then every attempt
	// fails and the job is discarded.
	appendQueueEvent(t, st, workspace, stream, job.Head, logqueue.KindClaimed, map[string]any{"attempt": 1, "worker": "test", "claimed_at": time.Now().UTC()})
	if _, err := st.pool.Exec(ctx, `UPDATE tasks SET state = 'running' WHERE workspace_id = $1 AND id = $2`, workspace, taskID); err != nil {
		t.Fatal(err)
	}
	head := job.Head + 1
	for attempt := 1; attempt < queue.DispatchTaskMaxAttempts; attempt++ {
		appendQueueEvent(t, st, workspace, stream, head, logqueue.KindFailed, map[string]any{"attempt": attempt, "error": "boom", "next_at": time.Now().UTC()})
		appendQueueEvent(t, st, workspace, stream, head+1, logqueue.KindClaimed, map[string]any{"attempt": attempt + 1, "worker": "test", "claimed_at": time.Now().UTC()})
		head += 2
	}
	appendQueueEvent(t, st, workspace, stream, head, logqueue.KindDiscarded, map[string]any{"attempt": queue.DispatchTaskMaxAttempts, "error": "boom"})

	if _, err := st.ReconcileQueuedTasks(ctx); err != nil {
		t.Fatal(err)
	}
	parked, err := st.GetTask(ctx, taskID)
	if err != nil || parked.State != core.TaskParked {
		t.Fatalf("task after exhausted dispatch=%+v err=%v", parked, err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded bool
	for _, e := range events {
		if e.Kind == "dispatch.failed" && strings.Contains(string(e.Payload), "discarded the dispatch job") {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("no dispatch.failed event records the exhausted dispatch: %+v", events)
	}
}

func appendQueueEvent(t *testing.T, st *Store, workspace string, stream eventlog.StreamID, expected eventlog.Version, kind string, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Log().Append(t.Context(), workspace, stream, expected, []eventlog.NewEvent{{Kind: kind, ActorID: "test", ActorRole: "system", Payload: encoded}}); err != nil {
		t.Fatal(err)
	}
}
