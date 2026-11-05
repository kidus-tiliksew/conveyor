package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func distinctModelsTask() core.Task {
	task := core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageReview}
	task.SetupContract.Review.DistinctModels = true
	return task
}

// Only a seat holding a live claim, a submitted verdict, or an accepted verdict
// counts (req-review-gates-evidence AC-2.10).
func TestReviewSeatModelRefusalCountsHoldingStates(t *testing.T) {
	now := time.Now().UTC()
	claiming := core.WorkOrder{ID: "seat-2", TaskID: "task", Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2}
	sibling := func(state core.WorkOrderState) core.WorkOrder {
		return core.WorkOrder{ID: "seat-1", TaskID: "task", Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, State: state, Model: "gpt-6", LeaseExpiresAt: now.Add(time.Hour)}
	}
	for state, refused := range map[core.WorkOrderState]bool{
		core.WorkOrderClaimed:   true,
		core.WorkOrderSubmitted: true,
		core.WorkOrderCompleted: true,
		core.WorkOrderQueued:    false,
		core.WorkOrderStale:     false,
		core.WorkOrderTimedOut:  false,
		core.WorkOrderCancelled: false,
	} {
		err := ReviewSeatModelRefusal(distinctModelsTask(), claiming, "GPT-6", []core.WorkOrder{sibling(state)}, now)
		if refused != errors.Is(err, ErrReviewSeatModelNotDistinct) {
			t.Errorf("sibling in state %s: err=%v, refused=%t", state, err, refused)
		}
	}
	lapsed := sibling(core.WorkOrderClaimed)
	lapsed.LeaseExpiresAt = now.Add(-time.Second)
	if err := ReviewSeatModelRefusal(distinctModelsTask(), claiming, "gpt-6", []core.WorkOrder{lapsed}, now); err != nil {
		t.Errorf("lapsed claim counted: %v", err)
	}
	overdue := sibling(core.WorkOrderClaimed)
	overdue.ExecutionDeadline = now.Add(-time.Second)
	if err := ReviewSeatModelRefusal(distinctModelsTask(), claiming, "gpt-6", []core.WorkOrder{overdue}, now); err != nil {
		t.Errorf("claim past its execution deadline counted: %v", err)
	}
	running := sibling(core.WorkOrderClaimed)
	running.ExecutionDeadline = now.Add(time.Minute)
	if err := ReviewSeatModelRefusal(distinctModelsTask(), claiming, "gpt-6", []core.WorkOrder{running}, now); !errors.Is(err, ErrReviewSeatModelNotDistinct) {
		t.Errorf("claim inside its execution deadline err=%v", err)
	}
}

func TestReviewSeatModelRefusalScope(t *testing.T) {
	now := time.Now().UTC()
	task := distinctModelsTask()
	seat := core.WorkOrder{ID: "seat-2", TaskID: "task", Stage: core.StageReview, ReviewRound: 2, ReviewSeat: 2}
	held := core.WorkOrder{ID: "seat-1", TaskID: "task", Stage: core.StageReview, ReviewRound: 2, ReviewSeat: 1, State: core.WorkOrderSubmitted, Model: "gpt-6"}
	for name, others := range map[string]core.WorkOrder{
		"earlier round": {ID: "old", TaskID: "task", Stage: core.StageReview, ReviewRound: 1, State: core.WorkOrderCompleted, Model: "gpt-6"},
		"implementer":   {ID: "impl", TaskID: "task", Stage: core.StageImplement, State: core.WorkOrderSubmitted, Model: "gpt-6"},
		"another task":  {ID: "other", TaskID: "other", Stage: core.StageReview, ReviewRound: 2, State: core.WorkOrderCompleted, Model: "gpt-6"},
		"itself":        {ID: "seat-2", TaskID: "task", Stage: core.StageReview, ReviewRound: 2, State: core.WorkOrderCompleted, Model: "gpt-6"},
	} {
		if err := ReviewSeatModelRefusal(task, seat, "gpt-6", []core.WorkOrder{others}, now); err != nil {
			t.Errorf("%s counted: %v", name, err)
		}
	}
	off := task
	off.SetupContract.Review.DistinctModels = false
	if err := ReviewSeatModelRefusal(off, seat, "gpt-6", []core.WorkOrder{held}, now); err != nil {
		t.Errorf("policy off compared names: %v", err)
	}
	if err := ReviewSeatModelRefusal(off, seat, "", nil, now); err != nil {
		t.Errorf("policy off refused a blank name: %v", err)
	}
	legacy := seat
	legacy.ReviewRound = 0
	if err := ReviewSeatModelRefusal(task, legacy, "", nil, now); err != nil {
		t.Errorf("legacy round 0 seat refused: %v", err)
	}
	verify := core.WorkOrder{ID: "verify", TaskID: "task", Stage: core.StageVerify}
	if err := ReviewSeatModelRefusal(task, verify, "", nil, now); err != nil {
		t.Errorf("verify order refused: %v", err)
	}
	err := ReviewSeatModelRefusal(task, seat, "  ", []core.WorkOrder{held}, now)
	if !errors.Is(err, ErrReviewSeatModelNotDistinct) || !strings.Contains(err.Error(), "gpt-6") {
		t.Errorf("blank name err=%v", err)
	}
}

// A policy-only change on a task with verify_stage true waiting at review
// returns before handoff resolution; a verify_stage change on the same task
// still hands off (req-review-gates-evidence AC-2.11).
func TestPlanTaskPolicyChangeDistinctModelsOnly(t *testing.T) {
	task := core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "head", RefreshReviewScope: config.RefreshReviewDelta}
	task.SetupContract = config.PolicyContract{VerifyStage: false, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}}
	orders := []core.WorkOrder{{ID: "seat", TaskID: "task", Stage: core.StageReview, State: core.WorkOrderQueued, HeadSHA: "head", ReviewScope: config.RefreshReviewDelta, BaselineSHA: "base", ReviewRound: 1, ReviewSeat: 1}}
	on, enable := true, true
	task.SetupContract.VerifyStage = true
	plan, err := PlanTaskPolicyChange(task, orders, SetupChangeRequest{Policy: &TaskPolicyChange{ReviewDistinctModels: &on}}, PolicyHandoffSeatInputs{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Setup.Review.DistinctModels || plan.NextStage != core.StageReview || plan.ReviewTransition != "policy_only" ||
		len(plan.NewWorkOrders)+len(plan.NewJobs)+len(plan.WorkOrderUpdates)+len(plan.SupersedeWorkOrderIDs) != 0 ||
		plan.ReviewRoundSeats != nil || plan.ResultingReviewRound != 0 || plan.Setup.StageTimeouts.Verify != "" {
		t.Fatalf("policy-only plan=%+v", plan)
	}
	task.SetupContract.VerifyStage = false
	handoff, err := PlanTaskPolicyChange(task, orders, SetupChangeRequest{Policy: &TaskPolicyChange{VerifyStage: &enable, ReviewDistinctModels: &on}}, PolicyHandoffSeatInputs{})
	if err != nil {
		t.Fatal(err)
	}
	if handoff.NextStage != core.StageVerify || handoff.ReviewTransition != "policy_handoff" || !handoff.Setup.Review.DistinctModels {
		t.Fatalf("combined change plan=%+v", handoff)
	}
	for _, blocked := range []core.WorkOrder{
		{ID: "claimed", TaskID: "task", Stage: core.StageReview, State: core.WorkOrderClaimed},
		{ID: "submitted", TaskID: "task", Stage: core.StageReview, State: core.WorkOrderSubmitted},
	} {
		if _, err := PlanTaskPolicyChange(task, []core.WorkOrder{blocked}, SetupChangeRequest{Policy: &TaskPolicyChange{ReviewDistinctModels: &on}}, PolicyHandoffSeatInputs{}); !errors.Is(err, ErrSetupChangeConflict) {
			t.Errorf("%s seat err=%v", blocked.ID, err)
		}
	}
}

func TestTaskPolicyChangeValidateNamesDistinctModels(t *testing.T) {
	err := TaskPolicyChange{}.Validate()
	if err == nil || !strings.Contains(err.Error(), "review_distinct_models") {
		t.Fatalf("empty change err=%v", err)
	}
	off := false
	if err := (TaskPolicyChange{ReviewDistinctModels: &off}).Validate(); err != nil {
		t.Fatalf("turning the policy off err=%v", err)
	}
}
