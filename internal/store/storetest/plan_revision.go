package storetest

import (
	"context"
	"encoding/json"
	"errors"
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

	t.Run("live claim rejects a different authenticated claimant", func(t *testing.T) {
		fixture := newPlanRevisionFixture(t, st, ctx, core.StageImplement, true, true)
		wrong := core.WorkOrderClaimIdentity{ClaimantID: "worker-b", SessionID: fixture.sessionID}
		_, err := taskops.ExecuteWorkOrder(ctx, st, fixture.order.TaskID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
			return st.RequestPlanRevisionCommand(ctx, lease, fixture.order.ID, wrong, "wrong claimant")
		})
		if !errors.Is(err, store.ErrWorkOrderClaimUnauthorized) {
			t.Fatalf("plan revision error=%v", err)
		}
		release := core.WorkOrderRelease{SessionID: fixture.sessionID, Outcome: core.WorkOrderOutcomeReleased}
		_, err = taskops.ExecuteWorkOrder(ctx, st, fixture.order.TaskID, core.WorkOrderCmdRelease, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			return st.ReleaseWorkerClaimCommand(ctx, lease, fixture.order.ID, wrong, release)
		})
		if !errors.Is(err, store.ErrWorkOrderClaimUnauthorized) {
			t.Fatalf("release error=%v", err)
		}
		order, getErr := st.GetWorkOrder(ctx, fixture.order.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if order.State != core.WorkOrderClaimed || order.SessionID != fixture.sessionID {
			t.Fatalf("unauthorized mutation changed claim: %+v", order)
		}
	})

	// A task with no plan required may ask for planning: the request contests
	// no plan version and records none, and the gate stays decidable
	// (component-work-orders invariant 57; req-task-plan-state AC-3.3).
	t.Run("no plan required asks for planning", func(t *testing.T) {
		fixture := newPlanRevisionFixtureIn(t, st, ctx, core.StageImplement, core.PlanNotRequired, false, true)
		result, err := requestPlanRevision(ctx, st, fixture.order.TaskID, fixture.order.ID, fixture.workerID, fixture.sessionID, "this needs a plan")
		if err != nil {
			t.Fatal(err)
		}
		if result.PlanVersion != 0 || result.Task.State != core.TaskAwaiting || result.Task.PlanState != core.PlanNotRequired {
			t.Fatalf("result=%+v", result)
		}
		events, err := st.ListEvents(ctx, fixture.order.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		requested := false
		for _, event := range events {
			if event.Kind != "work_order.plan_revision_requested" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if _, carries := payload["plan_version"]; carries || payload["rationale"] != "this needs a plan" {
				t.Fatalf("planning request payload=%v, want a rationale and no contested version", payload)
			}
			requested = true
		}
		if !requested {
			t.Fatalf("no plan-revision request event: %+v", events)
		}
		task, err := st.GetTask(ctx, fixture.order.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		request, pending, err := store.PlanRevisionGateState(task, events)
		if err != nil || !pending || request.PlanVersion != 0 || request.WorkOrderID != fixture.order.ID {
			t.Fatalf("gate request=%+v pending=%v err=%v", request, pending, err)
		}
		requireNoPlanStateMove(t, events)
	})

	// Approving a planning request sets planning needed with cause
	// planning_requested_approved, and approving a revision of a completed
	// plan sets it with cause plan_revision_approved, each in the decision's
	// own transaction (component-work-orders invariant 58;
	// req-task-plan-state AC-3.4).
	for _, approval := range []struct {
		name  string
		from  core.PlanState
		cause core.PlanStateCause
	}{
		{"planning request approved", core.PlanNotRequired, core.PlanCausePlanningRequestedApproved},
		{"plan revision approved", core.PlanCompleted, core.PlanCausePlanRevisionApproved},
	} {
		t.Run(approval.name+" sets planning needed", func(t *testing.T) {
			approved := approval.from == core.PlanCompleted
			initial := approval.from
			if approved {
				initial = core.PlanAwaitingApproval
			}
			fixture := newPlanRevisionFixtureIn(t, st, ctx, core.StageImplement, initial, approved, true)
			if _, err := requestPlanRevision(ctx, st, fixture.order.TaskID, fixture.order.ID, fixture.workerID, fixture.sessionID, "replan"); err != nil {
				t.Fatal(err)
			}
			task, err := taskops.New(st).Perform(ctx, fixture.order.TaskID, taskops.Command{Kind: core.TaskInterventionRedirect, NextStage: core.StageSpec, ProjectStages: true, PlanState: &taskops.PlanStateMove{To: core.PlanNeeded, Cause: approval.cause}})
			if err != nil {
				t.Fatal(err)
			}
			if task.Task.PlanState != core.PlanNeeded || task.Task.NextStage != core.StageSpec {
				t.Fatalf("task after approval=%+v", task.Task)
			}
			requirePlanStateMove(t, st, ctx, fixture.order.TaskID, approval.from, core.PlanNeeded, approval.cause)
		})
	}

	// A declined request returns the task to implementation and keeps its
	// plan state (component-work-orders invariant 58).
	t.Run("decline keeps the plan state", func(t *testing.T) {
		fixture := newPlanRevisionFixtureIn(t, st, ctx, core.StageImplement, core.PlanNotRequired, false, true)
		if _, err := requestPlanRevision(ctx, st, fixture.order.TaskID, fixture.order.ID, fixture.workerID, fixture.sessionID, "replan"); err != nil {
			t.Fatal(err)
		}
		outcome, err := taskops.New(st).Perform(ctx, fixture.order.TaskID, taskops.Command{Kind: core.TaskInterventionRedirect, NextStage: core.StageImplement, ProjectStages: true})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := st.GetTask(ctx, fixture.order.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Task.PlanState != core.PlanNotRequired || stored.PlanState != core.PlanNotRequired || stored.NextStage != core.StageImplement {
			t.Fatalf("declined task=%+v stored=%+v", outcome.Task, stored)
		}
		events, err := st.ListEvents(ctx, fixture.order.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		requireNoPlanStateMove(t, events)
	})

	// At the plan_approval gate, approve records the version approval and
	// planning completed (cause plan_approved), and redirect records planning
	// needed (cause plan_redirected); plan submission records awaiting
	// approval (cause plan_submitted) with gate.spec (component-task-lifecycle
	// invariant 54; component-work-orders invariant 56; req-task-plan-state
	// AC-1.2, AC-1.3).
	for _, decision := range []string{"approve", "redirect"} {
		t.Run("plan gate "+decision, func(t *testing.T) {
			now := time.Now().UTC()
			taskID := core.NewTaskID()
			if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspaceID(ctx), Repo: "app", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			commit := "0123456789abcdef0123456789abcdef01234567"
			plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}), Origin: core.PlanOriginPlanOrder, BaseCommit: &commit, CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			plane := taskops.New(st)
			if _, err = plane.Perform(ctx, taskID, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true, PlanState: &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted}}); err != nil {
				t.Fatal(err)
			}
			requirePlanStateMove(t, st, ctx, taskID, core.PlanNeeded, core.PlanAwaitingApproval, core.PlanCausePlanSubmitted)
			gated, err := st.GetTask(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			if gated.State != core.TaskAwaiting || gated.GateKind != core.GateKindPlanApproval || gated.PlanState != core.PlanAwaitingApproval {
				t.Fatalf("gated task=%+v", gated)
			}
			if decision == "approve" {
				if err = st.ApproveSpecVersion(ctx, taskID, plan.Version); err != nil {
					t.Fatal(err)
				}
				requirePlanStateMove(t, st, ctx, taskID, core.PlanAwaitingApproval, core.PlanCompleted, core.PlanCausePlanApproved)
				approved, ok, err := st.GetApprovedSpecVersion(ctx, taskID)
				if err != nil || !ok || approved.Version != plan.Version || approved.Origin != core.PlanOriginPlanOrder || approved.BaseCommit == nil || *approved.BaseCommit != commit {
					t.Fatalf("approved plan=%+v ok=%v err=%v", approved, ok, err)
				}
				// The approve command's own move is a no-op once the
				// approval recorded planning completed.
				if _, err = plane.Perform(ctx, taskID, taskops.Command{Kind: core.TaskInterventionApproveSpec, NextStage: core.StageImplement, ProjectStages: true, PlanState: &taskops.PlanStateMove{To: core.PlanCompleted, Cause: core.PlanCausePlanApproved}}); err != nil {
					t.Fatal(err)
				}
				requirePlanStateMoveCount(t, st, ctx, taskID, 3)
				return
			}
			if _, err = plane.Perform(ctx, taskID, taskops.Command{Kind: core.TaskInterventionRedirect, NextStage: core.StageSpec, ProjectStages: true, PlanState: &taskops.PlanStateMove{To: core.PlanNeeded, Cause: core.PlanCausePlanRedirected}}); err != nil {
				t.Fatal(err)
			}
			requirePlanStateMove(t, st, ctx, taskID, core.PlanAwaitingApproval, core.PlanNeeded, core.PlanCausePlanRedirected)
			redirected, err := st.GetTask(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			if redirected.PlanState != core.PlanNeeded || redirected.NextStage != core.StageSpec {
				t.Fatalf("redirected task=%+v", redirected)
			}
		})
	}

	// A refused lifecycle command records no plan-state move: the plan state
	// and its event commit with the transition or not at all
	// (component-plan-state, State and failure).
	t.Run("refused command records no move", func(t *testing.T) {
		now := time.Now().UTC()
		taskID := core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspaceID(ctx), Repo: "app", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		// running has no intervention.approve_spec edge.
		if _, err := taskops.New(st).Perform(ctx, taskID, taskops.Command{Kind: core.TaskInterventionApproveSpec, NextStage: core.StageImplement, ProjectStages: true, PlanState: &taskops.PlanStateMove{To: core.PlanCompleted, Cause: core.PlanCausePlanApproved}}); err == nil {
			t.Fatal("illegal transition unexpectedly succeeded")
		}
		task, err := st.GetTask(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		events, err := st.ListEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.PlanState != core.PlanNeeded {
			t.Fatalf("plan state=%s after a refused command", task.PlanState)
		}
		requireNoPlanStateMove(t, events)
	})

	rejections := []struct {
		name      string
		stage     core.Stage
		planState core.PlanState
		approved  bool
		claimed   bool
		sessionID string
		rationale string
	}{
		{name: "blank rationale", stage: core.StageImplement, planState: core.PlanAwaitingApproval, approved: true, claimed: true, sessionID: "session-a", rationale: " \t "},
		{name: "wrong stage", stage: core.StageReview, planState: core.PlanAwaitingApproval, approved: true, claimed: true, sessionID: "session-a", rationale: "wrong plan"},
		{name: "missing approved plan", stage: core.StageImplement, planState: core.PlanCompleted, claimed: true, sessionID: "session-a", rationale: "wrong plan"},
		{name: "planning needed", stage: core.StageImplement, planState: core.PlanNeeded, claimed: true, sessionID: "session-a", rationale: "wrong plan"},
		{name: "plan awaiting approval", stage: core.StageImplement, planState: core.PlanAwaitingApproval, claimed: true, sessionID: "session-a", rationale: "wrong plan"},
		{name: "stale session", stage: core.StageImplement, planState: core.PlanAwaitingApproval, approved: true, claimed: true, sessionID: "session-stale", rationale: "wrong plan"},
		{name: "unclaimed", stage: core.StageImplement, planState: core.PlanAwaitingApproval, approved: true, rationale: "wrong plan"},
		{name: "no plan required, blank rationale", stage: core.StageImplement, planState: core.PlanNotRequired, claimed: true, sessionID: "session-a", rationale: "  "},
	}
	for _, test := range rejections {
		t.Run(test.name+" preserves claim and projections", func(t *testing.T) {
			fixture := newPlanRevisionFixtureIn(t, st, ctx, test.stage, test.planState, test.approved, test.claimed)
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
	// An approved plan is planning completed; a fixture without one requires
	// no plan unless a case states otherwise.
	planState := core.PlanNotRequired
	if approved {
		planState = core.PlanAwaitingApproval
	}
	return newPlanRevisionFixtureIn(t, st, ctx, stage, planState, approved, claimed)
}

func newPlanRevisionFixtureIn(t *testing.T, st store.Store, ctx context.Context, stage core.Stage, planState core.PlanState, approved, claimed bool) planRevisionFixture {
	t.Helper()
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspaceID(ctx), Repo: "app", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: stage, PlanState: planState, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if approved {
		// ApproveSpecVersion moves the plan state to completed.
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
		claimedOrder, err := For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: fixture.sessionID, ClientToken: taskID + "-token", Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		fixture.order = claimedOrder
	}
	return fixture
}

func requestPlanRevision(ctx context.Context, st store.Store, taskID, orderID, workerID, sessionID, rationale string) (store.PlanRevisionRequestResult, error) {
	order, err := st.GetWorkOrder(ctx, orderID)
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
		claim := core.WorkOrderClaimIdentity{ClaimantID: order.ClaimantID, SessionID: sessionID}
		return st.RequestPlanRevisionCommand(ctx, lease, orderID, claim, rationale)
	})
}

func workspaceID(ctx context.Context) string {
	workspace, _ := store.WorkspaceFromContext(ctx)
	return workspace
}

// planStateMoves returns the task.plan_state_changed payloads recorded after
// the intake move.
func planStateMoves(t *testing.T, events []core.Event) []core.PlanStateChange {
	t.Helper()
	var moves []core.PlanStateChange
	for _, event := range events {
		if event.Kind != core.EventTaskPlanStateChanged {
			continue
		}
		var change core.PlanStateChange
		if err := json.Unmarshal(event.Payload, &change); err != nil {
			t.Fatalf("decode %s: %v", event.Kind, err)
		}
		if change.Cause == core.PlanCauseIntake {
			continue
		}
		moves = append(moves, change)
	}
	return moves
}

// requirePlanStateMove asserts the latest recorded plan-state move and the
// task's stored plan state.
func requirePlanStateMove(t *testing.T, st store.Store, ctx context.Context, taskID string, from, to core.PlanState, cause core.PlanStateCause) {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	moves := planStateMoves(t, events)
	if len(moves) == 0 {
		t.Fatalf("task %s recorded no plan-state move", taskID)
	}
	last := moves[len(moves)-1]
	if last.From == nil || *last.From != from || last.To != to || last.Cause != cause {
		t.Fatalf("latest plan-state move=%+v (from %v), want %s -> %s cause %s", last, last.From, from, to, cause)
	}
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.PlanState != to {
		t.Fatalf("stored plan state=%s, want %s", task.PlanState, to)
	}
}

func requirePlanStateMoveCount(t *testing.T, st store.Store, ctx context.Context, taskID string, want int) {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if moves := planStateMoves(t, events); len(moves) != want-1 {
		t.Fatalf("plan-state moves after intake=%+v, want %d", moves, want-1)
	}
}

func requireNoPlanStateMove(t *testing.T, events []core.Event) {
	t.Helper()
	if moves := planStateMoves(t, events); len(moves) != 0 {
		t.Fatalf("unexpected plan-state moves %+v", moves)
	}
}
