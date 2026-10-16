package storetest

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TaskFlowScenario is one command-built task history the flow read must fold
// identically on every backend (feature-operator-next-action NA-7).
type TaskFlowScenario struct {
	Name   string
	TaskID string
	// Check asserts scenario-specific flow facts.
	Check func(*testing.T, store.TaskFlow)
}

// SeedTaskFlowScenarios drives the shared audit histories through real
// commands and returns them for the store and HTTP flow suites.
func SeedTaskFlowScenarios(t *testing.T, st store.Backend, ctx context.Context, workspace string) []TaskFlowScenario {
	t.Helper()
	plane := taskops.New(st)
	operator := store.WithActor(ctx, store.Actor{ID: "operator-flow", Role: core.ActorUser})
	var scenarios []TaskFlowScenario
	add := func(name, id string, check func(*testing.T, store.TaskFlow)) {
		scenarios = append(scenarios, TaskFlowScenario{Name: name, TaskID: id, Check: check})
	}

	// Plan approval: a claimed spec order, then the spec gate.
	planID := flowTask(t, st, ctx, workspace, core.Task{SpecApproval: true, MergeApproval: true})
	spec := flowOrder(t, st, ctx, planID, core.StageSpec, "1", time.Hour)
	flowClaim(t, st, ctx, spec, "plan", time.Hour)
	_, err := plane.Perform(ctx, planID, taskops.Command{Kind: core.TaskGateSpec})
	requireOK(t, err)
	add("plan approval", planID, func(t *testing.T, flow store.TaskFlow) {
		if flow.Raw.GateKind != string(core.GateKindPlanApproval) || flow.Raw.State != string(core.TaskAwaiting) {
			t.Fatalf("raw = %+v", flow.Raw)
		}
		stage := taskFlowStage(t, flow, core.StageSpec)
		if stage.Status != store.FlowNeedsOperator || stage.Gate == nil || *stage.Gate != "plan_approval" {
			t.Fatalf("spec = %+v, want needs_operator behind plan_approval", stage)
		}
		if len(stage.Attempts) != 1 || stage.Attempts[0].AttemptID == "" || stage.Attempts[0].StartedAt == nil || stage.Attempts[0].Auto {
			t.Fatalf("spec attempts = %+v", stage.Attempts)
		}
		// The next action is the plan gate's own decision, so the gate needs
		// the operator; the merge gate ahead is not reached yet.
		requireFlowGates(t, flow, store.FlowNeedsOperator, store.FlowUpcoming)
		if stage.Label != "Plan" || stage.GateLabel != "Plan approval" || stage.GateStatusLabel != "Your approval" || stage.GateStatusText != "Plan approval is waiting for your decision." || !stage.Traversed || !stage.GateTraversed {
			t.Fatalf("spec node = %+v", stage)
		}
		if flow.CurrentStage == nil || *flow.CurrentStage != core.StageSpec {
			t.Fatalf("current_stage = %v, want spec", flow.CurrentStage)
		}
	})

	// A plan revision holds the plan gate from a later stage: the gate needs
	// the operator, and the stages already passed stay done.
	revisionID := flowTask(t, st, ctx, workspace, core.Task{SpecApproval: true, MergeApproval: true, NextStage: core.StageImplement})
	_, err = plane.Perform(ctx, revisionID, taskops.Command{Kind: core.TaskGatePlanRevision})
	requireOK(t, err)
	add("plan revision", revisionID, func(t *testing.T, flow store.TaskFlow) {
		if flow.NextAction.Actor != nextaction.ActorOperator || flow.NextAction.Kind != nextaction.KindPlanRevision {
			t.Fatalf("next action = %s/%s, want operator plan_revision", flow.NextAction.Actor, flow.NextAction.Kind)
		}
		requireFlowGates(t, flow, store.FlowNeedsOperator, store.FlowUpcoming)
	})

	// The merge gate: only the merge gate needs the operator, and the plan
	// gate the task passed reads done.
	mergeGateID := flowTask(t, st, ctx, workspace, core.Task{SpecApproval: true, MergeApproval: true, NextStage: core.StageReview})
	_, err = plane.Perform(ctx, mergeGateID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
	requireOK(t, err)
	add("merge approval", mergeGateID, func(t *testing.T, flow store.TaskFlow) {
		if flow.NextAction.Actor != nextaction.ActorOperator || flow.NextAction.Kind != nextaction.KindMergeApproval {
			t.Fatalf("next action = %s/%s, want operator merge_approval", flow.NextAction.Actor, flow.NextAction.Kind)
		}
		requireFlowGates(t, flow, store.FlowDone, store.FlowNeedsOperator)
		merge := taskFlowStage(t, flow, core.StageMerge)
		if merge.GateLabel != "Merge approval" || merge.GateStatusLabel != "Your approval" || !merge.GateTraversed {
			t.Fatalf("merge node = %+v", merge)
		}
	})

	// A context proposal at Plan stops the task at the stage, not at the plan
	// gate: the Plan row needs the operator and the gate stays upcoming
	// (req-operator-next-action AC-7.5).
	design, designInitial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "flow-context-" + core.NewTaskID(), Title: "Flow context", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("flow"), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designInitial.Version)
	requireOK(t, err)
	contextID := flowTask(t, st, ctx, workspace, core.Task{SpecApproval: true, MergeApproval: true, NextStage: core.StageSpec})
	_, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: contextID, TargetKind: core.TaskContextProposalSystemDesign, TargetID: design.ID, Source: core.TaskContextProposalTriage, Justification: "Triage suggests this design."})
	requireOK(t, err)
	add("context proposal at plan", contextID, func(t *testing.T, flow store.TaskFlow) {
		if flow.NextAction.Actor != nextaction.ActorOperator || flow.NextAction.Kind != nextaction.KindContextProposal {
			t.Fatalf("next action = %s/%s, want operator context_proposal", flow.NextAction.Actor, flow.NextAction.Kind)
		}
		if got := taskFlowStage(t, flow, core.StageSpec).Status; got != store.FlowNeedsOperator {
			t.Fatalf("spec status = %s, want needs_operator for the stop at Plan", got)
		}
		requireFlowGates(t, flow, store.FlowUpcoming, store.FlowUpcoming)
	})

	// Claimed implement order: in progress with its attempt folded from the ledger.
	claimedID := flowTask(t, st, ctx, workspace, core.Task{NextStage: core.StageImplement})
	implement := flowOrder(t, st, ctx, claimedID, core.StageImplement, "1", time.Hour)
	claimed := flowClaim(t, st, ctx, implement, "claimed", time.Hour)
	add("claimed", claimedID, func(t *testing.T, flow store.TaskFlow) {
		stage := taskFlowStage(t, flow, core.StageImplement)
		if stage.Status != store.FlowInProgress || stage.CurrentAttempt == nil {
			t.Fatalf("implement = %+v, want in_progress with an attempt", stage)
		}
		view := stage.CurrentAttempt
		if view.AttemptID != claimed.AttemptID || view.SessionID != claimed.SessionID || view.ClaimedBy != claimed.ClaimantID || view.ClaimedAt == nil {
			t.Fatalf("current_attempt = %+v, order = %+v", view, claimed)
		}
		// Elapsed time is the fold's own arithmetic over the served instant.
		if want := max(0, int64(flow.AsOf.Sub(*view.ClaimedAt).Seconds())); view.ElapsedSeconds != want {
			t.Fatalf("elapsed_seconds = %d, want %d from claimed_at %s to as_of %s", view.ElapsedSeconds, want, view.ClaimedAt, flow.AsOf)
		}
		if flow.CurrentStage == nil || *flow.CurrentStage != core.StageImplement {
			t.Fatalf("current_stage = %v, want implement", flow.CurrentStage)
		}
		if got := flow.NextAction.Attempt; flow.NextAction.CurrentOrder != nil && flow.NextAction.CurrentOrder.ID == implement && got != nil {
			if got.AttemptID != view.AttemptID || got.SessionID != view.SessionID || got.ClaimedBy != view.ClaimedBy || !sameFlowTime(got.ClaimedAt, view.ClaimedAt) {
				t.Fatalf("ledger attempt %+v disagrees with next action attempt %+v", view, got)
			}
		}
	})

	// Two review bounces, a human redirect, a bounce at the redirect's
	// instant, then one more bounce.
	bounceID := createRunningTask(t, st, ctx, workspace)
	accumulateAgentBounces(t, plane, st, ctx, bounceID, 2, 5)
	add("two bounces", bounceID, nil)
	bounceCount := flowBounceCount(t, st, ctx, bounceID)
	if bounceCount != 2 {
		t.Fatalf("store bounce window = %d, want 2", bounceCount)
	}
	redirectID := createRunningTask(t, st, ctx, workspace)
	accumulateAgentBounces(t, plane, st, ctx, redirectID, 2, 5)
	requireOK(t, st.CreateIntervention(operator, core.Intervention{TaskID: redirectID, Action: core.InterventionRedirect, ReasonCode: "operator_redirect", ActorRole: core.ActorHuman}))
	time.Sleep(5 * time.Millisecond) // later commands record strictly after the redirect
	_, err = plane.Perform(ctx, redirectID, taskops.Command{Kind: core.TaskDispatchStart})
	requireOK(t, err)
	jobs, orders := reviewRound(redirectID, 3)
	requireOK(t, CreateReviewRound(ctx, st, redirectID, jobs, orders))
	for i, order := range orders {
		requireOK(t, plane.AcceptReviewDecision(ctx, core.ReviewDecision{
			TaskID: redirectID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 3, ReviewSeat: i + 1,
			Verdict: "changes_requested", ReasonCode: "fix", Summary: "bounce", Feedback: "again", MaxBounces: 5,
		}))
	}
	add("redirect window", redirectID, nil)

	// Policy switches frozen at intake, and a verify switch turned back on.
	offID := flowTask(t, st, ctx, workspace, core.Task{SpecApproval: false, MergeApproval: false})
	add("policy off", offID, func(t *testing.T, flow store.TaskFlow) {
		spec, verify, merge := taskFlowStage(t, flow, core.StageSpec), taskFlowStage(t, flow, core.StageVerify), taskFlowStage(t, flow, core.StageMerge)
		if spec.Status != store.FlowOff || spec.Gate != nil || verify.Status != store.FlowOff || merge.Gate != nil || merge.Status == store.FlowOff {
			t.Fatalf("spec=%+v verify=%+v merge=%+v", spec, verify, merge)
		}
		requireFlowGates(t, flow, store.FlowOff, store.FlowOff)
		if spec.GateLabel != "Plan approval" || spec.GateStatusLabel != "Off for this task" || spec.Traversed || spec.GateTraversed {
			t.Fatalf("spec off node = %+v", spec)
		}
	})
	verifyID := flowTask(t, st, ctx, workspace, core.Task{SpecApproval: true, MergeApproval: true})
	enabled := true
	_, err = taskops.ExecuteSetupChange(ctx, st, verifyID, func(l taskops.TaskLease) (store.SetupChangeResult, error) {
		return st.ChangeTaskPolicyCommand(ctx, l, store.SetupChangeRequest{TaskID: verifyID, RequestID: "flow-verify-" + verifyID, Reason: "enable verification", Policy: &store.TaskPolicyChange{VerifyStage: &enabled}})
	})
	requireOK(t, err)
	add("verify enabled", verifyID, func(t *testing.T, flow store.TaskFlow) {
		if got := taskFlowStage(t, flow, core.StageVerify).Status; got == store.FlowOff {
			t.Fatalf("verify stays off after task.setup.changed enabled it")
		}
	})

	// Two spec orders: the newer one supersedes and retires the older, then
	// times out unclaimed and the clock redispatches it automatically.
	staleID := flowTask(t, st, ctx, workspace, core.Task{SpecApproval: true, MergeApproval: true, NextStage: core.StageSpec})
	older := flowOrder(t, st, ctx, staleID, core.StageSpec, "1", flowShort)
	newer := flowOrder(t, st, ctx, staleID, core.StageSpec, "2", flowShort)
	flowRecover(t, "stale redispatch", st, ctx, newer)
	add("stale redispatch", staleID, func(t *testing.T, flow store.TaskFlow) {
		attempts := taskFlowStage(t, flow, core.StageSpec).Attempts
		var olderRows, newerRows []store.FlowAttempt
		for _, attempt := range attempts {
			switch attempt.OrderID {
			case older:
				olderRows = append(olderRows, attempt)
			case newer:
				newerRows = append(newerRows, attempt)
			}
		}
		if len(olderRows) == 0 || len(newerRows) < 2 {
			t.Fatalf("attempts = %+v", attempts)
		}
		for _, row := range olderRows {
			if !row.Superseded {
				t.Fatalf("retired sibling entry not superseded: %+v", row)
			}
		}
		// The first dispatch is not automatic; the system replacement is.
		if olderRows[0].Auto || !newerRows[0].Auto || newerRows[0].EndedAt == nil || newerRows[0].Outcome != "stale" {
			t.Fatalf("dispatch entries = %+v %+v", olderRows[0], newerRows[0])
		}
		last := newerRows[len(newerRows)-1]
		if !last.Auto || last.Superseded || last.EndedAt != nil {
			t.Fatalf("automatic redispatch entry = %+v", last)
		}
		if len(flow.Raw.OrderIDs) != 2 || flow.Raw.OrderIDs[0] != older || flow.Raw.OrderIDs[1] != newer {
			t.Fatalf("order_ids = %v", flow.Raw.OrderIDs)
		}
	})

	// A claim times out and the clock recovers it automatically.
	retryID := flowTask(t, st, ctx, workspace, core.Task{NextStage: core.StageImplement})
	retryOrder := flowOrder(t, st, ctx, retryID, core.StageImplement, "1", time.Hour)
	flowClaim(t, st, ctx, retryOrder, "retry", flowShort)
	flowRecover(t, "automatic recovery", st, ctx, retryOrder)
	add("automatic recovery", retryID, func(t *testing.T, flow store.TaskFlow) {
		attempts := taskFlowStage(t, flow, core.StageImplement).Attempts
		if len(attempts) != 2 || attempts[0].Outcome != "timed_out" || attempts[0].Auto || !attempts[1].Auto || attempts[1].EndedAt != nil {
			t.Fatalf("attempts = %+v", attempts)
		}
	})

	// The same recovery, but the order is read only after its execution
	// deadline has passed, so the read projects it as timed_out
	// (component-work-orders v42). flowExpire must still wait on the
	// execution deadline, not the hour-long queue deadline. DEC-38: this
	// scenario extends the conformance flow and replaces none.
	lateID := flowTask(t, st, ctx, workspace, core.Task{NextStage: core.StageImplement})
	lateOrder := flowOrder(t, st, ctx, lateID, core.StageImplement, "1", time.Hour)
	flowClaim(t, st, ctx, lateOrder, "late", flowShort)
	time.Sleep(2 * flowShort)
	flowRecover(t, "late expiry read", st, ctx, lateOrder)
	add("late expiry read", lateID, func(t *testing.T, flow store.TaskFlow) {
		attempts := taskFlowStage(t, flow, core.StageImplement).Attempts
		if len(attempts) != 2 || attempts[0].Outcome != "timed_out" || attempts[0].Auto || !attempts[1].Auto || attempts[1].EndedAt != nil {
			t.Fatalf("attempts = %+v", attempts)
		}
	})

	// A claim times out and its automatic recovery is scheduled in the future:
	// the factory is retrying (NA-7 side.retrying).
	pendingID := flowTask(t, st, ctx, workspace, core.Task{NextStage: core.StageImplement})
	pendingOrder := flowOrder(t, st, ctx, pendingID, core.StageImplement, "1", time.Hour)
	flowClaim(t, st, ctx, pendingOrder, "pending", flowShort)
	pending := flowExpire(t, "retrying", st, ctx, pendingOrder)
	if !pending.NextAutomaticRecoveryAt.After(time.Now().UTC()) {
		t.Fatalf("order %s automatic recovery not scheduled ahead: %+v", pendingOrder, pending)
	}
	add("retrying", pendingID, func(t *testing.T, flow store.TaskFlow) {
		action := flow.NextAction
		if action.Actor != nextaction.ActorFactory || action.Kind != nextaction.KindRetrying || action.CurrentOrder == nil || action.CurrentOrder.ID != pendingOrder {
			t.Fatalf("next action = %s/%s order %+v", action.Actor, action.Kind, action.CurrentOrder)
		}
		bound := config.DefaultRecoveryPolicy().AutomaticRecoveries
		if got := flow.Side.Retrying; got == nil || got.Attempt != 1 || got.Of == nil || *got.Of != bound {
			t.Fatalf("side.retrying = %+v, want 1 of %d", got, bound)
		}
		if got, want := flow.Side.Labels[store.FlowSideRetrying], "Auto-retrying · 1 of "+strconv.Itoa(bound); got != want {
			t.Fatalf("side.labels = %v, want retrying %q", flow.Side.Labels, want)
		}
	})

	// An operator recovers a timed-out order by hand.
	recoverID := flowTask(t, st, ctx, workspace, core.Task{NextStage: core.StageImplement})
	recoverOrder := flowOrder(t, st, ctx, recoverID, core.StageImplement, "1", time.Hour)
	flowClaim(t, st, ctx, recoverOrder, "recover", flowShort)
	flowExpire(t, "operator recover", st, ctx, recoverOrder)
	_, err = RecoverWorkOrder(operator, st, recoverOrder, "flow-recover-"+recoverID, time.Hour)
	requireOK(t, err)
	add("operator recover", recoverID, func(t *testing.T, flow store.TaskFlow) {
		attempts := taskFlowStage(t, flow, core.StageImplement).Attempts
		if len(attempts) != 2 || attempts[0].Outcome != "timed_out" || attempts[1].Auto || attempts[1].EndedAt != nil {
			t.Fatalf("attempts = %+v", attempts)
		}
	})

	// One dependency stays open and one is removed.
	blockedID := flowTask(t, st, ctx, workspace, core.Task{NextStage: core.StageImplement})
	flowOrder(t, st, ctx, blockedID, core.StageImplement, "1", time.Hour)
	openDep := flowTask(t, st, ctx, workspace, core.Task{State: core.TaskRunning})
	removedDep := flowTask(t, st, ctx, workspace, core.Task{State: core.TaskRunning})
	for _, dep := range []string{openDep, removedDep} {
		_, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: blockedID, DependsOnTaskID: dep, Reason: "flow", RequestID: "flow-add-" + dep})
		requireOK(t, err)
	}
	_, err = st.RemoveTaskDependency(ctx, store.DependencyRemovalRequest{TaskID: blockedID, DependsOnTaskID: removedDep, Reason: "flow", RequestID: "flow-remove-" + removedDep})
	requireOK(t, err)
	add("dependency", blockedID, func(t *testing.T, flow store.TaskFlow) {
		// blocked_by is the open edges the next action names, so it never
		// lists the removed edge and never outruns the statement.
		named := map[string]bool{}
		for _, ref := range flow.NextAction.BlockedBy {
			named[ref.TaskID] = true
		}
		for _, ref := range flow.Side.BlockedBy {
			if ref.TaskID != openDep || !named[ref.TaskID] {
				t.Fatalf("blocked_by = %+v, next action %s blocked_by %+v", flow.Side.BlockedBy, flow.NextAction.Kind, flow.NextAction.BlockedBy)
			}
		}
		if named[openDep] && len(flow.Side.BlockedBy) != 1 {
			t.Fatalf("blocked_by = %+v, want the open edge the next action names", flow.Side.BlockedBy)
		}
		if _, labelled := flow.Side.Labels[store.FlowSideBlockedBy]; labelled != (len(flow.Side.BlockedBy) > 0) {
			t.Fatalf("side.labels = %v with blocked_by %+v", flow.Side.Labels, flow.Side.BlockedBy)
		}
	})

	parkedID := createRunningTask(t, st, ctx, workspace)
	_, err = plane.Perform(ctx, parkedID, taskops.Command{Kind: core.TaskTriagePark})
	requireOK(t, err)
	add("parked", parkedID, func(t *testing.T, flow store.TaskFlow) {
		if !flow.Side.Parked || flow.Side.Closed {
			t.Fatalf("side = %+v", flow.Side)
		}
		if want := map[string]string{store.FlowSideParked: "Parked"}; !reflect.DeepEqual(flow.Side.Labels, want) {
			t.Fatalf("side.labels = %v, want %v", flow.Side.Labels, want)
		}
	})

	closedID := createRunningTask(t, st, ctx, workspace)
	_, err = plane.Cancel(operator, core.Intervention{TaskID: closedID, Action: core.InterventionCancel, ReasonCode: "operator_cancel", ActorRole: core.ActorHuman})
	requireOK(t, err)
	add("closed", closedID, func(t *testing.T, flow store.TaskFlow) {
		if !flow.Side.Closed || flow.Side.Parked {
			t.Fatalf("side = %+v", flow.Side)
		}
		if want := map[string]string{store.FlowSideClosed: "Closed"}; !reflect.DeepEqual(flow.Side.Labels, want) {
			t.Fatalf("side.labels = %v, want %v", flow.Side.Labels, want)
		}
	})

	questionID := flowTask(t, st, ctx, workspace, core.Task{State: core.TaskRunning, NextStage: core.StageImplement})
	question := flowOrder(t, st, ctx, questionID, core.StageImplement, "1", time.Hour)
	questionClaim := flowClaim(t, st, ctx, question, "question", time.Hour)
	_, err = ReleaseWorkerClaim(ctx, st, question, questionClaim.WorkerID, core.WorkOrderRelease{
		SessionID: questionClaim.SessionID, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached,
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Which schema?"}, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
	})
	requireOK(t, err)
	add("agent question", questionID, func(t *testing.T, flow store.TaskFlow) {
		if flow.NextAction.Kind != nextaction.KindAgentQuestion || !flow.Side.AgentQuestion {
			t.Fatalf("next action = %s, side = %+v", flow.NextAction.Kind, flow.Side)
		}
		if want := map[string]string{store.FlowSideAgentQuestion: "Agent asks you"}; !reflect.DeepEqual(flow.Side.Labels, want) {
			t.Fatalf("side.labels = %v, want %v", flow.Side.Labels, want)
		}
		// An agent question is never a gate decision.
		for _, stage := range flow.Stages {
			if stage.GateStatus != nil && *stage.GateStatus == store.FlowNeedsOperator {
				t.Fatalf("%s gate needs the operator for an agent question", stage.ID)
			}
		}
	})
	return scenarios
}

// ReadTaskFlow is the store half of GET /v1/tasks/{id}/flow: the next action
// and the fold both read at now.
func ReadTaskFlow(t *testing.T, st store.Store, ctx context.Context, id string, now time.Time) store.TaskFlow {
	t.Helper()
	action, err := store.ReadNextAction(ctx, st, id, store.NextActionOptions{Now: now})
	requireOK(t, err)
	events, err := st.ListEvents(ctx, id)
	requireOK(t, err)
	return store.FoldTaskFlow(events, action, config.DefaultRecoveryPolicy(), now)
}

// ReplayTaskFlow copies a task's ledger verbatim into a fresh bare task on
// replay, whose task, work-order, job and intervention projections were never
// built by commands, and folds what replay stores. replay may be the source
// fixture: the copy lives under its own task ID.
func ReplayTaskFlow(t *testing.T, source store.Store, ctx context.Context, replay Fixture, id string, action nextaction.NextAction, now time.Time) store.TaskFlow {
	t.Helper()
	events, err := source.ListEvents(ctx, id)
	requireOK(t, err)
	copyID := core.NewTaskID()
	requireOK(t, replay.Backend.CreateTask(replay.Context, core.Task{ID: copyID, Workspace: replay.Workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/replay-" + copyID, State: core.TaskQueued, CreatedAt: time.Now().UTC()}))
	seed := make([]core.Event, len(events))
	for i, event := range events {
		event.ID, event.TaskID = int64(i+1), copyID
		seed[i] = event
	}
	seeded := replay.SeedEvents(t, replay.Context, 0, seed)
	keep := make(map[int64]bool, len(seeded))
	for _, event := range seeded {
		keep[event.ID] = true
	}
	stored, err := replay.Backend.ListEvents(replay.Context, copyID)
	requireOK(t, err)
	replayed := stored[:0]
	for _, event := range stored {
		if keep[event.ID] {
			replayed = append(replayed, event)
		}
	}
	if len(replayed) != len(events) {
		t.Fatalf("replay stored %d of %d events", len(replayed), len(events))
	}
	return store.FoldTaskFlow(replayed, action, config.DefaultRecoveryPolicy(), now)
}

func runTaskFlow(t *testing.T, factory Factory) {
	x := factory.fresh(t, nil)
	st, ctx := x.Backend, x.Context
	scenarios := SeedTaskFlowScenarios(t, st, ctx, x.Workspace)
	for _, scenario := range scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			now := time.Now().UTC()
			flow := ReadTaskFlow(t, st, ctx, scenario.TaskID, now)
			stages := make([]core.Stage, len(flow.Stages))
			for i, stage := range flow.Stages {
				stages[i] = stage.ID
			}
			if want := []core.Stage{core.StageTriage, core.StageSpec, core.StageImplement, core.StageVerify, core.StageReview, core.StageMerge}; !equalStages(stages, want) {
				t.Fatalf("stages = %v", stages)
			}
			count := flowBounceCount(t, st, ctx, scenario.TaskID)
			folded := 0
			for _, loop := range flow.Loops {
				folded += loop.Count
			}
			if folded != count {
				t.Fatalf("loops %+v count %d, store window counts %d", flow.Loops, folded, count)
			}
			switch scenario.Name {
			case "two bounces":
				if len(flow.Loops) != 1 || flow.Loops[0] != (store.FlowLoop{From: core.StageReview, To: core.StageImplement, Count: 2, Label: "sent back ×2"}) {
					t.Fatalf("loops = %+v", flow.Loops)
				}
			case "redirect window":
				if len(flow.Loops) != 1 || flow.Loops[0].Count != 1 {
					t.Fatalf("loops after redirect = %+v", flow.Loops)
				}
			}
			if !flow.AsOf.Equal(now) {
				t.Fatalf("as_of = %s, want the read's now %s", flow.AsOf, now)
			}
			if scenario.Check != nil {
				scenario.Check(t, flow)
			}
			want, err := json.Marshal(flow)
			requireOK(t, err)
			for range 50 {
				again, err := json.Marshal(ReadTaskFlowWith(t, st, ctx, scenario.TaskID, flow.NextAction, now))
				requireOK(t, err)
				if string(again) != string(want) {
					t.Fatalf("fold is not deterministic:\n%s\nwant\n%s", again, want)
				}
			}
			rebuilt, err := json.Marshal(ReplayTaskFlow(t, st, ctx, x, scenario.TaskID, flow.NextAction, now))
			requireOK(t, err)
			if string(rebuilt) != string(want) {
				t.Fatalf("replayed ledger folds differently:\n%s\nwant\n%s", rebuilt, want)
			}
		})
	}

	// A bounce recorded at the human redirect's own instant stays outside
	// the window in both the store count and the fold.
	t.Run("same instant bounce", func(t *testing.T) {
		var redirect string
		for _, scenario := range scenarios {
			if scenario.Name == "redirect window" {
				redirect = scenario.TaskID
			}
		}
		events, err := st.ListEvents(ctx, redirect)
		requireOK(t, err)
		var at time.Time
		for _, event := range events {
			if event.Kind == "intervention.redirect" && event.ActorRole == core.ActorHuman {
				at = event.At
			}
		}
		if at.IsZero() {
			t.Fatal("redirect intervention event missing")
		}
		x.SeedEvents(t, ctx, 0, []core.Event{{ID: 1, TaskID: redirect, Kind: "pipeline.bounced", Payload: core.JSONPayload(map[string]any{"from": "review", "to": "implement"}), At: at}})
		flow := ReadTaskFlow(t, st, ctx, redirect, time.Now().UTC())
		if count := flowBounceCount(t, st, ctx, redirect); count != 1 || len(flow.Loops) != 1 || flow.Loops[0].Count != 1 {
			t.Fatalf("store count %d, loops %+v; want 1", count, flow.Loops)
		}
	})

	// A verification failure records from=review beside source=verification;
	// the fold attributes it to verify while every other source keeps its
	// recorded from, and the stored payload is not rewritten
	// (req-operator-next-action AC-7.5).
	t.Run("verification bounce loop", func(t *testing.T) {
		id := flowTask(t, st, ctx, x.Workspace, core.Task{NextStage: core.StageImplement})
		at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		bounce := func(n int64, payload map[string]any) core.Event {
			return core.Event{ID: n, TaskID: id, Kind: "pipeline.bounced", ActorRole: core.ActorSystem, Payload: core.JSONPayload(payload), At: at.Add(time.Duration(n) * time.Minute)}
		}
		x.SeedEvents(t, ctx, 0, []core.Event{
			bounce(1, map[string]any{"from": "review", "to": "implement", "source": "verification", "reason_code": "verification-failed"}),
			bounce(2, map[string]any{"from": "review", "to": "implement"}),
			bounce(3, map[string]any{"from": "review", "to": "implement", "source": "mystery"}),
			bounce(4, map[string]any{"from": "review", "to": "implement", "source": "verification", "reason_code": "verification-failed"}),
		})
		flow := ReadTaskFlow(t, st, ctx, id, time.Now().UTC())
		want := []store.FlowLoop{
			{From: core.StageVerify, To: core.StageImplement, Count: 2, Label: "sent back ×2"},
			{From: core.StageReview, To: core.StageImplement, Count: 2, Label: "sent back ×2"},
		}
		if !reflect.DeepEqual(flow.Loops, want) {
			t.Fatalf("loops = %+v, want %+v", flow.Loops, want)
		}
		if count := flowBounceCount(t, st, ctx, id); count != 4 {
			t.Fatalf("store bounce window = %d, want 4", count)
		}
		events, err := st.ListEvents(ctx, id)
		requireOK(t, err)
		for _, event := range events {
			var payload struct {
				From string `json:"from"`
			}
			if event.Kind == "pipeline.bounced" && (json.Unmarshal(event.Payload, &payload) != nil || payload.From != "review") {
				t.Fatalf("recorded bounce payload changed: %s", event.Payload)
			}
		}
	})
}

// requireFlowGates asserts the plan and merge gate marks and that only those
// two rows carry one.
func requireFlowGates(t *testing.T, flow store.TaskFlow, plan, merge string) {
	t.Helper()
	for _, stage := range flow.Stages {
		var want string
		switch stage.ID {
		case core.StageSpec:
			want = plan
		case core.StageMerge:
			want = merge
		}
		got := ""
		if stage.GateStatus != nil {
			got = *stage.GateStatus
		}
		if got != want {
			t.Fatalf("%s gate_status = %q, want %q (next action %s/%s)", stage.ID, got, want, flow.NextAction.Actor, flow.NextAction.Kind)
		}
	}
}

// ReadTaskFlowWith folds the task's current ledger against a fixed next action
// at now.
func ReadTaskFlowWith(t *testing.T, st store.Store, ctx context.Context, id string, action nextaction.NextAction, now time.Time) store.TaskFlow {
	t.Helper()
	events, err := st.ListEvents(ctx, id)
	requireOK(t, err)
	return store.FoldTaskFlow(events, action, config.DefaultRecoveryPolicy(), now)
}

func taskFlowStage(t *testing.T, flow store.TaskFlow, id core.Stage) store.FlowStage {
	t.Helper()
	for _, stage := range flow.Stages {
		if stage.ID == id {
			return stage
		}
	}
	t.Fatalf("stage %s missing", id)
	return store.FlowStage{}
}

func equalStages(a, b []core.Stage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameFlowTime(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

func flowTask(t *testing.T, st store.Store, ctx context.Context, workspace string, task core.Task) string {
	t.Helper()
	task.ID = core.NewTaskID()
	task.Workspace, task.Repo, task.BaseBranch, task.Branch = workspace, "conveyor", "main", "conveyor/task-"+task.ID
	if task.State == "" {
		task.State = core.TaskRunning
	}
	task.CreatedAt = time.Now().UTC()
	requireOK(t, st.CreateTask(ctx, task))
	return task.ID
}

func flowOrder(t *testing.T, st store.Store, ctx context.Context, taskID string, stage core.Stage, n string, queue time.Duration) string {
	t.Helper()
	id := taskID + "-" + string(stage) + "-" + n
	now := time.Now().UTC()
	requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(queue), CreatedAt: now}))
	return id
}

// flowShort bounds the orders a scenario lets the clock act on; every other
// order is given an hour, so a scenario's tick never reaches another's. The
// clock ticks at the real time, so every event keeps its ledger order.
const flowShort = 50 * time.Millisecond

// flowMaxWait bounds any single flowExpire wait. Every legitimate wait is at
// most flowShort; a longer one means the harness picked the wrong deadline,
// so the flow fails fast instead of sleeping into the package timeout.
const flowMaxWait = 5 * time.Second

func flowClaim(t *testing.T, st store.Store, ctx context.Context, id, session string, bound time.Duration) core.WorkOrder {
	t.Helper()
	order, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker-flow", ClaimantID: "worker-flow", SessionID: "session-" + session, ClientToken: "token-" + session, Lease: time.Hour, ExecutionTimeout: bound, Agent: "omp", Model: "opus"})
	requireOK(t, err)
	return order
}

// flowExpire waits past the order's execution deadline when one is set,
// whatever its projected state, and its queue deadline otherwise, then ticks
// the order clock, so a claimed order times out and a queued one goes stale.
func flowExpire(t *testing.T, scenario string, st store.Store, ctx context.Context, id string) core.WorkOrder {
	t.Helper()
	order, err := st.GetWorkOrder(ctx, id)
	requireOK(t, err)
	// A read after the execution deadline projects a claimed order as
	// timed_out without writing (component-work-orders v42) and keeps
	// ExecutionDeadline, so select by the deadline, not the projected state.
	deadline := order.QueueDeadline
	if !order.ExecutionDeadline.IsZero() {
		deadline = order.ExecutionDeadline
	}
	wait := time.Until(deadline) + time.Millisecond
	if wait > flowMaxWait {
		t.Fatalf("flowExpire: scenario %q: order %s (state %s, execution_deadline %s, queue_deadline %s) would wait %s, over flowMaxWait %s", scenario, id, order.State, order.ExecutionDeadline, order.QueueDeadline, wait, flowMaxWait)
	}
	time.Sleep(wait)
	_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
	requireOK(t, err)
	order, err = st.GetWorkOrder(ctx, id)
	requireOK(t, err)
	if order.State != core.WorkOrderTimedOut && order.State != core.WorkOrderStale {
		t.Fatalf("scenario %q: order %s after clock = %s", scenario, id, order.State)
	}
	return order
}

// flowRecover expires the order and ticks to its scheduled automatic recovery.
func flowRecover(t *testing.T, scenario string, st store.Store, ctx context.Context, id string) {
	t.Helper()
	scheduled := flowExpire(t, scenario, st, ctx, id)
	if scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("scenario %q: order %s was not scheduled for automatic recovery: %+v", scenario, id, scheduled)
	}
	_, err := taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt)
	requireOK(t, err)
	order, err := st.GetWorkOrder(ctx, id)
	requireOK(t, err)
	if order.State != core.WorkOrderQueued {
		t.Fatalf("scenario %q: order %s after recovery = %s", scenario, id, order.State)
	}
}

func flowBounceCount(t *testing.T, st store.Store, ctx context.Context, id string) int {
	t.Helper()
	count, err := st.CountEventsSinceHumanIntervention(ctx, id, "pipeline.bounced")
	requireOK(t, err)
	return count
}
