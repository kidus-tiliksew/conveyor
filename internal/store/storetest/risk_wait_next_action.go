package storetest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunRiskWaitNextActionConformance proves on every backend, from a real plan
// submission and lifecycle commands only, that the public next-action read of
// an enforcing task at plan approval names the agent and the plan version's
// risk order while that order is queued or claimed, asks no person for the
// approval, and returns to the operator's plan approval after the order
// completes or is cancelled. It also proves that a record-mode task keeps its
// plan approval gate, and that a record-mode risk order never displaces
// another queued or claimed order of the task (req-risk-based-review AC-1.6,
// AC-4.8, AC-11.3; feature-risk-based-review RR-15, RR-16).
func RunRiskWaitNextActionConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	repo := "conveyor"
	if x.Config != nil && len(x.Config.Repos) > 0 {
		repo = x.Config.Repos[0].Name
	}
	plane := taskops.New(st)
	planningTask := func(t *testing.T, mode riskpolicy.Mode) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Source: "api", Repo: repo, Title: id, Body: "Risk wait " + id, BaseBranch: "main",
			Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded, PolicyVersion: 1,
			CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
		requireOK(t, st.CreateTask(ctx, task))
		return task
	}
	// submit is the plan submission's own command: it records plan version 1
	// and takes the task to the plan approval gate, which creates the plan
	// version's risk order in the same act.
	submit := func(t *testing.T, taskID string) core.RiskOrder {
		t.Helper()
		outcome, err := plane.Perform(ctx, taskID, taskops.Command{
			Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true,
			PlanState:   &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted},
			PlanVersion: &core.SpecVersion{Content: planRiskPlan("", ""), Acceptance: []byte(`[]`), Decomposition: []byte(`[]`), Agent: "codex", Model: "planner"},
		})
		requireOK(t, err)
		if outcome.Task.State != core.TaskAwaiting || outcome.Task.GateKind != core.GateKindPlanApproval {
			t.Fatalf("submitted task=%s/%s", outcome.Task.State, outcome.Task.GateKind)
		}
		orders, err := st.ListTaskRiskOrders(ctx, taskID)
		requireOK(t, err)
		if len(orders) != 1 || orders[0].Subject.PlanVersion != 1 || orders[0].Order.State != core.WorkOrderQueued {
			t.Fatalf("plan risk orders=%+v, want one queued order for version 1", orders)
		}
		return orders[0]
	}
	read := func(t *testing.T, taskID string) nextaction.NextAction {
		t.Helper()
		na, err := store.ReadNextAction(ctx, st, taskID, store.NextActionOptions{
			Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true},
		})
		requireOK(t, err)
		return na
	}
	claim := func(t *testing.T, orderID, session string) {
		t.Helper()
		_, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{ClaimantID: "run:risk-wait", SessionID: session, ClientToken: session + "-token",
			Agent: "claude-code", Model: "risk-model", Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
	}
	requireRiskWait := func(t *testing.T, na nextaction.NextAction, kind, orderID string) {
		t.Helper()
		if na.Actor != nextaction.ActorAgent || na.Kind != kind || na.Judgment || na.Details.ReasonCode != nextaction.PlanRiskRating {
			t.Fatalf("next action %s/%s judgment=%t reason=%q, want the agent's %s plan risk wait", na.Actor, na.Kind, na.Judgment, na.Details.ReasonCode, kind)
		}
		if na.CurrentOrder == nil || na.CurrentOrder.ID != orderID || na.CurrentOrder.Stage != string(core.StageRisk) {
			t.Fatalf("current order=%+v, want risk order %s", na.CurrentOrder, orderID)
		}
		for _, choice := range na.Choices {
			if choice.ID == "approve" {
				t.Fatalf("the plan risk wait offered an approval: %+v", na.Choices)
			}
		}
	}
	requireGate := func(t *testing.T, na nextaction.NextAction) {
		t.Helper()
		approve := false
		for _, choice := range na.Choices {
			approve = approve || choice.ID == "approve"
		}
		if na.Actor != nextaction.ActorOperator || na.Kind != nextaction.KindPlanApproval || !na.Judgment || !approve {
			t.Fatalf("next action %s/%s judgment=%t choices=%+v, want the operator's plan approval", na.Actor, na.Kind, na.Judgment, na.Choices)
		}
	}

	t.Run("EnforceWaitsUntilTheRatingCompletes", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeEnforce)
		order := submit(t, task.ID)
		requireRiskWait(t, read(t, task.ID), nextaction.KindQueued, order.Order.ID)
		session := "risk-wait-complete"
		claim(t, order.Order.ID, session)
		requireRiskWait(t, read(t, task.ID), nextaction.KindWorking, order.Order.ID)
		_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
			return st.CompleteRiskOrderCommand(ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: order.Order.ID,
				Claim: core.WorkOrderClaimIdentity{ClaimantID: "run:risk-wait", SessionID: session}, Tier: riskpolicy.TierLow, Reason: "Rated from the plan."})
		})
		requireOK(t, err)
		requireGate(t, read(t, task.ID))
		// The returned gate is the one the route accepts.
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, 1))
	})

	t.Run("EnforceWaitEndsWhenTheOrderIsCancelled", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeEnforce)
		order := submit(t, task.ID)
		claim(t, order.Order.ID, "risk-wait-cancel")
		requireRiskWait(t, read(t, task.ID), nextaction.KindWorking, order.Order.ID)
		_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			cancelled, _, cancelErr := st.CancelRiskOrderCommand(ctx, lease, store.RiskOrderCancelRequest{WorkOrderID: order.Order.ID, Reason: "operator ended the wait"})
			return cancelled, cancelErr
		})
		requireOK(t, err)
		requireGate(t, read(t, task.ID))
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, 1))
	})

	t.Run("RecordKeepsThePlanApprovalGate", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeRecord)
		order := submit(t, task.ID)
		requireGate(t, read(t, task.ID))
		claim(t, order.Order.ID, "risk-wait-record-gate")
		requireGate(t, read(t, task.ID))
	})

	// approvedRecordTask submits and approves a record-mode plan, which keeps
	// the plan risk order queued, and creates the implementation order.
	approvedRecordTask := func(t *testing.T) (core.RiskOrder, core.WorkOrder) {
		t.Helper()
		task := planningTask(t, riskpolicy.ModeRecord)
		order := submit(t, task.ID)
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, 1))
		_, err := plane.Perform(ctx, task.ID, taskops.Command{Kind: core.TaskInterventionApproveSpec, NextStage: core.StageImplement, ProjectStages: true,
			PlanState: &taskops.PlanStateMove{To: core.PlanCompleted, Cause: core.PlanCausePlanApproved}})
		requireOK(t, err)
		implementID := task.ID + "-implement-1"
		created, err := CreateStageWorkOrder(ctx, st, core.Job{ID: implementID, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending},
			core.WorkOrder{ID: implementID, TaskID: task.ID, JobID: implementID, Stage: core.StageImplement})
		requireOK(t, err)
		if !created {
			t.Fatalf("implementation order %s was not created", implementID)
		}
		implement, err := st.GetWorkOrder(ctx, implementID)
		requireOK(t, err)
		if risk, err := st.GetWorkOrder(ctx, order.Order.ID); err != nil || risk.State != core.WorkOrderQueued {
			t.Fatalf("record approval ended the risk order: %+v err=%v", risk, err)
		}
		return order, implement
	}
	requireNames := func(t *testing.T, na nextaction.NextAction, kind, orderID string) {
		t.Helper()
		if na.Actor != nextaction.ActorAgent || na.Kind != kind || na.CurrentOrder == nil || na.CurrentOrder.ID != orderID {
			t.Fatalf("next action %s/%s order %+v, want the agent's %s naming %s", na.Actor, na.Kind, na.CurrentOrder, kind, orderID)
		}
	}

	t.Run("RecordClaimedRiskOrderBesideQueuedWork", func(t *testing.T) {
		risk, implement := approvedRecordTask(t)
		claim(t, risk.Order.ID, "risk-wait-record-claimed")
		requireNames(t, read(t, implement.TaskID), nextaction.KindQueued, implement.ID)
	})

	t.Run("RecordQueuedRiskOrderBesideClaimedWork", func(t *testing.T) {
		risk, implement := approvedRecordTask(t)
		requireNames(t, read(t, implement.TaskID), nextaction.KindQueued, implement.ID)
		claim(t, implement.ID, "risk-wait-record-implement")
		requireNames(t, read(t, implement.TaskID), nextaction.KindWorking, implement.ID)
		if current, err := st.GetWorkOrder(ctx, risk.Order.ID); err != nil || current.State != core.WorkOrderQueued {
			t.Fatalf("risk order=%+v err=%v, want it still queued", current, err)
		}
	})

	headRisk := func(t *testing.T, taskID, head string) core.WorkOrder {
		t.Helper()
		order, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			created, made, createErr := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: taskID, Subject: core.RiskOrderSubject{HeadSHA: head, BaseSHA: "base-" + head}})
			if createErr == nil && !made {
				t.Fatalf("no risk order was created for head %s", head)
			}
			return created, createErr
		})
		requireOK(t, err)
		return order
	}
	requireLatest := func(t *testing.T, taskID, orderID string) {
		t.Helper()
		orders, err := st.ListTaskWorkOrders(ctx, taskID)
		requireOK(t, err)
		if len(orders) == 0 || orders[len(orders)-1].ID != orderID {
			t.Fatalf("task orders=%+v, want %s newest", orders, orderID)
		}
	}
	requireCompactParity := func(t *testing.T, taskID string, full nextaction.NextAction) {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		compact, err := store.ReadNextActions(ctx, st, []core.Task{task}, store.NextActionOptions{
			Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true}, CompactOnly: true,
		})
		requireOK(t, err)
		if got, want := nextaction.CompactFrom(compact[taskID]), nextaction.CompactFrom(full); !reflect.DeepEqual(got, want) {
			t.Fatalf("compact=%+v, full=%+v", got, want)
		}
	}

	// An ordinary order released at a real operator checkpoint, followed by
	// a newer record-mode risk order: the agent question still names the
	// ordinary order, the compact read agrees, and the requirement page
	// still offers the task as paused at a checkpoint.
	t.Run("RecordRiskOrderNeverHidesACheckpoint", func(t *testing.T) {
		_, implement := approvedRecordTask(t)
		session := "risk-wait-checkpoint"
		claim(t, implement.ID, session)
		request := "Confirm which plan step governs the wait."
		_, err := ReleaseWorkerClaim(ctx, st, implement.ID, "run:risk-wait", core.WorkOrderRelease{
			SessionID: session, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
			Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: request},
		})
		requireOK(t, err)
		risk := headRisk(t, implement.TaskID, "checkpoint-head")
		requireLatest(t, implement.TaskID, risk.ID)
		na := read(t, implement.TaskID)
		if na.Kind != nextaction.KindAgentQuestion || na.Actor != nextaction.ActorOperator || na.CurrentOrder == nil || na.CurrentOrder.ID != implement.ID || na.Details.Checkpoint != request {
			t.Fatalf("next action %s/%s order %+v checkpoint %q, want the agent question on %s", na.Actor, na.Kind, na.CurrentOrder, na.Details.Checkpoint, implement.ID)
		}
		requireCompactParity(t, implement.TaskID, na)
		candidates, err := st.ListCheckpointContextCandidates(ctx, "req-risk-wait-unattached")
		requireOK(t, err)
		found := false
		for _, candidate := range candidates {
			found = found || candidate.ID == implement.TaskID
		}
		if !found {
			t.Fatalf("checkpoint candidates=%+v, want task %s", candidates, implement.TaskID)
		}
	})

	// A verify order sealed at its verification checkpoint, followed by a
	// newer record-mode risk order: the disposition and agent-question rows
	// read the verify order, and the compact read states the same row as the
	// full read. With an empty disposition set the row is the agent
	// question; with a usable set it is the disposition.
	t.Run("RecordRiskOrderNeverHidesASealedVerificationCheckpoint", func(t *testing.T) {
		scope := []core.VerificationRevision{{Repository: "conveyor", RemoteIdentity: "https://example.test/conveyor", SHA: strings.Repeat("a", 40)}}
		record := func(task *core.Task) {
			task.SetupContract.Risk = &riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
		}
		t.Run("EmptySet", func(t *testing.T) {
			v := newVerificationFixtureTask(t, x, scope, record, true)
			v.subject = recoveryFixtureRegister(t, &v, recoveryFixtureObligation("risk-wait-replay-"+v.access.TaskID, "safe_to_replay", ""))
			v.start(t, "risk-wait-replay")
			v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "blocked", Explanation: "Fixture account locked"}})
			v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: verificationFixtureCoverage(v.snapshot(t)), Feedback: "Fixture account locked", RequiredAction: "Unlock the fixture account"}})
			risk := headRisk(t, v.access.TaskID, "sealed-empty-head")
			requireLatest(t, v.access.TaskID, risk.ID)
			na := read(t, v.access.TaskID)
			if na.Kind != nextaction.KindAgentQuestion || na.CurrentOrder == nil || na.CurrentOrder.ID != v.access.WorkOrderID || na.Details.Checkpoint == "" {
				t.Fatalf("empty set = %s on %+v checkpoint %q, want the agent question on %s", na.Kind, na.CurrentOrder, na.Details.Checkpoint, v.access.WorkOrderID)
			}
			requireCompactParity(t, v.access.TaskID, na)
		})
		t.Run("UsableSet", func(t *testing.T) {
			v := newVerificationFixtureTask(t, x, scope, record, true)
			for i, name := range []string{"login", "checkout"} {
				subject := recoveryFixtureRegister(t, &v, recoveryFixtureObligation("risk-wait-"+name+"-"+v.access.TaskID, "operator_action_required", ""))
				v.runID = v.apply(t, recoveryFixtureStart(fmt.Sprintf("risk-wait-%d", i), "", subject)).ID
				v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "blocked", Explanation: "fixture environment unavailable"}})
			}
			v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: verificationFixtureCoverage(v.snapshot(t)), Feedback: "Two exercises need an operator", RequiredAction: "Restore the fixtures"}})
			risk := headRisk(t, v.access.TaskID, "sealed-usable-head")
			requireLatest(t, v.access.TaskID, risk.ID)
			na := read(t, v.access.TaskID)
			if na.Kind != nextaction.KindVerificationDisposition || na.CurrentOrder == nil || na.CurrentOrder.ID != v.access.WorkOrderID || len(na.Details.Dispositions) != 2 {
				t.Fatalf("usable set = %s on %+v dispositions %+v, want the disposition on %s", na.Kind, na.CurrentOrder, na.Details.Dispositions, v.access.WorkOrderID)
			}
			requireCompactParity(t, v.access.TaskID, na)
		})
	})
}
