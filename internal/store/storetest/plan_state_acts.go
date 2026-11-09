package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// planStateActsPlan passes the content rules of a submitted plan.
const planStateActsPlan = "## Approach\nAttach a plan.\n\n## Done criteria\n- The plan waits for approval."

// runPlanStateActs proves on every backend that the /policy plan_state change
// and the attach-plan act commit their effects, events, order cancellation,
// lifecycle command, and replay record together; that each refusal records
// nothing; that a reused request ID replays or conflicts; and that earlier
// plan versions stay readable (component-plan-state invariants 3 to 5 and
// Verification; req-task-plan-state AC-3.1, AC-3.2).
func runPlanStateActs(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	commit := "0123456789abcdef0123456789abcdef01234567"

	newTask := func(t *testing.T, state core.TaskState, next core.Stage, planState core.PlanState) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Branch: "conveyor/task-" + id, PolicyVersion: 1,
			State: state, NextStage: next, PlanState: planState, CreatedAt: time.Now().UTC()}
		requireOK(t, st.CreateTask(ctx, task))
		return task
	}
	addOrder := func(t *testing.T, taskID string, stage core.Stage, claimed bool) core.WorkOrder {
		t.Helper()
		now := time.Now().UTC()
		id := taskID + "-" + string(stage) + "-" + core.NewTaskID()
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}))
		order := core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		requireOK(t, For(st).CreateWorkOrder(ctx, order))
		if claimed {
			claimedOrder, err := For(st).ClaimWorkOrder(ctx, id, core.WorkOrderClaim{SessionID: "session-" + id, ClientToken: "token-" + id, Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: time.Hour})
			requireOK(t, err)
			return claimedOrder
		}
		stored, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		return stored
	}
	setPlanState := func(taskID, requestID, reason string, to core.PlanState) (store.SetupChangeResult, error) {
		return taskops.ExecuteSetupChange(ctx, st, taskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
			return st.ChangeTaskPolicyCommand(ctx, lease, store.SetupChangeRequest{TaskID: taskID, RequestID: requestID, Reason: reason, Policy: &store.TaskPolicyChange{PlanState: &to}})
		})
	}
	attach := func(taskID, requestID, plan, base string) (store.AttachPlanResult, error) {
		return taskops.ExecuteSetupChange(ctx, st, taskID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
			return st.AttachTaskPlanCommand(ctx, lease, store.AttachPlanRequest{TaskID: taskID, RequestID: requestID, Reason: "operator plan", Plan: plan, BaseCommit: base})
		})
	}
	snapshot := func(t *testing.T, taskID string) (core.Task, int, []core.WorkOrder) {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		orders, err := st.ListTaskWorkOrders(ctx, taskID)
		requireOK(t, err)
		return task, len(events), orders
	}
	requireNothingRecorded := func(t *testing.T, taskID string, err error, before core.Task, beforeEvents int, beforeOrders []core.WorkOrder) {
		t.Helper()
		if !errors.Is(err, store.ErrSetupChangeConflict) {
			t.Fatalf("err = %v, want a setup change conflict", err)
		}
		after, afterEvents, afterOrders := snapshot(t, taskID)
		if after.State != before.State || after.NextStage != before.NextStage || after.PlanState != before.PlanState || after.GateKind != before.GateKind || afterEvents != beforeEvents {
			t.Fatalf("refusal recorded: task %+v -> %+v, events %d -> %d", before, after, beforeEvents, afterEvents)
		}
		for i := range beforeOrders {
			if afterOrders[i].State != beforeOrders[i].State || afterOrders[i].SessionID != beforeOrders[i].SessionID {
				t.Fatalf("refusal changed order %s: %+v -> %+v", beforeOrders[i].ID, beforeOrders[i], afterOrders[i])
			}
		}
	}
	eventKinds := func(t *testing.T, taskID string) map[string]int {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		counts := map[string]int{}
		for _, event := range events {
			counts[event.Kind]++
		}
		return counts
	}
	requireRerouted := func(t *testing.T, taskID string, command core.TaskCommand) {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		for _, event := range events {
			if event.Kind != "task.state_changed" {
				continue
			}
			var payload struct {
				Command core.TaskCommand `json:"command"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Command == command {
				return
			}
		}
		t.Fatalf("task %s recorded no %s transition", taskID, command)
	}
	requireCancelled := func(t *testing.T, orderID string) {
		t.Helper()
		order, err := st.GetWorkOrder(ctx, orderID)
		requireOK(t, err)
		if order.State != core.WorkOrderCancelled || !order.RetrySuppressed || order.RetrySuppressionReason != store.PlanStateChangedSuppressionReason ||
			order.SessionID != "" || order.ClaimantID != "" || !order.LeaseExpiresAt.IsZero() {
			t.Fatalf("order %s = %+v, want cancelled with retry suppressed for %s and no claim", orderID, order, store.PlanStateChangedSuppressionReason)
		}
	}

	t.Run("plan_state to not_required cancels a claimed plan order and reroutes to implement", func(t *testing.T) {
		task := newTask(t, core.TaskRunning, core.StageSpec, core.PlanNeeded)
		order := addOrder(t, task.ID, core.StageSpec, true)
		result, err := setPlanState(task.ID, core.NewTaskID(), "no plan needed", core.PlanNotRequired)
		requireOK(t, err)
		if result.Task.State != core.TaskQueued || result.Task.NextStage != core.StageImplement || result.Task.PlanState != core.PlanNotRequired ||
			!reflect.DeepEqual(result.CancelledWorkOrders, []string{order.ID}) || result.ReviewTransition != "plan_state" {
			t.Fatalf("result = %+v", result)
		}
		requireCancelled(t, order.ID)
		requirePlanStateMove(t, st, ctx, task.ID, core.PlanNeeded, core.PlanNotRequired, core.PlanCauseOperator)
		requireRerouted(t, task.ID, core.TaskPlanReroute)
		if kinds := eventKinds(t, task.ID); kinds["task.setup.changed"] != 1 || kinds["work_order.cancelled"] != 1 {
			t.Fatalf("event kinds = %v", kinds)
		}
		stored, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if stored.State != core.TaskQueued || stored.NextStage != core.StageImplement || stored.SetupContract.VerifyStage != task.SetupContract.VerifyStage {
			t.Fatalf("stored task = %+v", stored)
		}
	})

	t.Run("plan_state to needed cancels a queued implement order and reroutes to the plan stage", func(t *testing.T) {
		task := newTask(t, core.TaskRunning, core.StageImplement, core.PlanNotRequired)
		order := addOrder(t, task.ID, core.StageImplement, false)
		result, err := setPlanState(task.ID, core.NewTaskID(), "needs design", core.PlanNeeded)
		requireOK(t, err)
		if result.Task.NextStage != core.StageSpec || result.Task.State != core.TaskQueued || result.Task.PlanState != core.PlanNeeded {
			t.Fatalf("result task = %+v", result.Task)
		}
		requireCancelled(t, order.ID)
		requirePlanStateMove(t, st, ctx, task.ID, core.PlanNotRequired, core.PlanNeeded, core.PlanCauseOperator)
	})

	t.Run("plan_state before triage moves the state alone", func(t *testing.T) {
		task := newTask(t, core.TaskQueued, core.StageTriage, core.PlanNeeded)
		result, err := setPlanState(task.ID, core.NewTaskID(), "small change", core.PlanNotRequired)
		requireOK(t, err)
		if result.Task.State != core.TaskQueued || result.Task.NextStage != core.StageTriage || result.Task.PlanState != core.PlanNotRequired {
			t.Fatalf("result task = %+v", result.Task)
		}
		requirePlanStateMove(t, st, ctx, task.ID, core.PlanNeeded, core.PlanNotRequired, core.PlanCauseOperator)
		if kinds := eventKinds(t, task.ID); kinds["task.state_changed"] != 0 {
			t.Fatalf("a state-only move changed the task state: %v", kinds)
		}
	})

	gated := func(t *testing.T) core.Task {
		t.Helper()
		task := newTask(t, core.TaskRunning, core.StageSpec, core.PlanNeeded)
		_, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "submitted plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}), CreatedAt: time.Now().UTC()})
		requireOK(t, err)
		_, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true,
			PlanState: &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted}})
		requireOK(t, err)
		stored, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if stored.GateKind != core.GateKindPlanApproval {
			t.Fatalf("fixture gate = %q", stored.GateKind)
		}
		return stored
	}

	t.Run("plan_state at the plan approval gate reroutes and leaves the version unapproved", func(t *testing.T) {
		for to, next := range map[core.PlanState]core.Stage{core.PlanNotRequired: core.StageImplement, core.PlanNeeded: core.StageSpec} {
			task := gated(t)
			result, err := setPlanState(task.ID, core.NewTaskID(), "reroute at the gate", to)
			requireOK(t, err)
			if result.Task.State != core.TaskQueued || result.Task.NextStage != next || result.Task.GateKind != "" || result.Task.PlanState != to {
				t.Fatalf("to %s: result task = %+v", to, result.Task)
			}
			requirePlanStateMove(t, st, ctx, task.ID, core.PlanAwaitingApproval, to, core.PlanCauseOperator)
			spec, ok, err := st.GetSpecVersion(ctx, task.ID, 1)
			requireOK(t, err)
			if !ok || spec.Approved || spec.Content != "submitted plan" {
				t.Fatalf("plan version 1 = %+v ok=%t", spec, ok)
			}
		}
	})

	t.Run("plan_state refusals record nothing", func(t *testing.T) {
		claimedImplement := newTask(t, core.TaskRunning, core.StageImplement, core.PlanCompleted)
		addOrder(t, claimedImplement.ID, core.StageImplement, true)
		review := newTask(t, core.TaskQueued, core.StageReview, core.PlanNotRequired)
		current := newTask(t, core.TaskQueued, core.StageTriage, core.PlanNeeded)
		closed := newTask(t, core.TaskQueued, core.StageTriage, core.PlanNeeded)
		_, err := taskops.New(st).Perform(ctx, closed.ID, taskops.Command{Kind: core.TaskCancel})
		requireOK(t, err)
		for name, test := range map[string]struct {
			task core.Task
			to   core.PlanState
		}{
			"claimed implement order":  {claimedImplement, core.PlanNotRequired},
			"needed while review next": {review, core.PlanNeeded},
			"names the current state":  {current, core.PlanNeeded},
			"terminal task":            {closed, core.PlanNotRequired},
		} {
			before, beforeEvents, beforeOrders := snapshot(t, test.task.ID)
			_, err := setPlanState(test.task.ID, core.NewTaskID(), "refused "+name, test.to)
			requireNothingRecorded(t, test.task.ID, err, before, beforeEvents, beforeOrders)
		}
	})

	t.Run("plan_state request ID replays or conflicts", func(t *testing.T) {
		task := newTask(t, core.TaskQueued, core.StageTriage, core.PlanNeeded)
		requestID := core.NewTaskID()
		first, err := setPlanState(task.ID, requestID, "replay", core.PlanNotRequired)
		requireOK(t, err)
		_, events, _ := snapshot(t, task.ID)
		replayed, err := setPlanState(task.ID, requestID, "replay", core.PlanNotRequired)
		requireOK(t, err)
		if replayed.RequestID != first.RequestID || replayed.Task.PlanState != core.PlanNotRequired {
			t.Fatalf("replay = %+v, first = %+v", replayed, first)
		}
		if _, after, _ := snapshot(t, task.ID); after != events {
			t.Fatalf("replay appended events: %d -> %d", events, after)
		}
		if _, err = setPlanState(task.ID, requestID, "different reason", core.PlanNotRequired); !errors.Is(err, store.ErrSetupChangeConflict) {
			t.Fatalf("different input err = %v", err)
		}
		if _, err = attach(task.ID, requestID, planStateActsPlan, ""); !errors.Is(err, store.ErrSetupChangeConflict) {
			t.Fatalf("attach reusing a /policy request ID err = %v", err)
		}
	})

	t.Run("attach before triage records the version and waits for triage", func(t *testing.T) {
		task := newTask(t, core.TaskQueued, core.StageTriage, core.PlanNeeded)
		result, err := attach(task.ID, core.NewTaskID(), planStateActsPlan, commit)
		requireOK(t, err)
		if result.PlanVersion != 1 || result.Task.State != core.TaskQueued || result.Task.NextStage != core.StageTriage || result.Task.PlanState != core.PlanAwaitingApproval {
			t.Fatalf("result = %+v", result)
		}
		spec, ok, err := st.GetSpecVersion(ctx, task.ID, 1)
		requireOK(t, err)
		if !ok || spec.Origin != core.PlanOriginOperator || spec.BaseCommit == nil || *spec.BaseCommit != commit || spec.Approved {
			t.Fatalf("plan version = %+v ok=%t", spec, ok)
		}
		requirePlanStateMove(t, st, ctx, task.ID, core.PlanNeeded, core.PlanAwaitingApproval, core.PlanCausePlanAttached)
	})

	t.Run("attach after triage cancels the plan order and enters the gate with earlier versions readable", func(t *testing.T) {
		task := newTask(t, core.TaskRunning, core.StageSpec, core.PlanNeeded)
		_, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "earlier plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}), CreatedAt: time.Now().UTC()})
		requireOK(t, err)
		order := addOrder(t, task.ID, core.StageSpec, true)
		result, err := attach(task.ID, core.NewTaskID(), planStateActsPlan, "")
		requireOK(t, err)
		if result.PlanVersion != 2 || result.Task.State != core.TaskAwaiting || result.Task.GateKind != core.GateKindPlanApproval || result.Task.PlanState != core.PlanAwaitingApproval ||
			!reflect.DeepEqual(result.CancelledWorkOrders, []string{order.ID}) {
			t.Fatalf("result = %+v", result)
		}
		requireCancelled(t, order.ID)
		requireRerouted(t, task.ID, core.TaskGateSpec)
		requirePlanStateMove(t, st, ctx, task.ID, core.PlanNeeded, core.PlanAwaitingApproval, core.PlanCausePlanAttached)
		earlier, ok, err := st.GetSpecVersion(ctx, task.ID, 1)
		requireOK(t, err)
		if !ok || earlier.Content != "earlier plan" {
			t.Fatalf("plan version 1 = %+v ok=%t", earlier, ok)
		}
		latest, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
		requireOK(t, err)
		if !ok || latest.Version != 2 || latest.Origin != core.PlanOriginOperator || latest.BaseCommit != nil || latest.Approved {
			t.Fatalf("latest plan version = %+v ok=%t", latest, ok)
		}
	})

	t.Run("attach from a queued implement order enters the gate from queued", func(t *testing.T) {
		task := newTask(t, core.TaskQueued, core.StageImplement, core.PlanNotRequired)
		order := addOrder(t, task.ID, core.StageImplement, false)
		result, err := attach(task.ID, core.NewTaskID(), planStateActsPlan, "")
		requireOK(t, err)
		if result.Task.State != core.TaskAwaiting || result.Task.GateKind != core.GateKindPlanApproval || result.Task.RecoveryStage != core.StageImplement {
			t.Fatalf("result task = %+v", result.Task)
		}
		requireCancelled(t, order.ID)
		requirePlanStateMove(t, st, ctx, task.ID, core.PlanNotRequired, core.PlanAwaitingApproval, core.PlanCausePlanAttached)
	})

	t.Run("attach refusals record nothing", func(t *testing.T) {
		claimedImplement := newTask(t, core.TaskRunning, core.StageImplement, core.PlanNotRequired)
		addOrder(t, claimedImplement.ID, core.StageImplement, true)
		completed := newTask(t, core.TaskQueued, core.StageImplement, core.PlanCompleted)
		review := newTask(t, core.TaskQueued, core.StageReview, core.PlanNotRequired)
		awaiting := gated(t)
		for name, task := range map[string]core.Task{"claimed implement order": claimedImplement, "completed": completed, "review next": review, "awaiting approval": awaiting} {
			before, beforeEvents, beforeOrders := snapshot(t, task.ID)
			_, err := attach(task.ID, core.NewTaskID(), planStateActsPlan, "")
			if err == nil {
				t.Fatalf("%s: attach accepted", name)
			}
			requireNothingRecorded(t, task.ID, err, before, beforeEvents, beforeOrders)
		}
	})

	t.Run("attach request ID replays or conflicts", func(t *testing.T) {
		task := newTask(t, core.TaskQueued, core.StageTriage, core.PlanNotRequired)
		requestID := core.NewTaskID()
		first, err := attach(task.ID, requestID, planStateActsPlan, "")
		requireOK(t, err)
		_, events, _ := snapshot(t, task.ID)
		replayed, err := attach(task.ID, requestID, planStateActsPlan, "")
		requireOK(t, err)
		if replayed.PlanVersion != first.PlanVersion || replayed.RequestID != requestID {
			t.Fatalf("replay = %+v, first = %+v", replayed, first)
		}
		if _, after, _ := snapshot(t, task.ID); after != events {
			t.Fatalf("replay appended events: %d -> %d", events, after)
		}
		if latest, _, _ := st.GetLatestSpecVersion(ctx, task.ID); latest.Version != 1 {
			t.Fatalf("replay recorded another version: %+v", latest)
		}
		if _, err = attach(task.ID, requestID, planStateActsPlan+"\n- More.", ""); !errors.Is(err, store.ErrSetupChangeConflict) {
			t.Fatalf("different plan err = %v", err)
		}
		if _, err = setPlanState(task.ID, requestID, "operator plan", core.PlanNeeded); !errors.Is(err, store.ErrSetupChangeConflict) {
			t.Fatalf("/policy reusing an attach request ID err = %v", err)
		}
	})

	// A triage proceed command carries the step its stale snapshot chose; the
	// backend replaces it with the step the plan state it reads under the
	// task-operation lock selects, so a plan act committed while triage ran
	// decides the route (component-plan-state invariant 5; component-triage
	// invariant 29; req-task-plan-state AC-1.1, AC-3.2).
	t.Run("triage proceed follows the plan state committed during triage", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			initial core.PlanState
			act     func(t *testing.T, taskID string)
			state   core.TaskState
			next    core.Stage
			gate    core.GateKind
		}{
			{"attach on not_required", core.PlanNotRequired, func(t *testing.T, taskID string) {
				_, err := attach(taskID, core.NewTaskID(), planStateActsPlan, "")
				requireOK(t, err)
			}, core.TaskAwaiting, "", core.GateKindPlanApproval},
			{"policy to needed", core.PlanNotRequired, func(t *testing.T, taskID string) {
				_, err := setPlanState(taskID, core.NewTaskID(), "needs design", core.PlanNeeded)
				requireOK(t, err)
			}, core.TaskQueued, core.StageSpec, ""},
			{"policy to not_required", core.PlanNeeded, func(t *testing.T, taskID string) {
				_, err := setPlanState(taskID, core.NewTaskID(), "small change", core.PlanNotRequired)
				requireOK(t, err)
			}, core.TaskQueued, core.StageImplement, ""},
			{"no act on not_required", core.PlanNotRequired, nil, core.TaskQueued, core.StageImplement, ""},
			{"no act on needed", core.PlanNeeded, nil, core.TaskQueued, core.StageSpec, ""},
		} {
			task := newTask(t, core.TaskRunning, core.StageTriage, tc.initial)
			stale, staleNext, staleRecovery, err := core.TriageProceedStep(tc.initial)
			requireOK(t, err)
			if tc.act != nil {
				tc.act(t, task.ID)
			}
			proceeded, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: stale, NextStage: staleNext, RecoveryStage: staleRecovery, ProjectStages: true, TriageProceed: true})
			requireOK(t, err)
			stored, err := st.GetTask(ctx, task.ID)
			requireOK(t, err)
			for _, got := range []core.Task{proceeded.Task, stored} {
				if got.State != tc.state || got.GateKind != tc.gate || (tc.state == core.TaskQueued && got.NextStage != tc.next) {
					t.Fatalf("%s: task state=%s next=%s gate=%s, want %s next=%s gate=%s", tc.name, got.State, got.NextStage, got.GateKind, tc.state, tc.next, tc.gate)
				}
			}
			if tc.gate == core.GateKindPlanApproval && stored.RecoveryStage != core.StageImplement {
				t.Fatalf("%s: plan gate recovery stage=%s", tc.name, stored.RecoveryStage)
			}
			if orders, listErr := st.ListTaskWorkOrders(ctx, task.ID); listErr != nil || len(orders) != 0 {
				t.Fatalf("%s: proceed created orders %+v err=%v", tc.name, orders, listErr)
			}
		}
	})
}
