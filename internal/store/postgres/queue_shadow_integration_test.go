package postgres

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestQueueShadowDualEnqueuesAndKeepsRiverAuthoritative: with the shadow
// on, a queued task's dispatch lands in River and on the log in the same
// transaction, and the reconciliation reads still answer from River.
func TestQueueShadowDualEnqueuesAndKeepsRiverAuthoritative(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	var lines []string
	st.EnableQueueShadow(func(format string, args ...any) { lines = append(lines, format) })
	st.EnableQueueShadow(nil) // idempotent
	if _, ok := st.queue.(*shadowDispatchQueue); !ok {
		t.Fatalf("queue=%T, want shadow wrapper", st.queue)
	}
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})

	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, phase61Task(workspace, taskID, core.TaskQueued, "")); err != nil {
		t.Fatal(err)
	}
	var riverRows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind='dispatch_task' AND args->>'workspace_id'=$1 AND args->>'task_id'=$2`, workspace, taskID).Scan(&riverRows); err != nil {
		t.Fatal(err)
	}
	if riverRows != 1 {
		t.Fatalf("river rows=%d", riverRows)
	}
	job, err := logqueue.Load(ctx, st.Log(), workspace, logqueue.StreamFor(queueargs.DispatchTaskArgs{}.Kind(), taskID))
	if err != nil || job.State != logqueue.StateAvailable || job.Generation != 1 {
		t.Fatalf("mirrored job=%+v err=%v", job, err)
	}

	// A duplicate enqueue is suppressed on both sides, with no mismatch
	// logged, and River's answer drives reconciliation.
	if err := st.EnsureTaskEnqueued(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if job, _ = logqueue.Load(ctx, st.Log(), workspace, job.Stream); job.Generation != 1 {
		t.Fatalf("duplicate enqueue reached the log: generation %d", job.Generation)
	}
	repaired, err := st.ReconcileQueuedTasks(ctx)
	if err != nil || repaired != 0 {
		t.Fatalf("reconcile repaired=%d err=%v", repaired, err)
	}
	if len(lines) != 0 {
		t.Fatalf("shadow logged mismatches: %v", lines)
	}
}
