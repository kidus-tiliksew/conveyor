package workorder

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
)

const planStateBaseCommit = "0123456789abcdef0123456789abcdef01234567"

// planStateMovesAfterIntake returns every task.plan_state_changed move a task
// recorded after its intake move.
func planStateMovesAfterIntake(t *testing.T, st store.Store, ctx context.Context, taskID string) []core.PlanStateChange {
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

// TestSubmitPlanBaseCommit covers submit_plan with a valid, an absent, and a
// malformed base_commit. A valid plan becomes the next version with origin
// plan_order and its base commit, moves the plan state to awaiting_approval
// with cause plan_submitted, parks the task at the plan_approval gate, and is
// never approved by the submission. A malformed base commit is refused in
// band: the order stays claimed and no version or move is recorded
// (component-work-orders invariant 56; component-mcp-protocol invariant 40;
// req-task-plan-state AC-1.2, REQ-4).
func TestSubmitPlanBaseCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		baseCommit string
		want       *string
	}{
		{name: "valid", baseCommit: planStateBaseCommit, want: ptr(planStateBaseCommit)},
		{name: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newPlanSeatsEnv(t, "plan-base-"+tc.name, true, frozenSeats(1), 1)
			order := env.claimPlan(t)
			result, err := env.service.SubmitPlan(env.ctx, order.ID, order.SessionID, pipeline.StructuredPlan{Markdown: planSeatsMarkdown, Decomposition: []pipeline.DecompositionItem{}, BaseCommit: tc.baseCommit})
			if err != nil {
				t.Fatal(err)
			}
			if result["approved"] != false || result["version"] != 1 {
				t.Fatalf("submit_plan result=%v, want version 1 not approved", result)
			}
			spec, ok, err := env.st.GetSpecVersion(env.ctx, env.taskID, 1)
			if err != nil || !ok {
				t.Fatalf("plan version ok=%v err=%v", ok, err)
			}
			if spec.Origin != core.PlanOriginPlanOrder || spec.Approved || (spec.BaseCommit == nil) != (tc.want == nil) || (tc.want != nil && *spec.BaseCommit != *tc.want) {
				t.Fatalf("plan version=%+v base=%v, want origin plan_order, unapproved, base %v", spec, spec.BaseCommit, tc.want)
			}
			task, err := env.st.GetTask(env.ctx, env.taskID)
			if err != nil {
				t.Fatal(err)
			}
			if task.PlanState != core.PlanAwaitingApproval || task.State != core.TaskAwaiting || task.GateKind != core.GateKindPlanApproval {
				t.Fatalf("task=%s/%s gate %s, want awaiting_approval at plan_approval", task.PlanState, task.State, task.GateKind)
			}
			moves := planStateMovesAfterIntake(t, env.st, env.ctx, env.taskID)
			if len(moves) != 1 || moves[0].From == nil || *moves[0].From != core.PlanNeeded || moves[0].To != core.PlanAwaitingApproval || moves[0].Cause != core.PlanCausePlanSubmitted {
				t.Fatalf("plan-state moves=%+v, want needed -> awaiting_approval (plan_submitted)", moves)
			}
			if _, approved, _ := env.st.GetApprovedSpecVersion(env.ctx, env.taskID); approved {
				t.Fatal("a submission approved the plan")
			}
		})
	}
	for name, malformed := range map[string]any{
		"uppercase": strings.ToUpper(planStateBaseCommit),
		"short":     planStateBaseCommit[:39],
		"long":      planStateBaseCommit + "0",
		"abbrev":    "0123456",
	} {
		t.Run("malformed "+name, func(t *testing.T) {
			env := newPlanSeatsEnv(t, "plan-base-bad-"+name, true, frozenSeats(1), 1)
			order := env.claimPlan(t)
			before := env.snapshot(t, order.ID)
			_, err := env.service.SubmitPlan(env.ctx, order.ID, order.SessionID, pipeline.StructuredPlan{Markdown: planSeatsMarkdown, Decomposition: []pipeline.DecompositionItem{}, BaseCommit: malformed.(string)})
			if err == nil || !strings.Contains(err.Error(), "base_commit") {
				t.Fatalf("malformed base_commit err=%v", err)
			}
			env.requireRefusedUnchanged(t, before, order)
			if _, ok, _ := env.st.GetLatestSpecVersion(env.ctx, env.taskID); ok {
				t.Fatal("a refused plan recorded a version")
			}
			task, err := env.st.GetTask(env.ctx, env.taskID)
			if err != nil {
				t.Fatal(err)
			}
			if task.PlanState != core.PlanNeeded || len(planStateMovesAfterIntake(t, env.st, env.ctx, env.taskID)) != 0 {
				t.Fatalf("a refused plan moved the plan state to %s", task.PlanState)
			}
		})
	}
}

func ptr(value string) *string { return &value }

// planStateContractEnv is a task at a given plan state with a claimed order
// of one stage, read through the real contract assembly.
type planStateContractEnv struct {
	ctx     context.Context
	st      store.Store
	service *Service
	task    core.Task
}

func newPlanStateContractEnv(t *testing.T, id string, state core.PlanState, plan *core.SpecVersion) planStateContractEnv {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	initial := state
	if plan != nil {
		initial = core.PlanAwaitingApproval
	}
	task := core.Task{ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageImplement, PlanState: initial, Body: "Make the widget blue.", Source: "api", CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		plan.TaskID = id
		created, err := st.CreateSpecVersion(ctx, *plan)
		if err != nil {
			t.Fatal(err)
		}
		if err = st.ApproveSpecVersion(ctx, id, created.Version); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PlanState != state {
		t.Fatalf("fixture plan state=%s, want %s", stored.PlanState, state)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	d := dispatch.New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	return planStateContractEnv{ctx: ctx, st: st, task: stored, service: &Service{Store: st, Pack: bundle, Dispatcher: d, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}}
}

func (e planStateContractEnv) contract(t *testing.T, stage core.Stage) Context {
	t.Helper()
	job := core.Job{ID: e.task.ID + "-" + string(stage) + "-1", TaskID: e.task.ID, Stage: stage, State: core.JobPending}
	if err := e.st.CreateJob(e.ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: e.task.ID, JobID: job.ID, Stage: stage}
	if stage == core.StageReview {
		order.ReviewRound, order.ReviewSeat = 1, 1
	}
	if err := storetest.For(e.st).CreateWorkOrder(e.ctx, order); err != nil {
		t.Fatal(err)
	}
	session := string(stage) + "-session"
	if _, err := e.service.Claim(e.ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", ClaimantID: string(stage) + "-agent", Agent: "codex", Model: string(stage), Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	contract, err := e.service.Get(e.ctx, job.ID, session)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

// TestContractByPlanState covers the implement and review contract of a
// completed task, which carries approved_spec with its version, content,
// origin, base commit, and approval time and states the base commit, or none
// recorded, and the approval time, and of a not_required task, which carries
// no approved_spec and takes the task body as the statement of done
// (component-work-orders invariant 61; component-review invariant 22;
// req-task-plan-state REQ-4, AC-1.5).
func TestContractByPlanState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		base *string
		want string
	}{
		{name: "base commit", base: ptr(planStateBaseCommit), want: "Base commit: `" + planStateBaseCommit + "`."},
		{name: "none recorded", want: "Base commit: none recorded."},
	} {
		t.Run("completed "+tc.name, func(t *testing.T) {
			env := newPlanStateContractEnv(t, "plan-contract-"+strings.ReplaceAll(tc.name, " ", "-"), core.PlanCompleted, &core.SpecVersion{Content: planSeatsMarkdown, Origin: core.PlanOriginPlanOrder, BaseCommit: tc.base})
			implement := env.contract(t, core.StageImplement)
			plan := implement.ApprovedSpec
			if plan == nil || plan.Version != 1 || plan.Content != planSeatsMarkdown || plan.Origin != core.PlanOriginPlanOrder || plan.ApprovedAt.IsZero() || (plan.BaseCommit == nil) != (tc.base == nil) {
				t.Fatalf("implement approved_spec=%+v", plan)
			}
			approvedAt := "Approved at: " + plan.ApprovedAt.UTC().Format(time.RFC3339) + "."
			for _, want := range []string{tc.want, approvedAt, "Before editing, check the plan's claims against your checkout.", "`request_plan_revision`", "`approved_spec.content`"} {
				if !strings.Contains(implement.RolePrompt, want) {
					t.Fatalf("implement contract lacks %q:\n%s", want, implement.RolePrompt)
				}
			}
			wire, err := json.Marshal(implement.Outbound())
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{`"approved_spec"`, `"origin":"plan_order"`, `"approved_at"`, `"base_commit"`} {
				if !strings.Contains(string(wire), field) {
					t.Fatalf("implement contract wire lacks %s: %s", field, wire)
				}
			}
			review := env.contract(t, core.StageReview)
			if review.ApprovedSpec == nil || review.ApprovedSpec.Version != 1 || !strings.Contains(review.RolePrompt, "Judge the `## Done criteria` section of `approved_spec.content`") {
				t.Fatalf("review approved_spec=%+v role=%s", review.ApprovedSpec, review.RolePrompt)
			}
		})
	}
	t.Run("not required", func(t *testing.T) {
		env := newPlanStateContractEnv(t, "plan-contract-not-required", core.PlanNotRequired, nil)
		implement := env.contract(t, core.StageImplement)
		if implement.ApprovedSpec != nil {
			t.Fatalf("not_required implement carries approved_spec %+v", implement.ApprovedSpec)
		}
		for _, want := range []string{"# Statement of done", "The task body is the statement of done", "ask that the task be planned"} {
			if !strings.Contains(implement.RolePrompt, want) {
				t.Fatalf("implement contract lacks %q:\n%s", want, implement.RolePrompt)
			}
		}
		if strings.Contains(implement.RolePrompt, "Base commit:") {
			t.Fatalf("not_required implement states a base commit:\n%s", implement.RolePrompt)
		}
		wire, err := json.Marshal(implement.Outbound())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), `"approved_spec"`) {
			t.Fatalf("not_required implement wire carries approved_spec: %s", wire)
		}
		review := env.contract(t, core.StageReview)
		if review.ApprovedSpec != nil || !strings.Contains(review.RolePrompt, "The task description is the statement of done") || !strings.Contains(review.RolePrompt, "Make the widget blue.") {
			t.Fatalf("not_required review approved_spec=%+v role=%s", review.ApprovedSpec, review.RolePrompt)
		}
	})
	// An approved version on a task set back to planning needed is history,
	// not the statement of done: only planning completed delivers a plan
	// (component-plan-state invariant 7).
	t.Run("needed after an approved version", func(t *testing.T) {
		env := newPlanStateContractEnv(t, "plan-contract-reneeded", core.PlanCompleted, &core.SpecVersion{Content: planSeatsMarkdown})
		if _, ok, err := dispatch.DeliveredApprovedPlan(env.ctx, env.st, env.task); err != nil || !ok {
			t.Fatalf("completed delivered ok=%v err=%v", ok, err)
		}
		reneeded := env.task
		reneeded.PlanState = core.PlanNeeded
		if _, ok, err := dispatch.DeliveredApprovedPlan(env.ctx, env.st, reneeded); err != nil || ok {
			t.Fatalf("needed delivered ok=%v err=%v", ok, err)
		}
	})
}
