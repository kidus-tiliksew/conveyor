package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunPlanRevisionConformance applies the agent-facing revision request
// contract to every store implementation (REQ-1, AC-1.1–AC-1.3).
func RunPlanRevisionConformance(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	t.Run("happy path is atomic and auditable", func(t *testing.T) {
		fixture := newPlanRevisionFixture(t, st, ctx, core.StageImplement, true, true)
		result, err := requestPlanRevision(ctx, st, fixture.order.TaskID, fixture.order.ID, fixture.workerID, fixture.sessionID, "  the API moved  ")
		if err != nil {
			t.Fatal(err)
		}
		if result.Rationale != "the API moved" || result.PlanVersion != 1 {
			t.Fatalf("result=%+v", result)
		}
		if result.WorkOrder.State != core.WorkOrderQueued || !result.WorkOrder.RetrySuppressed || result.WorkOrder.AutomaticRetryCount != 0 ||
			result.WorkOrder.LastFailureMessage != core.WorkOrderReleaseReasonPlanRevisionRequested || result.WorkOrder.SessionID != "" {
			t.Fatalf("released order=%+v", result.WorkOrder)
		}
		if result.Task.State != core.TaskAwaiting {
			t.Fatalf("task state=%s want %s", result.Task.State, core.TaskAwaiting)
		}
		events, err := st.ListEvents(ctx, fixture.order.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		var requestSeen, releaseSeen, gateSeen bool
		for _, event := range events {
			switch event.Kind {
			case "work_order.plan_revision_requested":
				var payload struct {
					Rationale   string `json:"rationale"`
					PlanVersion int    `json:"plan_version"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.Rationale == "the API moved" && payload.PlanVersion == 1 {
					requestSeen = true
				}
			case "work_order.released":
				var payload struct {
					Reason          string `json:"reason"`
					RetrySuppressed bool   `json:"retry_suppressed"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.Reason == core.WorkOrderReleaseReasonPlanRevisionRequested && payload.RetrySuppressed {
					releaseSeen = true
				}
			case "task.state_changed":
				var payload struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.Command == string(core.TaskGatePlanRevision) {
					gateSeen = true
				}
			}
		}
		if !requestSeen || !releaseSeen || !gateSeen {
			t.Fatalf("request=%v release=%v gate=%v events=%+v", requestSeen, releaseSeen, gateSeen, events)
		}
	})

	rejections := []struct {
		name      string
		stage     core.Stage
		approved  bool
		claimed   bool
		sessionID string
		rationale string
	}{
		{name: "blank rationale", stage: core.StageImplement, approved: true, claimed: true, sessionID: "session-a", rationale: " \t "},
		{name: "wrong stage", stage: core.StageReview, approved: true, claimed: true, sessionID: "session-a", rationale: "wrong plan"},
		{name: "missing approved plan", stage: core.StageImplement, claimed: true, sessionID: "session-a", rationale: "wrong plan"},
		{name: "stale session", stage: core.StageImplement, approved: true, claimed: true, sessionID: "session-stale", rationale: "wrong plan"},
		{name: "unclaimed", stage: core.StageImplement, approved: true, rationale: "wrong plan"},
	}
	for _, test := range rejections {
		t.Run(test.name+" preserves claim and projections", func(t *testing.T) {
			fixture := newPlanRevisionFixture(t, st, ctx, test.stage, test.approved, test.claimed)
			beforeOrder, err := st.GetWorkOrder(ctx, fixture.order.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeTask, err := st.GetTask(ctx, fixture.order.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			beforeEvents, err := st.ListEvents(ctx, fixture.order.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = requestPlanRevision(ctx, st, fixture.order.TaskID, fixture.order.ID, fixture.workerID, test.sessionID, test.rationale); err == nil {
				t.Fatal("request unexpectedly succeeded")
			}
			afterOrder, _ := st.GetWorkOrder(ctx, fixture.order.ID)
			afterTask, _ := st.GetTask(ctx, fixture.order.TaskID)
			afterEvents, _ := st.ListEvents(ctx, fixture.order.TaskID)
			if afterOrder.State != beforeOrder.State || afterOrder.SessionID != beforeOrder.SessionID || afterOrder.WorkerID != beforeOrder.WorkerID ||
				afterOrder.AutomaticRetryCount != beforeOrder.AutomaticRetryCount || afterOrder.RetrySuppressed != beforeOrder.RetrySuppressed ||
				afterTask.State != beforeTask.State || len(afterEvents) != len(beforeEvents) {
				t.Fatalf("rejection mutated state: order before=%+v after=%+v task %s->%s events %d->%d", beforeOrder, afterOrder, beforeTask.State, afterTask.State, len(beforeEvents), len(afterEvents))
			}
		})
	}
}

type planRevisionFixture struct {
	order     core.WorkOrder
	workerID  string
	sessionID string
}

func newPlanRevisionFixture(t *testing.T, st store.Store, ctx context.Context, stage core.Stage, approved, claimed bool) planRevisionFixture {
	t.Helper()
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspaceID(ctx), Repo: "app", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: stage, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if approved {
		plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "approved plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}), CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.ApproveSpecVersion(ctx, taskID, plan.Version); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{ID: taskID + "-job", TaskID: taskID, Stage: stage, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: taskID + "-order", TaskID: taskID, JobID: job.ID, Stage: stage, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	fixture := planRevisionFixture{order: order, workerID: "worker-a", sessionID: "session-a"}
	if claimed {
		claimedOrder, err := For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: fixture.sessionID, ClientToken: taskID + "-token", Agent: "codex", Model: "model", WorkerID: fixture.workerID, Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		fixture.order = claimedOrder
	}
	return fixture
}

func requestPlanRevision(ctx context.Context, st store.Store, taskID, orderID, workerID, sessionID, rationale string) (store.PlanRevisionRequestResult, error) {
	return taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
		return st.RequestPlanRevisionCommand(ctx, lease, orderID, workerID, sessionID, rationale)
	})
}

func workspaceID(ctx context.Context) string {
	workspace, _ := store.WorkspaceFromContext(ctx)
	return workspace
}
