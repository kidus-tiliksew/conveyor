package storetest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunTimedOutSiblingRetirementConformance proves stage re-entry retires a
// stored timed_out sibling with the same audit payload on every backend
// (component-work-orders "Stages and order minting"; DEC-62).
func RunTimedOutSiblingRetirementConformance(t *testing.T, x Fixture) {
	t.Helper()
	st, ctx, workspace := x.Backend, x.Context, x.Workspace
	taskID := "timed-out-sibling-" + core.NewTaskID()
	task := core.Task{
		ID: taskID, Workspace: workspace, Title: taskID, Repo: "conveyor",
		BaseBranch: "main", Branch: "conveyor/task-" + taskID,
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	requireOK(t, st.CreateTask(ctx, task))
	first := taskID + "-implement-1"
	requireOK(t, st.CreateJob(ctx, core.Job{ID: first, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: first, TaskID: taskID, JobID: first, Stage: core.StageImplement}))
	claimed, err := For(st).ClaimWorkOrder(ctx, first, core.WorkOrderClaim{
		SessionID: "timed-out-session", ClientToken: "timed-out-token",
		Lease: time.Hour, ExecutionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderTimedOut
	requireOK(t, UpdateWorkOrder(ctx, st, claimed, core.WorkOrderCmdTimeout))

	ops := taskops.New(st)
	if _, err = ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	later := taskID + "-review-1-seat-1"
	requireOK(t, st.CreateJob(ctx, core.Job{ID: later, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{
		ID: later, TaskID: taskID, JobID: later, Stage: core.StageReview,
		ReviewRound: 1, ReviewSeat: 1,
	}))
	if _, err = ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
		t.Fatal(err)
	}
	if _, err = ops.Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageBounce, NextStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}

	next := taskID + "-implement-2"
	created, err := CreateStageWorkOrder(ctx, st, core.Job{ID: next, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}, core.WorkOrder{ID: next, TaskID: taskID, JobID: next, Stage: core.StageImplement})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("stage re-entry refused beside a timed_out sibling")
	}
	old, err := st.GetWorkOrder(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != core.WorkOrderCancelled || !old.RetrySuppressed || old.RetrySuppressionReason != "superseded" {
		t.Fatalf("timed_out sibling=%+v", old)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var retired bool
	for _, event := range events {
		if event.Kind != "work_order.retired" {
			continue
		}
		var payload struct {
			WorkOrderID string              `json:"work_order_id"`
			PriorState  core.WorkOrderState `json:"prior_state"`
			Reason      string              `json:"reason"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID == first && payload.PriorState == core.WorkOrderTimedOut && payload.Reason == "superseded by stage re-entry" {
			retired = true
			break
		}
	}
	if !retired {
		t.Fatalf("timed_out retirement event missing for %s", first)
	}
}
