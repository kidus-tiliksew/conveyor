package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPolicyOnlyDispatchCarriesTimeoutAndNoExecutionPins(t *testing.T) {
	cfg := &config.Config{
		WorkOrderQueueTimeout: time.Hour,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Execution: config.ExecutionMCP, TimeoutText: "3h"},
		}},
	}
	order, err := BuildFutureWorkOrderRouting(cfg, core.Task{}, core.StageImplement)
	if err != nil {
		t.Fatal(err)
	}
	if order.ExecutionTimeoutText != "3h" {
		t.Fatalf("timeout=%q", order.ExecutionTimeoutText)
	}
	if order.RequiredModel != "" || order.RequiredHarness != "" || order.RequiredEffort != "" || order.RequiredHarnessConfig != nil {
		t.Fatalf("server execution pins survived: %+v", order)
	}
}

func TestPolicyOnlyReviewRoundFreezesSeatShapeNotExecution(t *testing.T) {
	cfg := &config.Config{
		WorkOrderQueueTimeout: time.Hour,
		Review:                config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}},
	}
	minimum := store.ReviewSeatMinimum(config.PolicyContract{}, len(cfg.Review.Seats))
	_, orders, err := BuildReviewRound(cfg, core.Task{ID: "policy-review"}, config.StageRoute{TimeoutText: "45m"}, 1, minimum, minimum)
	if err != nil || len(orders) != 2 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	for _, order := range orders {
		if order.RequiredModel != "" || order.RequiredHarness != "" || order.RequiredEffort != "" || order.RequiredHarnessConfig != nil {
			t.Fatalf("seat %d carries server execution pins: %+v", order.ReviewSeat, order)
		}
	}
}

func TestFrozenPolicySurvivesWorkspacePolicyEdits(t *testing.T) {
	intake := &config.Config{
		MaxBounces:            3,
		WorkOrderQueueTimeout: time.Hour,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"spec":      {Execution: config.ExecutionMCP, TimeoutText: "20m"},
			"implement": {Execution: config.ExecutionMCP, TimeoutText: "2h"},
			"review":    {Execution: config.ExecutionMCP, TimeoutText: "40m"},
		}},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}},
	}
	task := core.Task{ID: "frozen-policy", SetupContract: intake.FreezePolicy()}
	current := &config.Config{
		MaxBounces:            9,
		WorkOrderQueueTimeout: time.Hour,
		Routing: config.Routing{Triage: config.TriageRoute{Model: "title-model", TimeoutText: "1m"}, Stages: map[string]config.StageRoute{
			"spec":      {Execution: config.ExecutionMCP, TimeoutText: "50m"},
			"implement": {Execution: config.ExecutionMCP, TimeoutText: "8h"},
			"review":    {Execution: config.ExecutionMCP, TimeoutText: "3h"},
		}},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
	}
	implement, err := BuildFutureWorkOrderRouting(current, task, core.StageImplement)
	if err != nil || implement.ExecutionTimeoutText != "2h" {
		t.Fatalf("implementation order=%+v err=%v", implement, err)
	}
	// The frozen two-seat list stays the minimum after the workspace shrinks
	// to one seat (DEC-77).
	minimum := store.ReviewSeatMinimum(task.SetupContract, len(current.Review.Seats))
	_, reviews, err := BuildReviewRound(current, task, current.Routing.Stages["review"], 1, minimum, minimum)
	if err != nil || len(reviews) != 2 || reviews[0].ExecutionTimeoutText != "40m" {
		t.Fatalf("review orders=%+v err=%v", reviews, err)
	}
	frozen := current.WithPolicy(task.SetupContract)
	if frozen.MaxBounces != 3 || frozen.Routing.Triage != current.Routing.Triage || frozen.Routing.Stages["implement"].TimeoutText != "2h" || len(frozen.Review.Seats) != 2 {
		t.Fatalf("policy overlay replaced the deployment triage route or missed the frozen policy: %+v", frozen)
	}
}

// routingPlan is a valid execution plan for plan-state routing tests.
const routingPlan = "## Approach\nRoute it.\n\n## Files touched\n- internal/dispatch/dispatch.go\n\n## Ordering\n1. Route.\n\n## Risks\n- None.\n\n## Done criteria\n- Routing follows the plan state."

// routingPlanStateMoves returns the plan-state moves a task recorded after
// intake.
func routingPlanStateMoves(t *testing.T, ctx context.Context, st store.Store, taskID string) []core.PlanStateChange {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var moves []core.PlanStateChange
	for _, event := range events {
		if event.Kind != core.EventTaskPlanStateChanged {
			continue
		}
		var change core.PlanStateChange
		if err := json.Unmarshal(event.Payload, &change); err != nil {
			t.Fatal(err)
		}
		if change.Cause != core.PlanCauseIntake {
			moves = append(moves, change)
		}
	}
	return moves
}

// TestTriageProceedTable is component-triage invariant 29's table: the plan
// state alone selects the step a proceed verdict applies, and an unknown
// state selects none (req-task-plan-state AC-1.1; DEC-110(1); DEC-125(5)).
func TestTriageProceedTable(t *testing.T) {
	t.Parallel()
	for state, want := range map[core.PlanState]struct {
		command        core.TaskCommand
		next, recovery core.Stage
	}{
		core.PlanNotRequired:      {core.TaskStageAdvance, core.StageImplement, ""},
		core.PlanCompleted:        {core.TaskStageAdvance, core.StageImplement, ""},
		core.PlanNeeded:           {core.TaskStageAdvance, core.StageSpec, ""},
		core.PlanAwaitingApproval: {core.TaskGateSpec, "", core.StageImplement},
	} {
		command, next, recovery, err := TriageProceed(state)
		if err != nil || command != want.command || next != want.next || recovery != want.recovery {
			t.Fatalf("%s: proceed=%s next=%s recovery=%s err=%v, want %+v", state, command, next, recovery, err, want)
		}
	}
	if _, _, _, err := TriageProceed(""); err == nil {
		t.Fatal("an empty plan state selected a proceed step")
	}
}

// TestTriageProceedRoutesByPlanState drives a real triage verdict for each
// plan state through the memory store: no plan required and planning
// completed advance to implementation, planning needed advances to the plan
// stage, and a plan awaiting approval enters the plan_approval gate for the
// version recorded at intake. Triage moves no plan state, ignores the legacy
// spec_approval and level, and approves no plan (component-triage invariant
// 29; component-task-lifecycle invariant 4; req-task-plan-state AC-1.1).
func TestTriageProceedRoutesByPlanState(t *testing.T) {
	t.Parallel()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		task  core.Task
		state core.TaskState
		next  core.Stage
		gate  core.GateKind
	}{
		// The legacy spec_approval and level disagree with the plan state
		// and decide nothing.
		{name: "not_required", task: core.Task{PlanState: core.PlanNotRequired, Level: core.L3}, state: core.TaskQueued, next: core.StageImplement},
		{name: "needed", task: core.Task{PlanState: core.PlanNeeded, Level: core.L0}, state: core.TaskQueued, next: core.StageSpec},
		{name: "awaiting_approval", task: core.Task{IntakePlan: &core.IntakePlan{Content: routingPlan}}, state: core.TaskAwaiting, gate: core.GateKindPlanApproval},
		{name: "completed", task: core.Task{IntakePlan: &core.IntakePlan{Content: routingPlan, Approved: true}}, state: core.TaskQueued, next: core.StageImplement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "user:routing-operator", Role: core.ActorUser}), "demo")
			st := store.NewMemory()
			task := tc.task
			task.ID, task.Workspace, task.Repo, task.Title, task.Body = "route-"+tc.name, "demo", "api", "Route", "Route by plan state."
			task.BaseBranch, task.Branch, task.State, task.NextStage, task.CreatedAt = "main", "conveyor/task-route-"+tc.name, core.TaskQueued, core.StageTriage, time.Now()
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			created, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "triage", Timeout: time.Minute}, Stages: map[string]config.StageRoute{
				"spec":      {Execution: config.ExecutionMCP, Timeout: time.Hour},
				"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour},
			}}}
			agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"feature\",\"route\":\"proceed\",\"summary\":\"Ready.\"}\n```"}}
			dispatcher := New(st, cfg, agent)
			dispatcher.Pack = bundle
			dispatcher.DisableMemoryQueueForTest()
			if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != tc.state || current.GateKind != tc.gate || (tc.state == core.TaskQueued && current.NextStage != tc.next) {
				t.Fatalf("after triage task=%s next=%s gate=%s, want %s next=%s gate=%s", current.State, current.NextStage, current.GateKind, tc.state, tc.next, tc.gate)
			}
			if tc.gate == core.GateKindPlanApproval && current.RecoveryStage != core.StageImplement {
				t.Fatalf("plan gate recovery stage=%s, want implement", current.RecoveryStage)
			}
			if current.PlanState != created.PlanState || len(routingPlanStateMoves(t, ctx, st, task.ID)) != 0 {
				t.Fatalf("triage moved the plan state %s -> %s", created.PlanState, current.PlanState)
			}
			_, approved, err := st.GetApprovedSpecVersion(ctx, task.ID)
			if err != nil || approved != (created.PlanState == core.PlanCompleted) {
				t.Fatalf("approved plan present=%v err=%v for %s", approved, err, created.PlanState)
			}
		})
	}
}

// TestNoPlanApprovedWithoutPersonDecision submits a plan for tasks whose
// legacy policy would once have approved it automatically: policy version 0
// with level L2, and policy version 1 with spec_approval false. Every
// submission records an unapproved version with origin plan_order, moves the
// plan state to awaiting_approval (cause plan_submitted), and parks the task
// at the plan_approval gate. Only the gate's approve records the approval and
// planning completed (cause plan_approved); a redirect sets planning needed
// (cause plan_redirected) and returns the task to the plan stage
// (component-work-orders invariant 56; component-task-lifecycle invariant 54;
// req-task-plan-state AC-1.2, AC-1.3; DEC-125(6)).
func TestNoPlanApprovedWithoutPersonDecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		task     core.Task
		decision core.InterventionAction
	}{
		{name: "policy version 0 approve", task: core.Task{PolicyVersion: 0, Level: core.L2}, decision: core.InterventionApprove},
		{name: "spec_approval off approve", task: core.Task{PolicyVersion: 1, SpecApproval: false}, decision: core.InterventionApprove},
		{name: "spec_approval off redirect", task: core.Task{PolicyVersion: 1, SpecApproval: false}, decision: core.InterventionRedirect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := tc.task
			task.ID, task.Workspace, task.Repo, task.State, task.NextStage, task.PlanState, task.CreatedAt = "no-auto-"+strings.ReplaceAll(tc.name, " ", "-"), "demo", "api", core.TaskRunning, core.StageSpec, core.PlanNeeded, time.Now()
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobRunning}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			created, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "demo", MaxBounces: 3, Repos: []config.Repo{{Name: "api"}}}
			d := New(st, cfg, nil)
			d.DisableMemoryQueueForTest()
			commit := "0123456789abcdef0123456789abcdef01234567"
			submitted, err := d.ApplyExternalPlan(ctx, created, job, pipeline.StructuredPlan{Markdown: routingPlan, Decomposition: []pipeline.DecompositionItem{}, BaseCommit: commit}, "codex", "gpt")
			version := submitted.Version
			if err != nil {
				t.Fatal(err)
			}
			if version.Approved || version.Origin != core.PlanOriginPlanOrder || version.BaseCommit == nil || *version.BaseCommit != commit {
				t.Fatalf("submitted version=%+v", version)
			}
			if _, approved, _ := st.GetApprovedSpecVersion(ctx, task.ID); approved {
				t.Fatal("a submission approved the plan")
			}
			gated, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gated.State != core.TaskAwaiting || gated.GateKind != core.GateKindPlanApproval || gated.PlanState != core.PlanAwaitingApproval {
				t.Fatalf("after submission task=%s gate=%s plan=%s", gated.State, gated.GateKind, gated.PlanState)
			}
			operator := store.WithActor(ctx, store.Actor{ID: "user:operator", Role: core.ActorUser})
			if err = d.HandleIntervention(operator, gated, job, core.Intervention{TaskID: task.ID, JobID: job.ID, Action: tc.decision, ReasonCode: "decided", Comment: "Cover the legacy path."}); err != nil {
				t.Fatal(err)
			}
			decided, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			moves := routingPlanStateMoves(t, ctx, st, task.ID)
			if len(moves) != 2 || moves[0].To != core.PlanAwaitingApproval || moves[0].Cause != core.PlanCausePlanSubmitted {
				t.Fatalf("plan-state moves=%+v", moves)
			}
			_, approved, err := st.GetApprovedSpecVersion(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.decision {
			case core.InterventionApprove:
				if !approved || decided.PlanState != core.PlanCompleted || decided.NextStage != core.StageImplement || moves[1].To != core.PlanCompleted || moves[1].Cause != core.PlanCausePlanApproved {
					t.Fatalf("after approve task=%+v approved=%v moves=%+v", decided, approved, moves)
				}
			case core.InterventionRedirect:
				if approved || decided.PlanState != core.PlanNeeded || decided.NextStage != core.StageSpec || moves[1].To != core.PlanNeeded || moves[1].Cause != core.PlanCausePlanRedirected {
					t.Fatalf("after redirect task=%+v approved=%v moves=%+v", decided, approved, moves)
				}
			}
		})
	}
}
