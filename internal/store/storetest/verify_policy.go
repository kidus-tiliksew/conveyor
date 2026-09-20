package storetest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// DEC-43 / VK-2: the same policy handoff and claim lifecycle runs against all stores.
func runVerifyPolicy(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "submitted-head", CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.ExecutionSetup{MaxBounces: 3, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}, ExecutionSettings: config.ContextualExecutionSettings{Review: config.ReviewExecutionSettings{TimeoutText: "1h"}}}
	requireOK(t, st.CreateTask(ctx, task))
	enabled := true
	request := store.SetupChangeRequest{TaskID: task.ID, RequestID: core.NewTaskID(), Reason: "enable verification", Policy: &store.TaskPolicyChange{VerifyStage: &enabled, StageTimeouts: map[string]string{"verify": "45m"}}}
	change := func(r store.SetupChangeRequest) (store.SetupChangeResult, error) {
		return taskops.ExecuteSetupChange(ctx, st, task.ID, func(l taskops.TaskLease) (store.SetupChangeResult, error) {
			return st.ChangeTaskSetupCommand(ctx, l, r)
		})
	}
	bad := request
	bad.Reason = " "
	if _, err := change(bad); err == nil {
		t.Fatal("policy change accepted blank reason")
	}
	result, err := change(request)
	requireOK(t, err)
	if !result.Task.SetupContract.VerifyStage || result.Task.NextStage != core.StageVerify || len(result.CreatedWorkOrders) != 1 {
		t.Fatalf("policy result: %+v", result)
	}
	verify, err := st.GetWorkOrder(ctx, result.CreatedWorkOrders[0])
	requireOK(t, err)
	if verify.ID != task.ID+"-verify-1" || verify.HeadSHA != "submitted-head" || verify.ReviewSeat != 0 || verify.ExecutionTimeoutText != "45m" {
		t.Fatalf("verify handoff: %+v", verify)
	}
	before, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	replay, err := change(request)
	requireOK(t, err)
	if len(replay.CreatedWorkOrders) != 1 || replay.CreatedWorkOrders[0] != verify.ID {
		t.Fatal("replay changed result")
	}
	after, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	if len(before) != len(after) {
		t.Fatal("replay appended events")
	}
	foreignActor := store.WithActor(ctx, store.Actor{ID: "different-operator", Role: core.ActorHuman})
	_, actorErr := taskops.ExecuteSetupChange(foreignActor, st, task.ID, func(l taskops.TaskLease) (store.SetupChangeResult, error) {
		return st.ChangeTaskSetupCommand(foreignActor, l, request)
	})
	if actorErr == nil {
		t.Fatal("policy replay accepted a different actor")
	}
	conflict := request
	conflict.Reason = "different reason"
	if _, err := change(conflict); err == nil {
		t.Fatal("changed replay accepted")
	}
	found := false
	for _, e := range after {
		if e.Kind == "task.setup.changed" {
			var p struct {
				Reason string `json:"reason"`
			}
			requireOK(t, json.Unmarshal(e.Payload, &p))
			found = p.Reason == request.Reason
		}
	}
	if !found {
		t.Fatal("policy reason missing from audit")
	}
	claimed, err := ClaimWorkOrder(ctx, st, verify.ID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "verify-session", ClientToken: "verify-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	if claimed.Stage != core.StageVerify || claimed.State != core.WorkOrderClaimed {
		t.Fatal("verify claim failed")
	}
	disabled := false
	next := store.SetupChangeRequest{TaskID: task.ID, RequestID: core.NewTaskID(), Reason: "disable verification", Policy: &store.TaskPolicyChange{VerifyStage: &disabled}}
	if _, err := change(next); err == nil {
		t.Fatal("policy changed during verify claim")
	}
	renewed, err := RenewWorkerClaim(ctx, st, verify.ID, "worker", "verify-session", 2*time.Minute)
	requireOK(t, err)
	if renewed.AttemptID != claimed.AttemptID {
		t.Fatal("renewal changed attempt")
	}
	_, err = ReleaseWorkerClaim(ctx, st, verify.ID, "worker", core.WorkOrderRelease{SessionID: "verify-session", Reason: "fixture", Outcome: core.WorkOrderOutcomeReleased})
	requireOK(t, err)
	result, err = change(next)
	requireOK(t, err)
	if result.Task.NextStage != core.StageReview || len(result.CreatedWorkOrders) != 1 {
		t.Fatalf("disabled handoff: %+v", result)
	}
	retired, err := st.GetWorkOrder(ctx, verify.ID)
	requireOK(t, err)
	if retired.State != core.WorkOrderCancelled {
		t.Fatal("obsolete verify order still claimable")
	}
	request.RequestID = core.NewTaskID()
	result, err = change(request)
	requireOK(t, err)
	second, err := ClaimWorkOrder(ctx, st, result.CreatedWorkOrders[0], core.WorkOrderClaim{ClaimantID: "worker", SessionID: "verify-session-2", ClientToken: "verify-token-2", Lease: time.Minute})
	requireOK(t, err)
	second.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(ctx, st, second, core.WorkOrderCmdSubmitVerification))
	completed, err := st.GetWorkOrder(ctx, second.ID)
	requireOK(t, err)
	if !core.VerifyReviewReady(result.Task, []core.WorkOrder{completed}) {
		t.Fatal("completed verify did not unlock its exact head")
	}
	result.Task.ReviewedHeadSHA = "different-head"
	if core.VerifyReviewReady(result.Task, []core.WorkOrder{completed}) {
		t.Fatal("completed verify unlocked a different head")
	}
}

func runVerifyAdmission(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x, core.StageVerify)
	claim := core.WorkOrderClaim{WorkerID: "verify-worker", ClaimantID: "verify-worker", SessionID: "verify-admission", ClientToken: "verify-admission", Lease: time.Minute}
	var err error
	dependency := newAggregateTask(t, x)
	_, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: order.TaskID, DependsOnTaskID: dependency.ID, Reason: "verify dependency", RequestID: core.NewTaskID()})
	requireOK(t, err)
	if _, err = ClaimWorkOrder(ctx, st, order.ID, claim); err == nil {
		t.Fatal("blocked verify was claimed")
	}
	blocked, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if blocked.QueueBlockedAt.IsZero() {
		t.Fatal("verify queue clock did not pause")
	}
	_, err = st.RemoveTaskDependency(ctx, store.DependencyRemovalRequest{TaskID: order.TaskID, DependsOnTaskID: dependency.ID, Reason: "remove dependency", RequestID: core.NewTaskID()})
	requireOK(t, err)
	resumed, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if !resumed.QueueBlockedAt.IsZero() || resumed.QueueDeadline.Before(order.QueueDeadline) {
		t.Fatal("verify queue clock did not resume")
	}
	document, proposal, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "verify-" + core.NewTaskID(), Title: "Verify proposal", Category: "Architecture"}, core.SystemDesignVersion{Content: designProposalContent("verify"), Origin: core.SystemDesignOriginImplementation, OriginTaskID: order.TaskID})
	requireOK(t, err)
	if _, err = ClaimWorkOrder(ctx, st, order.ID, claim); err == nil {
		t.Fatal("verify bypassed undecided proposal")
	}
	_, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, proposal.Version)
	requireOK(t, err)
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, claim)
	requireOK(t, err)
	if _, err = ClaimWorkOrder(ctx, st, order.ID, claim); err == nil {
		t.Fatal("verify admitted a concurrent claim")
	}
	claimed.State = core.WorkOrderCancelled
	requireOK(t, UpdateWorkOrder(ctx, st, claimed, core.WorkOrderCmdCancel))
}
