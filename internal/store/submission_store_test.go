package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestMemoryConflictFixCommandFailureLeavesNoPartialOutcome(t *testing.T) {
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemory()
	task := core.Task{ID: "conflict-atomic-memory", Workspace: "demo", State: core.TaskApproved, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := ConflictFixRequest{
		TaskID: task.ID, Job: job,
		WorkOrder:    core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, ReasonCode: "merge-conflict", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
		Intervention: core.Intervention{TaskID: task.ID, ActorID: "system", ActorRole: core.ActorSystem, Action: core.InterventionRedirect, ReasonCode: "merge-conflict"},
		ApprovedHead: "approved", NewHead: "conflicting",
	}
	_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (ConflictFixResult, error) {
		return st.CreateConflictFixCommand(ctx, lease, request)
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("command error=%v", err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	interventions, _ := st.ListInterventions(ctx, task.ID)
	if current.State != core.TaskApproved || current.NextStage != core.StageReview || len(orders) != 0 || len(interventions) != 0 {
		t.Fatalf("partial outcome: task=%+v orders=%+v interventions=%+v", current, orders, interventions)
	}
	for _, kind := range []string{"task.state_changed", "pipeline.transition_decided", "merge.conflict_fix_dispatched"} {
		if count, _ := st.CountEvents(ctx, task.ID, kind); count != 0 {
			t.Fatalf("%s events=%d, want 0", kind, count)
		}
	}
}
