package workorder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// policyChangeFixture is a task whose implementation was submitted at "head"
// and handed to a queued verify order bound to baseline "base". Its frozen
// policy names a two-seat review panel.
func policyChangeFixture(t *testing.T) (*Service, store.Store, context.Context, core.Task, core.WorkOrder, core.WorkOrder) {
	t.Helper()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), "demo"), store.Actor{ID: "operator", Role: core.ActorHuman})
	st := store.NewMemory()
	contract := config.ExecutionSetup{Name: "legacy", VerifyStage: true, MaxBounces: 3, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}},
		ExecutionSettings: config.ContextualExecutionSettings{Verify: config.ImplementationSettings{TimeoutText: "1h"}, Review: config.ReviewExecutionSettings{TimeoutText: "45m"}}}
	now := time.Now().UTC()
	task := core.Task{ID: "policy-change", Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head", SetupName: "legacy", SetupContract: contract, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	implement := core.WorkOrder{ID: task.ID + "-implement-1", TaskID: task.ID, JobID: task.ID + "-implement-1", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	verify := core.WorkOrder{ID: task.ID + "-verify-1", TaskID: task.ID, JobID: task.ID + "-verify-1", Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: "head", BaselineSHA: "base", ExecutionTimeoutText: "1h", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	for _, order := range []core.WorkOrder{implement, verify} {
		if err := st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, implement.ID, core.WorkOrderClaim{SessionID: "delivered", ClientToken: "delivered-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	return &Service{Store: st}, st, ctx, task, claimed, verify
}

func TestChangeTaskPolicyHandsSubmittedImplementationToEveryReviewSeat(t *testing.T) {
	service, st, ctx, task, implement, verify := policyChangeFixture(t)
	disabled := false
	result, err := service.ChangeTaskPolicy(ctx, task.ID, "skip verification", "policy-disable", store.TaskPolicyChange{VerifyStage: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewTransition != "policy_handoff" || result.Task.NextStage != core.StageReview || result.Task.SetupContract.VerifyStage ||
		len(result.CreatedWorkOrders) != 2 || len(result.SupersededWorkOrders) != 1 || result.SupersededWorkOrders[0] != verify.ID {
		t.Fatalf("result=%+v", result)
	}
	if result.Task.SetupName != "legacy" || result.Task.SetupContract.Name != "legacy" {
		t.Fatalf("policy change selected an execution setup: %+v", result.Task)
	}
	for seat, id := range result.CreatedWorkOrders {
		order, getErr := st.GetWorkOrder(ctx, id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if order.Stage != core.StageReview || order.ReviewRound != 1 || order.ReviewSeat != seat+1 || order.HeadSHA != "head" || order.BaselineSHA != "base" ||
			order.ExecutionTimeoutText != "45m" || order.State != core.WorkOrderQueued || order.RequiredHarness != "" || order.RequiredModel != "" {
			t.Fatalf("review seat %d=%+v", seat+1, order)
		}
	}
	retired, _ := st.GetWorkOrder(ctx, verify.ID)
	if retired.State != core.WorkOrderCancelled {
		t.Fatalf("superseded verify order=%+v", retired)
	}
	untouched, _ := st.GetWorkOrder(ctx, implement.ID)
	if untouched.State != core.WorkOrderSubmitted {
		t.Fatalf("delivered implementation mutated=%+v", untouched)
	}
	for kind, want := range map[string]int{"task.setup.changed": 1, "review.seat.setup_rebuilt": 2, "work_order.cancelled": 1, "pipeline.transition_decided": 1} {
		if count, countErr := st.CountEvents(ctx, task.ID, kind); countErr != nil || count != want {
			t.Fatalf("%s events=%d want %d err=%v", kind, count, want, countErr)
		}
	}
	// A same-input replay returns the recorded result without new writes.
	before, _ := st.ListEvents(ctx, task.ID)
	replay, err := service.ChangeTaskPolicy(ctx, task.ID, "skip verification", "policy-disable", store.TaskPolicyChange{VerifyStage: &disabled})
	if err != nil || len(replay.CreatedWorkOrders) != 2 || replay.CreatedWorkOrders[0] != result.CreatedWorkOrders[0] {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if after, _ := st.ListEvents(ctx, task.ID); len(after) != len(before) {
		t.Fatalf("replay appended %d events", len(after)-len(before))
	}
}

func TestChangeTaskPolicyUpdatesQueuedVerifyTimeoutOnly(t *testing.T) {
	service, st, ctx, task, _, verify := policyChangeFixture(t)
	result, err := service.ChangeTaskPolicy(ctx, task.ID, "longer verification", "policy-timeout", store.TaskPolicyChange{StageTimeouts: map[string]string{"verify": "2h"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewTransition != "policy_only" || result.Task.NextStage != core.StageVerify || len(result.CreatedWorkOrders) != 0 ||
		len(result.UpdatedWorkOrders) != 1 || result.UpdatedWorkOrders[0] != verify.ID || result.Task.SetupContract.ExecutionSettings.Verify.TimeoutText != "2h" {
		t.Fatalf("result=%+v", result)
	}
	updated, _ := st.GetWorkOrder(ctx, verify.ID)
	if updated.State != core.WorkOrderQueued || updated.ExecutionTimeoutText != "2h" || updated.HeadSHA != "head" || updated.BaselineSHA != "base" || updated.RedispatchCount != 1 {
		t.Fatalf("verify order=%+v", updated)
	}
}

func TestChangeTaskPolicyRejectsClaimedAttemptWithoutMutation(t *testing.T) {
	service, st, ctx, task, _, verify := policyChangeFixture(t)
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, verify.ID, core.WorkOrderClaim{SessionID: "active", ClientToken: "secret", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := service.ChangeTaskPolicy(ctx, task.ID, "too late", "policy-active", store.TaskPolicyChange{VerifyStage: &disabled}); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("err=%v", err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if !current.SetupContract.VerifyStage || current.NextStage != core.StageVerify {
		t.Fatalf("task mutated=%+v", current)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "task.setup.changed"); count != 0 {
		t.Fatalf("refused change appended %d audit events", count)
	}
}

func TestChangeTaskPolicyRejectsInFlightReviewVerdict(t *testing.T) {
	service, st, ctx, task, _, _ := policyChangeFixture(t)
	disabled := false
	result, err := service.ChangeTaskPolicy(ctx, task.ID, "skip verification", "policy-to-review", store.TaskPolicyChange{VerifyStage: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	seat, err := storetest.For(st).ClaimWorkOrder(ctx, result.CreatedWorkOrders[0], core.WorkOrderClaim{SessionID: "verdict", ClientToken: "verdict-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	seat.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, seat); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err = service.ChangeTaskPolicy(ctx, task.ID, "verdict in flight", "policy-verdict", store.TaskPolicyChange{VerifyStage: &enabled}); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("err=%v", err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.SetupContract.VerifyStage || current.NextStage != core.StageReview {
		t.Fatalf("task mutated=%+v", current)
	}
}
