package dispatch_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestPostgresPlanRevisionDecisionLoopIntegration(t *testing.T) {
	databaseURL := planRevisionIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	workspace := "plan-revision-" + core.NewTaskID()
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"spec":      {Model: "planner", Execution: config.ExecutionMCP, Timeout: time.Hour},
		"implement": {Model: "implementer", Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor.git", Base: "main"}}}
	if _, err = st.CreateWorkspace(store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman}), workspace, "Plan revision integration", cfg); err != nil {
		t.Fatal(err)
	}
	ctx = store.WithWorkspace(ctx, workspace)
	ctx, _, dispatcher, service, task, contested := newPlanRevisionLoopWithStore(t, ctx, st, workspace, core.NewTaskID())

	current, _ := st.GetTask(ctx, task.ID)
	latest, _, _ := st.GetLatestJob(ctx, task.ID)
	decision := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionRedirect, ReasonCode: dispatch.PlanRevisionApprovedReasonCode, Comment: "Preserve the compatibility adapter."}
	if err = dispatcher.HandleIntervention(ctx, current, latest, decision); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateIntervention(ctx, decision); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
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
	planOrder, err := storetest.For(st).ClaimWorkOrder(ctx, orders[1].ID, core.WorkOrderClaim{SessionID: "postgres-plan-session", ClientToken: "postgres-plan-token", WorkerID: "postgres-plan-worker", Agent: "codex", Model: "planner", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	orderContext, err := service.Get(ctx, planOrder.ID, planOrder.SessionID)
	if err != nil || orderContext.PlanRevision == nil || orderContext.PlanRevision.ContestedPlanVersion != 1 || orderContext.PlanRevision.PriorWorkOrderID != contested.ID {
		t.Fatalf("plan context=%+v err=%v", orderContext.PlanRevision, err)
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
	approved, ok, approvalErr := st.GetApprovedSpecVersion(ctx, task.ID)
	if err != nil || approvalErr != nil || !ok || approved.Version != 2 || len(orders) != 3 || orders[2].Stage != core.StageImplement || orders[2].ID == contested.ID {
		t.Fatalf("approved=%+v ok=%t orders=%+v list_err=%v approval_err=%v", approved, ok, orders, err, approvalErr)
	}
}

func planRevisionIntegrationDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse CONVEYOR_TEST_DATABASE_URL: %v", err)
	}
	if !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
		t.Fatalf("refusing integration database %q: name must end in _test", parsed.Path)
	}
	return databaseURL
}
