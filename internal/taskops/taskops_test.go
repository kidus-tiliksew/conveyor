package taskops_test

import (
	"context"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestPlaneOwnsTaskLeaseAndCommitsCanonicalEvent(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "taskops-lease", Workspace: "demo", State: core.TaskClaiming, NextStage: core.StageTriage}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyTaskCommand(ctx, taskops.TaskLease{}, task.ID, taskops.Command{Kind: core.TaskIntakeFinalize}); err == nil || !strings.Contains(err.Error(), "taskops lease") {
		t.Fatalf("zero lease error=%v", err)
	}
	outcome, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskIntakeFinalize})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Task.State != core.TaskQueued || outcome.Enqueued {
		t.Fatalf("outcome=%+v", outcome)
	}
	if count, err := st.CountEvents(ctx, task.ID, "task.state_changed"); err != nil || count != 1 {
		t.Fatalf("state events=%d err=%v", count, err)
	}
}

func TestWorkOrderCommandLeaseCannotBeForgedOrReusedForAnotherCommand(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "order-capability", Workspace: "demo", State: core.TaskQueued, NextStage: core.StageImplement}
	job := core.Job{ID: "order-capability-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrderCommand(ctx, taskops.TaskLease{}, order); err == nil {
		t.Fatal("zero lease created a work order")
	}
	if _, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdRedispatch, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, st.CreateWorkOrderCommand(ctx, lease, order)
	}); err == nil {
		t.Fatal("redispatch lease was reused for order.create")
	}
	if _, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, st.CreateWorkOrderCommand(ctx, lease, order)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReadsArePureAndOrderClockPersistsCanonicalCommands(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	seed := func(taskID string) core.WorkOrder {
		t.Helper()
		task := core.Task{ID: taskID, Workspace: "demo", State: core.TaskRunning}
		order := core.WorkOrder{
			ID: taskID + "-implement-1", TaskID: task.ID, JobID: taskID + "-implement-1",
			Stage: core.StageImplement, State: core.WorkOrderQueued,
			QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: now.Add(-time.Minute),
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		return order
	}
	// An order nobody claimed reads as queued past its stored queue deadline,
	// and the clock has nothing to persist for it (component-work-orders
	// invariant 5). This replaces the retired expectation that reads
	// projected it stale and the clock persisted work_order.stale.
	waiting := seed("order-clock-waiting")
	// A claimed order past its execution deadline reads as timed_out, and only
	// the clock persists that transition (component-work-orders invariant 8).
	timing := seed("order-clock")
	if _, err := storetest.ClaimWorkOrder(ctx, st, timing.ID, core.WorkOrderClaim{SessionID: "clock", ClientToken: "clock", Lease: time.Hour, ExecutionTimeout: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	before, _ := st.CountEvents(ctx, timing.TaskID, "work_order.timed_out")
	projected, err := st.GetWorkOrder(ctx, timing.ID)
	if err != nil || projected.State != core.WorkOrderTimedOut {
		t.Fatalf("projected=%+v err=%v", projected, err)
	}
	if read, err := st.GetWorkOrder(ctx, waiting.ID); err != nil || read.State != core.WorkOrderQueued || !read.Claimable {
		t.Fatalf("waiting read=%+v err=%v", read, err)
	}
	_, _ = st.ListWorkOrders(ctx)
	_, _ = st.ListTaskWorkOrders(ctx, timing.TaskID)
	afterReads, _ := st.CountEvents(ctx, timing.TaskID, "work_order.timed_out")
	if afterReads != before {
		t.Fatalf("observational reads emitted %d timed_out events, before=%d", afterReads, before)
	}
	count, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
	if err != nil || count != 1 {
		t.Fatalf("clock count=%d err=%v", count, err)
	}
	afterTick, _ := st.CountEvents(ctx, timing.TaskID, "work_order.timed_out")
	if afterTick != before+1 {
		t.Fatalf("clock timed_out events=%d before=%d", afterTick, before)
	}
	if stale, _ := st.CountEvents(ctx, waiting.TaskID, "work_order.stale"); stale != 0 {
		t.Fatalf("waiting order stale events=%d, want none", stale)
	}
}

// clockBackend fails ApplyWorkOrderClock for the tasks in fail and counts one
// transition for every other task.
type clockBackend struct {
	taskops.Backend
	ids     []string
	fail    map[string]error
	applied []string
	cancel  func()
}

func (b *clockBackend) ListElapsedWorkOrderTaskIDs(context.Context, time.Time) ([]string, error) {
	return b.ids, nil
}

func (b *clockBackend) ApplyWorkOrderClock(_ context.Context, _ taskops.TaskLease, taskID string, _ time.Time) (int, error) {
	b.applied = append(b.applied, taskID)
	if b.cancel != nil {
		b.cancel()
	}
	if err := b.fail[taskID]; err != nil {
		return 0, err
	}
	return 1, nil
}

// A failing first task must not stop later tasks' deadlines, and the error
// still returns so the deadline alarm retries (req-quiet-idle-runtime AC-2.4).
func TestOrderClockContinuesPastFailingTaskAndReturnsJoinedError(t *testing.T) {
	backend := &clockBackend{
		ids:  []string{"task-a", "task-b", "task-c"},
		fail: map[string]error{"task-a": store.ErrRetryable, "task-c": errors.New("boom")},
	}
	count, err := taskops.New(backend).TickOrderClock(context.Background(), time.Now().UTC())
	if count != 1 {
		t.Fatalf("count=%d, want 1", count)
	}
	if strings.Join(backend.applied, ",") != "task-a,task-b,task-c" {
		t.Fatalf("applied=%v", backend.applied)
	}
	if err == nil || !errors.Is(err, store.ErrRetryable) {
		t.Fatalf("err=%v, want joined error matching store.ErrRetryable", err)
	}
	for _, want := range []string{"task task-a:", "task task-c: boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%q lacks %q", err, want)
		}
	}
}

func TestOrderClockStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &clockBackend{ids: []string{"task-a", "task-b"}, cancel: cancel}
	count, err := taskops.New(backend).TickOrderClock(ctx, time.Now().UTC())
	if count != 1 || strings.Join(backend.applied, ",") != "task-a" {
		t.Fatalf("count=%d applied=%v", count, backend.applied)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}
