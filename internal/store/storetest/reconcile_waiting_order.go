package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// durableQueue is implemented by backends whose dispatch jobs live on the
// workspace event log (PostgreSQL, SingleStore).
type durableQueue interface {
	Log() eventlog.Store
	ReconcileQueuedTasks(context.Context) (int, error)
}

type queueReconciler interface {
	ReconcileQueuedTasks(context.Context) (int, error)
}

// RunReconcileWaitingOrderConformance proves a queued task whose current
// implement stage is held by a stored claimed order behind a later review seat
// waits for the order clock rather than drifting. Durable reconcile neither
// re-enqueues it nor appends dispatch.reconciled across repeated ticks;
// Claims from a prior spec stage before implement, and from implement behind
// review before verify, do not hold the task's current stage.
// EnsureTaskEnqueued names the newest historical claim. Memory exercises the
// same holder predicate and no-op reconciler, while PostgreSQL and SingleStore
// also prove repair after the clock persists timeout (component-durable-queue
// "Reconciliation between tasks and jobs"; DEC-62).
func RunReconcileWaitingOrderConformance(t *testing.T, st store.Store, ctx context.Context, workspace string, production bool) {
	var durable durableQueue
	if production {
		var ok bool
		durable, ok = st.(durableQueue)
		if !ok {
			t.Fatal("production backend has no durable dispatch queue")
		}
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
	withClaimedBounce := func() (string, string) {
		t.Helper()
		taskID := newQueued(core.StageImplement)
		first := taskID + "-implement-1"
		if err := st.CreateJob(ctx, core.Job{ID: first, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: first, TaskID: taskID, JobID: first, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err := ClaimWorkOrder(ctx, st, first, core.WorkOrderClaim{
			SessionID: "reconcile-claimed-session", ClientToken: "reconcile-claimed-token",
			Lease: 4 * time.Hour, ExecutionTimeout: time.Hour,
		}); err != nil {
			t.Fatal(err)
		}
		ops := taskops.New(st)
		if _, err := ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		later := taskID + "-review-1-seat-1"
		if err := st.CreateJob(ctx, core.Job{ID: later, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: later, TaskID: taskID, JobID: later, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err := ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
			t.Fatal(err)
		}
		if _, err := ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageBounce, NextStage: core.StageImplement, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		return taskID, first
	}
	withClaimedDifferentStage := func(claimedStage, currentStage core.Stage) (string, string) {
		t.Helper()
		taskID := newQueued(claimedStage)
		orderID := fmt.Sprintf("%s-%s-1", taskID, claimedStage)
		if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: claimedStage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: claimedStage, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{
			SessionID: "reconcile-different-stage-session", ClientToken: "reconcile-different-stage-token",
			Lease: 4 * time.Hour, ExecutionTimeout: time.Hour,
		}); err != nil {
			t.Fatal(err)
		}
		ops := taskops.New(st)
		switch {
		case claimedStage == core.StageSpec && currentStage == core.StageImplement:
			if _, err := ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageImplement, ProjectStages: true}); err != nil {
				t.Fatal(err)
			}
		case claimedStage == core.StageImplement && currentStage == core.StageVerify:
			if _, err := ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
				t.Fatal(err)
			}
			if _, err := ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageVerify, ProjectStages: true}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unsupported different-stage fixture %s -> %s", claimedStage, currentStage)
		}
		return taskID, orderID
	}
	claimed, claimedOrder := withClaimedBounce()
	if err := st.EnsureTaskEnqueued(ctx, claimed); !errors.Is(err, store.ErrTaskHeldByWorkOrder) || !strings.Contains(err.Error(), claimedOrder) {
		t.Fatalf("claimed historical task enqueue err=%v, want holder %s", err, claimedOrder)
	}
	claimedReconciled, err := st.CountEvents(ctx, claimed, "dispatch.reconciled")
	if err != nil {
		t.Fatal(err)
	}
	differentSpec, differentSpecOrder := withClaimedDifferentStage(core.StageSpec, core.StageImplement)
	differentVerify, differentVerifyOrder := withClaimedDifferentStage(core.StageImplement, core.StageVerify)
	for _, fixture := range []struct {
		taskID  string
		orderID string
	}{
		{differentSpec, differentSpecOrder},
		{differentVerify, differentVerifyOrder},
	} {
		if err := st.EnsureTaskEnqueued(ctx, fixture.taskID); err != nil {
			t.Fatalf("different-stage task %s held by %s: %v", fixture.taskID, fixture.orderID, err)
		}
	}
	if !production {
		reconciler, ok := st.(queueReconciler)
		if !ok {
			t.Fatal("memory backend has no queue reconciler")
		}
		for tick := range 3 {
			repaired, err := reconciler.ReconcileQueuedTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if repaired != 0 {
				t.Fatalf("memory tick %d repaired=%d want 0", tick, repaired)
			}
		}
		if got, err := st.CountEvents(ctx, claimed, "dispatch.reconciled"); err != nil || got != claimedReconciled {
			t.Fatalf("memory dispatch.reconciled=%d err=%v, want %d", got, err, claimedReconciled)
		}
		return
	}
	completeDispatchJob(t, ctx, durable.Log(), workspace, claimed)
	for tick := range 3 {
		repaired, err := durable.ReconcileQueuedTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if repaired != 0 {
			t.Fatalf("claimed historical tick %d repaired=%d want 0", tick, repaired)
		}
	}
	if got, err := st.CountEvents(ctx, claimed, "dispatch.reconciled"); err != nil || got != claimedReconciled {
		t.Fatalf("claimed historical dispatch.reconciled=%d err=%v, want %d", got, err, claimedReconciled)
	}
	for _, id := range []string{differentSpec, differentVerify} {
		completeDispatchJob(t, ctx, durable.Log(), workspace, id)
	}
	repaired, err := durable.ReconcileQueuedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 2 {
		t.Fatalf("different-stage claims repaired=%d, want 2", repaired)
	}
	for _, id := range []string{differentSpec, differentVerify} {
		if got, err := st.CountEvents(ctx, id, "dispatch.reconciled"); err != nil || got != 1 {
			t.Fatalf("different-stage task %s dispatch.reconciled=%d err=%v, want 1", id, got, err)
		}
	}
	if got, err := st.CountEvents(ctx, claimed, "dispatch.reconciled"); err != nil || got != claimedReconciled {
		t.Fatalf("claimed historical dispatch.reconciled=%d err=%v, want %d", got, err, claimedReconciled)
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
	for _, id := range []string{waiting, stale, review, claimed} {
		if err := st.EnsureTaskEnqueued(ctx, id); !errors.Is(err, store.ErrTaskHeldByWorkOrder) || !strings.Contains(err.Error(), id+"-") {
			t.Fatalf("task %s redispatch err=%v", id, err)
		}
	}
	for id, want := range map[string]int{waiting: 0, stale: 0, review: 0, drifted: 1, bounced: 1, claimed: claimedReconciled} {
		if n, err := st.CountEvents(ctx, id, "dispatch.reconciled"); err != nil || n != want {
			t.Fatalf("task %s dispatch.reconciled=%d want %d err=%v", id, n, want, err)
		}
	}
	if _, err := taskops.New(st).TickOrderClock(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	settled, err := st.GetWorkOrder(ctx, claimedOrder)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != core.WorkOrderTimedOut {
		t.Fatalf("claimed order state=%s, want timed_out after clock settlement", settled.State)
	}
	if _, err := durable.ReconcileQueuedTasks(ctx); err != nil {
		t.Fatal(err)
	}
	reconciled, err := st.CountEvents(ctx, claimed, "dispatch.reconciled")
	if err != nil {
		t.Fatal(err)
	}
	job, err := logqueue.Load(ctx, durable.Log(), workspace, logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), claimed))
	if err != nil {
		t.Fatal(err)
	}
	if reconciled == claimedReconciled && !job.Active() {
		t.Fatalf("settled task was neither repaired nor dispatched: reconciled=%d job=%+v", reconciled, job)
	}
	kind := queue.DispatchTaskArgs{}.Kind()
	for id, wantActive := range map[string]bool{waiting: false, stale: false, review: false, drifted: true, bounced: true} {
		job, err := logqueue.Load(ctx, durable.Log(), workspace, logqueue.StreamFor(kind, id))
		if err != nil || job.Active() != wantActive {
			t.Fatalf("task %s job active=%v want %v err=%v", id, job.Active(), wantActive, err)
		}
	}
	runReconcileStoredHoldsAndConcurrentRepair(t, st, ctx, workspace, durable, newQueued, withOrder, now)
}

// runReconcileStoredHoldsAndConcurrentRepair pins the incident shape behind
// task 261015-3721c6: a held queued task whose implement order went stale and
// was followed by a fresh queued order accumulated one dispatch.reconciled
// event per reconcile tick on a release without the 261013-2bd2e8 hold. Stored
// stale and timed-out current-stage orders hold the stage, so repeated ticks
// append nothing and enqueue nothing. A genuinely missing dispatch is repaired
// exactly once even when reconcilers run concurrently, because the
// per-workspace queue-reconcile lock serializes them and the second pass finds
// an active job (component-durable-queue "Reconciliation between tasks and
// jobs"; DEC-62; req-task-lifecycle-and-queue AC-1.2, AC-1.3).
func runReconcileStoredHoldsAndConcurrentRepair(t *testing.T, st store.Store, ctx context.Context, workspace string, durable durableQueue, newQueued func(core.Stage) string, withOrder func(string, core.Stage, time.Time), now time.Time) {
	t.Helper()
	transition := func(orderID string, state core.WorkOrderState, command core.WorkOrderCommand) {
		t.Helper()
		order, err := st.GetWorkOrder(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		order.State = state
		if err := UpdateWorkOrder(ctx, st, order, command); err != nil {
			t.Fatalf("order %s -> %s: %v", orderID, state, err)
		}
	}
	storedStale := newQueued(core.StageImplement)
	withOrder(storedStale, core.StageImplement, now.Add(time.Hour))
	transition(storedStale+"-implement-1", core.WorkOrderStale, core.WorkOrderCmdMarkStale)
	storedTimedOut := newQueued(core.StageImplement)
	withOrder(storedTimedOut, core.StageImplement, now.Add(time.Hour))
	transition(storedTimedOut+"-implement-1", core.WorkOrderTimedOut, core.WorkOrderCmdTimeout)
	// The incident: implement-1 went stale and the order clock minted
	// implement-2, which waits queued for a claim.
	incident := newQueued(core.StageImplement)
	withOrder(incident, core.StageImplement, now.Add(time.Hour))
	transition(incident+"-implement-1", core.WorkOrderStale, core.WorkOrderCmdMarkStale)
	second := incident + "-implement-2"
	if err := st.CreateJob(ctx, core.Job{ID: second, TaskID: incident, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: second, TaskID: incident, JobID: second, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	missing := newQueued(core.StageImplement)
	held := []string{storedStale, storedTimedOut, incident}
	for _, id := range append(append([]string{}, held...), missing) {
		completeDispatchJob(t, ctx, durable.Log(), workspace, id)
	}

	// Concurrent reconcilers start together behind a barrier.
	const reconcilers = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, reconcilers)
	for range reconcilers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := durable.ReconcileQueuedTasks(ctx); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent reconcile: %v", err)
	}
	// Repeated ticks after the repair change nothing.
	for tick := range 3 {
		if _, err := durable.ReconcileQueuedTasks(ctx); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	kind := queue.DispatchTaskArgs{}.Kind()
	for _, id := range held {
		if n, err := st.CountEvents(ctx, id, "dispatch.reconciled"); err != nil || n != 0 {
			t.Fatalf("held task %s dispatch.reconciled=%d err=%v, want 0", id, n, err)
		}
		job, err := logqueue.Load(ctx, durable.Log(), workspace, logqueue.StreamFor(kind, id))
		if err != nil || job.Active() {
			t.Fatalf("held task %s job active=%v err=%v, want no useless dispatch", id, job.Active(), err)
		}
		if err := st.EnsureTaskEnqueued(ctx, id); !errors.Is(err, store.ErrTaskHeldByWorkOrder) {
			t.Fatalf("held task %s redispatch err=%v, want ErrTaskHeldByWorkOrder", id, err)
		}
	}
	if n, err := st.CountEvents(ctx, missing, "dispatch.reconciled"); err != nil || n != 1 {
		t.Fatalf("missing task dispatch.reconciled=%d err=%v, want exactly 1 after concurrent passes", n, err)
	}
	job, err := logqueue.Load(ctx, durable.Log(), workspace, logqueue.StreamFor(kind, missing))
	if err != nil || !job.Active() {
		t.Fatalf("missing task job active=%v err=%v, want one repaired dispatch", job.Active(), err)
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
