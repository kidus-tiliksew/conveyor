package dispatch_test

import (
	"context"
	"encoding/json"
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
	// The approved revision sets planning needed from completed
	// (component-work-orders invariant 58; req-task-plan-state AC-3.4).
	requireLoopPlanState(t, ctx, st, task.ID, core.PlanCompleted, core.PlanNeeded, core.PlanCausePlanRevisionApproved)

	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 2 || orders[0].State != core.WorkOrderCancelled || orders[1].Stage != core.StageSpec {
		t.Fatalf("plan re-entry orders=%+v err=%v", orders, err)
	}
	jobs, _ := st.ListJobs(ctx, task.ID)
	cancelledEvents, _ := st.CountEvents(ctx, task.ID, "work_order.cancelled")
	if len(jobs) != 2 || jobs[0].State != core.JobFailed || cancelledEvents != 1 {
		t.Fatalf("contested job/events jobs=%+v cancelled_events=%d", jobs, cancelledEvents)
	}
	planOrder, err := storetest.For(st).ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "plan-session", ClientToken: "plan-token", Agent: "codex", Model: "planner", Lease: time.Minute, ExecutionTimeout: time.Hour})
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
	resumed, err := storetest.For(st).ClaimWorkOrder(ctx, orders[2].ID, core.WorkOrderClaim{SessionID: "resumed-implementation", ClientToken: "resumed-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	resumedContext, err := service.Get(ctx, resumed.ID, resumed.SessionID)
	if err != nil || resumedContext.Order.ID != resumed.ID {
		t.Fatalf("resumed context=%+v err=%v", resumedContext.Order, err)
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
	// A decline keeps the plan state (component-work-orders invariant 58).
	if declined, _ := st.GetTask(ctx, task.ID); declined.PlanState != core.PlanCompleted {
		t.Fatalf("declined plan state=%s, want completed", declined.PlanState)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, recovered.ID, core.WorkOrderClaim{SessionID: "retry-session", ClientToken: "retry-token", Agent: "codex", Model: "implementer", Lease: time.Minute, ExecutionTimeout: time.Hour})
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
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	return newPlanRevisionLoopWithStore(t, ctx, st, "demo", suffix)
}

func newPlanRevisionLoopWithStore(t *testing.T, ctx context.Context, st store.Store, workspace, suffix string) (context.Context, store.Store, *dispatch.Dispatcher, *workorder.Service, core.Task, core.WorkOrder) {
	t.Helper()
	return newPlanRevisionLoopIn(t, ctx, st, workspace, suffix, core.PlanAwaitingApproval)
}

// newPlanRevisionLoopIn claims an implement order and requests plan revision
// on a task at planState: awaiting_approval approves plan v1 first, so the
// request contests the completed plan, and not_required asks for planning.
func newPlanRevisionLoopIn(t *testing.T, ctx context.Context, st store.Store, workspace, suffix string, planState core.PlanState) (context.Context, store.Store, *dispatch.Dispatcher, *workorder.Service, core.Task, core.WorkOrder) {
	t.Helper()
	now := time.Now().UTC()
	task := core.Task{ID: "plan-revision-loop-" + suffix, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + suffix, PolicyVersion: 1, PlanState: planState, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if planState == core.PlanAwaitingApproval {
		plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "## Approach\nUse v1.\n\n## Files touched\n- old.go\n\n## Ordering\n1. Implement.\n\n## Risks\n- Drift.\n\n## Done criteria\n- v1 works.", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.ApproveSpecVersion(ctx, task.ID, plan.Version); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if _, err = storetest.For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "implement-session", ClientToken: "implement-token", Agent: "codex", Model: "implementer", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.Progress = "Inspected the affected API boundary."
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	result, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
		return st.RequestPlanRevisionCommand(ctx, lease, order.ID, core.WorkOrderClaimIdentity{ClaimantID: claimed.ClaimantID, SessionID: claimed.SessionID}, "the public API changed")
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"spec":      {Execution: config.ExecutionMCP, Timeout: time.Hour},
		"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour},
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

// requireLoopPlanState asserts the task's plan state and its latest recorded
// move.
func requireLoopPlanState(t *testing.T, ctx context.Context, st store.Store, taskID string, from, to core.PlanState, cause core.PlanStateCause) {
	t.Helper()
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var last *core.PlanStateChange
	for _, event := range events {
		if event.Kind != core.EventTaskPlanStateChanged {
			continue
		}
		var change core.PlanStateChange
		if err := json.Unmarshal(event.Payload, &change); err != nil {
			t.Fatal(err)
		}
		last = &change
	}
	if task.PlanState != to || last == nil || last.From == nil || *last.From != from || last.To != to || last.Cause != cause {
		t.Fatalf("plan state=%s latest move=%+v, want %s -> %s (%s)", task.PlanState, last, from, to, cause)
	}
}

// TestPlanningRequestFromNoPlanRequiredTask covers a request_plan_revision on
// a no-plan-required task's implement claim. Approval sets planning needed
// with cause planning_requested_approved and returns the task to the plan
// stage, whose order receives the request as plan-revision context; the plan
// then waits for a person's approval, and the fresh implementation receives
// it. A decline returns the task to implementation and keeps no plan
// required (component-work-orders invariants 57, 58, and 61;
// req-task-plan-state AC-3.3, AC-3.4).
func TestPlanningRequestFromNoPlanRequiredTask(t *testing.T) {
	t.Run("approved", func(t *testing.T) {
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		ctx, st, dispatcher, service, task, contested := newPlanRevisionLoopIn(t, ctx, store.NewMemory(), "demo", "planning-approved", core.PlanNotRequired)
		current, _ := st.GetTask(ctx, task.ID)
		latest, _, _ := st.GetLatestJob(ctx, task.ID)
		decision := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionRedirect, ReasonCode: dispatch.PlanRevisionApprovedReasonCode, Comment: "Plan the migration first."}
		if err := dispatcher.HandleIntervention(ctx, current, latest, decision); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateIntervention(ctx, decision); err != nil {
			t.Fatal(err)
		}
		requireLoopPlanState(t, ctx, st, task.ID, core.PlanNotRequired, core.PlanNeeded, core.PlanCausePlanningRequestedApproved)
		if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		orders, err := st.ListTaskWorkOrders(ctx, task.ID)
		if err != nil || len(orders) != 2 || orders[0].ID != contested.ID || orders[0].State != core.WorkOrderCancelled || orders[1].Stage != core.StageSpec {
			t.Fatalf("plan entry orders=%+v err=%v", orders, err)
		}
		planOrder, err := storetest.For(st).ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "plan-session", ClientToken: "plan-token", Agent: "codex", Model: "planner", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		planContext, err := service.Get(ctx, planOrder.ID, planOrder.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if planContext.PlanRevision == nil || planContext.PlanRevision.ContestedPlanVersion != 0 || !strings.Contains(planContext.RolePrompt, "asked that this task, which required no plan, be planned") || !strings.Contains(planContext.RolePrompt, "the public API changed") {
			t.Fatalf("planning request context=%+v prompt=%s", planContext.PlanRevision, planContext.RolePrompt)
		}
		plan := pipeline.StructuredPlan{Markdown: "## Approach\nPlan it.\n\n## Files touched\n- new.go\n\n## Ordering\n1. Plan.\n\n## Risks\n- Drift.\n\n## Done criteria\n- The migration is planned.", Decomposition: []pipeline.DecompositionItem{}}
		result, err := service.SubmitPlan(ctx, planOrder.ID, planOrder.SessionID, plan)
		if err != nil || result["version"] != 1 || result["approved"] != false {
			t.Fatalf("submit plan result=%+v err=%v", result, err)
		}
		requireLoopPlanState(t, ctx, st, task.ID, core.PlanNeeded, core.PlanAwaitingApproval, core.PlanCausePlanSubmitted)
		current, _ = st.GetTask(ctx, task.ID)
		latest, _, _ = st.GetLatestJob(ctx, task.ID)
		if err = dispatcher.HandleIntervention(ctx, current, latest, core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionApprove, ReasonCode: "plan-approved"}); err != nil {
			t.Fatal(err)
		}
		requireLoopPlanState(t, ctx, st, task.ID, core.PlanAwaitingApproval, core.PlanCompleted, core.PlanCausePlanApproved)
		if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		orders, err = st.ListTaskWorkOrders(ctx, task.ID)
		if err != nil || len(orders) != 3 || orders[2].Stage != core.StageImplement {
			t.Fatalf("fresh implementation orders=%+v err=%v", orders, err)
		}
		implementation, err := storetest.For(st).ClaimWorkOrder(ctx, orders[2].ID, core.WorkOrderClaim{SessionID: "implement-again", ClientToken: "implement-again-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		implementContext, err := service.Get(ctx, implementation.ID, implementation.SessionID)
		if err != nil || implementContext.ApprovedSpec == nil || implementContext.ApprovedSpec.Version != 1 {
			t.Fatalf("implementation approved_spec=%+v err=%v", implementContext.ApprovedSpec, err)
		}
	})
	t.Run("declined", func(t *testing.T) {
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		ctx, st, dispatcher, _, task, contested := newPlanRevisionLoopIn(t, ctx, store.NewMemory(), "demo", "planning-declined", core.PlanNotRequired)
		current, _ := st.GetTask(ctx, task.ID)
		latest, _, _ := st.GetLatestJob(ctx, task.ID)
		decision := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionRedirect, ReasonCode: dispatch.PlanRevisionDeclinedReasonCode, Comment: "Implement it as filed."}
		if err := dispatcher.HandleIntervention(ctx, current, latest, decision); err != nil {
			t.Fatal(err)
		}
		declined, err := st.GetTask(ctx, task.ID)
		if err != nil || declined.PlanState != core.PlanNotRequired || declined.NextStage != core.StageImplement {
			t.Fatalf("declined task=%+v err=%v", declined, err)
		}
		recovered, err := st.GetWorkOrder(ctx, contested.ID)
		if err != nil || recovered.State != core.WorkOrderQueued || !recovered.Claimable {
			t.Fatalf("recovered order=%+v err=%v", recovered, err)
		}
	})
}
