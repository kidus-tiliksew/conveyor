package dispatch_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestPlanRevisionApproveReentersPlanAndBindsFreshImplementationToV2(t *testing.T) {
	ctx, st, dispatcher, service, task, contested := newPlanRevisionLoop(t, "approve")
	current, _ := st.GetTask(ctx, task.ID)
	latest, _, _ := st.GetLatestJob(ctx, task.ID)
	decision := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionRedirect, ReasonCode: dispatch.PlanRevisionApprovedReasonCode, Comment: "Keep the compatibility adapter."}
	if err := dispatcher.HandleIntervention(ctx, current, latest, decision); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(ctx, decision); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}

	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 2 || orders[0].State != core.WorkOrderCancelled || orders[1].Stage != core.StageSpec {
		t.Fatalf("plan re-entry orders=%+v err=%v", orders, err)
	}
	jobs, _ := st.ListJobs(ctx, task.ID)
	cancelledEvents, _ := st.CountEvents(ctx, task.ID, "work_order.cancelled")
	if len(jobs) != 2 || jobs[0].State != core.JobFailed || cancelledEvents != 1 {
		t.Fatalf("contested job/events jobs=%+v cancelled_events=%d", jobs, cancelledEvents)
	}
	planOrder, err := storetest.For(st).ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "plan-session", ClientToken: "plan-token", WorkerID: "plan-worker", Agent: "codex", Model: "planner", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	context, err := service.Get(ctx, planOrder.ID, planOrder.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if context.PlanRevision == nil || context.PlanRevision.ContestedPlanVersion != 1 || context.PlanRevision.PriorAttemptID != contested.LastAttemptID {
		t.Fatalf("plan revision context=%+v", context.PlanRevision)
	}
	for _, required := range []string{"# Plan revision context", "the public API changed", "# Operator direction\n\nKeep the compatibility adapter.", "# Historical prior-attempt checkpoint claims", "Inspected the affected API boundary."} {
		if !strings.Contains(context.RolePrompt, required) {
			t.Errorf("plan prompt missing %q: %s", required, context.RolePrompt)
		}
	}

	revised := pipeline.StructuredPlan{Markdown: "## Approach\nRevise the adapter.\n\n## Files touched\n- internal/dispatch/dispatch.go\n\n## Ordering\n1. Preserve compatibility.\n\n## Risks\n- Drift.\n\n## Done criteria\n- The revised adapter is covered.", Decomposition: []pipeline.DecompositionItem{}}
	result, err := service.SubmitPlan(ctx, planOrder.ID, planOrder.SessionID, revised)
	if err != nil || result["version"] != 2 || result["approved"] != false {
		t.Fatalf("submit revised plan result=%+v err=%v", result, err)
	}
	current, _ = st.GetTask(ctx, task.ID)
	latest, _, _ = st.GetLatestJob(ctx, task.ID)
	approval := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionApprove, ReasonCode: "plan-approved"}
	if err = dispatcher.HandleIntervention(ctx, current, latest, approval); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateIntervention(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err = st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 3 || orders[2].Stage != core.StageImplement || orders[2].ID == contested.ID {
		t.Fatalf("fresh implementation orders=%+v err=%v", orders, err)
	}
	approved, ok, err := st.GetApprovedSpecVersion(ctx, task.ID)
	if err != nil || !ok || approved.Version != 2 || approved.Content != strings.TrimSpace(revised.Markdown) {
		t.Fatalf("approved plan=%+v ok=%t err=%v", approved, ok, err)
	}
}

func TestPlanRevisionDeclineRecoversImplementationWithDirection(t *testing.T) {
	ctx, st, dispatcher, service, task, contested := newPlanRevisionLoop(t, "decline")
	current, _ := st.GetTask(ctx, task.ID)
	latest, _, _ := st.GetLatestJob(ctx, task.ID)
	direction := "Continue under plan v1 and keep the old endpoint."
	decision := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionRedirect, ReasonCode: dispatch.PlanRevisionDeclinedReasonCode, Comment: direction}
	if err := dispatcher.HandleIntervention(ctx, current, latest, decision); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(ctx, decision); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetWorkOrder(ctx, contested.ID)
	if err != nil || recovered.State != core.WorkOrderQueued || !recovered.Claimable || recovered.AutomaticRetryCount != 0 || recovered.OperatorDirection != direction {
		t.Fatalf("recovered order=%+v err=%v", recovered, err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, recovered.ID, core.WorkOrderClaim{SessionID: "retry-session", ClientToken: "retry-token", WorkerID: "retry-worker", Agent: "codex", Model: "implementer", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	context, err := service.Get(ctx, claimed.ID, claimed.SessionID)
	if err != nil || !strings.Contains(context.RolePrompt, "# Operator direction\n\n"+direction) {
		t.Fatalf("retry context prompt=%q err=%v", context.RolePrompt, err)
	}
}

func newPlanRevisionLoop(t *testing.T, suffix string) (context.Context, store.Store, *dispatch.Dispatcher, *workorder.Service, core.Task, core.WorkOrder) {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	return newPlanRevisionLoopWithStore(t, ctx, st, "demo", suffix)
}

func newPlanRevisionLoopWithStore(t *testing.T, ctx context.Context, st store.Store, workspace, suffix string) (context.Context, store.Store, *dispatch.Dispatcher, *workorder.Service, core.Task, core.WorkOrder) {
	t.Helper()
	now := time.Now().UTC()
	task := core.Task{ID: "plan-revision-loop-" + suffix, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + suffix, PolicyVersion: 1, SpecApproval: true, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "## Approach\nUse v1.\n\n## Files touched\n- old.go\n\n## Ordering\n1. Implement.\n\n## Risks\n- Drift.\n\n## Done criteria\n- v1 works.", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if _, err = storetest.For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "implement-session", ClientToken: "implement-token", WorkerID: "implement-worker", Agent: "codex", Model: "implementer", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.Progress = "Inspected the affected API boundary."
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	result, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
		return st.RequestPlanRevisionCommand(ctx, lease, order.ID, "implement-worker", claimed.SessionID, "the public API changed")
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"spec":      {Model: "planner", Execution: config.ExecutionMCP, Timeout: time.Hour},
		"implement": {Model: "implementer", Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor.git", Base: "main"}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: st, Dispatcher: dispatcher, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	return ctx, st, dispatcher, service, result.Task, result.WorkOrder
}
