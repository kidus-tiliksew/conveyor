package taskops_test

import (
	"context"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestPlaneOwnsTaskLeaseAndCommitsCanonicalEvent(t *testing.T) {
	ctx := store.WithWorkspace(context.Background(), "demo")
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
	ctx := store.WithWorkspace(context.Background(), "demo")
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
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "order-clock", Workspace: "demo", State: core.TaskRunning}
	order := core.WorkOrder{
		ID: "order-clock-implement-1", TaskID: task.ID, JobID: "order-clock-implement-1",
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
	before, _ := st.CountEvents(ctx, task.ID, "work_order.stale")
	projected, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || projected.State != core.WorkOrderStale {
		t.Fatalf("projected=%+v err=%v", projected, err)
	}
	_, _ = st.ListWorkOrders(ctx)
	_, _ = st.ListTaskWorkOrders(ctx, task.ID)
	afterReads, _ := st.CountEvents(ctx, task.ID, "work_order.stale")
	if afterReads != before {
		t.Fatalf("observational reads emitted %d stale events, before=%d", afterReads, before)
	}
	count, err := taskops.New(st).TickOrderClock(ctx, now)
	if err != nil || count != 1 {
		t.Fatalf("clock count=%d err=%v", count, err)
	}
	afterTick, _ := st.CountEvents(ctx, task.ID, "work_order.stale")
	if afterTick != before+1 {
		t.Fatalf("clock stale events=%d before=%d", afterTick, before)
	}
}
