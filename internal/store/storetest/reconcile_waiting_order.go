package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// durableQueue is implemented by backends whose dispatch jobs live on the
// workspace event log (PostgreSQL, SingleStore). The memory backend keeps
// dispatch in-process, so the case applies to production backends only.
type durableQueue interface {
	Log() eventlog.Store
	ReconcileQueuedTasks(context.Context) (int, error)
}

// RunReconcileWaitingOrderConformance: a queued task whose current stage is
// held by a mint-blocking order (queued, or stale past its queue deadline; for
// review, any seat of the latest round) is waiting for a claim or the order
// clock, not drifting. Reconcile neither re-enqueues it nor appends
// dispatch.reconciled on any tick, EnsureTaskEnqueued refuses with
// store.ErrTaskHeldByWorkOrder naming the order, while a queued task with no order and a completed job is still
// repaired exactly once (component-durable-queue "Reconciliation between
// tasks and jobs"; DEC-62).
func RunReconcileWaitingOrderConformance(t *testing.T, st store.Store, ctx context.Context, workspace string, production bool) {
	if !production {
		t.Skip("memory dispatch is in-process; no durable jobs to reconcile")
	}
	durable, ok := st.(durableQueue)
	if !ok {
		t.Fatal("production backend has no durable dispatch queue")
	}
	now := time.Now().UTC()
	newQueued := func(stage core.Stage) string {
		t.Helper()
		id := core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: stage, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	withOrder := func(taskID string, stage core.Stage, deadline time.Time) {
		t.Helper()
		orderID := taskID + "-" + string(stage) + "-1"
		if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-48 * time.Hour), QueueDeadline: deadline}); err != nil {
			t.Fatal(err)
		}
	}
	waiting := newQueued(core.StageSpec)
	withOrder(waiting, core.StageSpec, now.Add(time.Hour))
	stale := newQueued(core.StageImplement)
	withOrder(stale, core.StageImplement, now.Add(-time.Second))
	drifted := newQueued(core.StageImplement)
	// The task left implement and bounced back: its old implement order is
	// history behind a later review order, so it holds nothing and the task
	// is real drift.
	bounced := newQueued(core.StageImplement)
	withOrder(bounced, core.StageImplement, now.Add(time.Hour))
	withOrder(bounced, core.StageReview, now.Add(time.Hour))
	// A review task holds its stage through any seat of its latest round:
	// seat 1 reads stale while seat 2, the latest order, is cancelled.
	review := newQueued(core.StageReview)
	for seat := range 2 {
		orderID := fmt.Sprintf("%s-review-1-seat-%d", review, seat+1)
		if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: review, Stage: core.StageReview, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: review, JobID: orderID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: seat + 1, QueueEnteredAt: now.Add(-48 * time.Hour), QueueDeadline: now.Add(-time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	cancelled, err := st.GetWorkOrder(ctx, review+"-review-1-seat-2")
	if err != nil {
		t.Fatal(err)
	}
	cancelled.State = core.WorkOrderCancelled
	if err = UpdateWorkOrder(ctx, st, cancelled, core.WorkOrderCmdCancel); err != nil {
		t.Fatal(err)
	}

	// The dispatch job ran and completed, as it does once an order exists.
	for _, id := range []string{waiting, stale, review, drifted, bounced} {
		completeDispatchJob(t, ctx, durable.Log(), workspace, id)
	}
	for tick := range 5 {
		repaired, err := durable.ReconcileQueuedTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if tick == 0 {
			want = 2 // the drifted and bounced tasks
		}
		if repaired != want {
			t.Fatalf("tick %d repaired=%d want %d", tick, repaired, want)
		}
	}
	// Operator task redispatch cannot move a held stage; it names the order.
	for _, id := range []string{waiting, stale, review} {
		if err := st.EnsureTaskEnqueued(ctx, id); !errors.Is(err, store.ErrTaskHeldByWorkOrder) || !strings.Contains(err.Error(), id+"-") {
			t.Fatalf("task %s redispatch err=%v", id, err)
		}
	}
	for id, want := range map[string]int{waiting: 0, stale: 0, review: 0, drifted: 1, bounced: 1} {
		if n, err := st.CountEvents(ctx, id, "dispatch.reconciled"); err != nil || n != want {
			t.Fatalf("task %s dispatch.reconciled=%d want %d err=%v", id, n, want, err)
		}
	}
	kind := queue.DispatchTaskArgs{}.Kind()
	for id, wantActive := range map[string]bool{waiting: false, stale: false, review: false, drifted: true, bounced: true} {
		job, err := logqueue.Load(ctx, durable.Log(), workspace, logqueue.StreamFor(kind, id))
		if err != nil || job.Active() != wantActive {
			t.Fatalf("task %s job active=%v want %v err=%v", id, job.Active(), wantActive, err)
		}
	}
}

func completeDispatchJob(t *testing.T, ctx context.Context, log eventlog.Store, workspace, taskID string) {
	t.Helper()
	stream := logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), taskID)
	job, err := logqueue.Load(ctx, log, workspace, stream)
	if err != nil {
		t.Fatal(err)
	}
	if !job.Active() {
		t.Fatalf("task %s has no active dispatch job to complete: %+v", taskID, job)
	}
	appendJobEvent := func(expected eventlog.Version, kind string, payload map[string]any) {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(store.WithActor(ctx, store.SystemActor()), workspace, stream, expected, []eventlog.NewEvent{{Kind: kind, ActorID: "storetest", ActorRole: "system", Payload: encoded}}); err != nil {
			t.Fatal(err)
		}
	}
	appendJobEvent(job.Head, logqueue.KindClaimed, map[string]any{"attempt": 1, "worker": "storetest", "claimed_at": time.Now().UTC()})
	appendJobEvent(job.Head+1, logqueue.KindCompleted, map[string]any{"attempt": 1})
}
