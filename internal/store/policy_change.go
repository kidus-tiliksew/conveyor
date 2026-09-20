package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TaskPolicyChange contains only the explicit frozen-policy exception admitted
// by DEC-7 and DEC-43 (feature-verification-kit-execution VK-10.1).
type TaskPolicyChange struct {
	VerifyStage   *bool             `json:"verify_stage,omitempty"`
	StageTimeouts map[string]string `json:"stage_timeouts,omitempty"`
}

func (p TaskPolicyChange) Validate() error {
	if p.VerifyStage == nil && len(p.StageTimeouts) == 0 {
		return fmt.Errorf("verify_stage or stage_timeouts.verify is required")
	}
	for stage, value := range p.StageTimeouts {
		if stage != "verify" {
			return fmt.Errorf("only stage_timeouts.verify may change")
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return fmt.Errorf("stage_timeouts.verify must be a positive duration")
		}
	}
	return nil
}

// PlanTaskPolicyChange runs inside the backend task transaction, after replay
// resolution and claim exclusion. It never accepts client-supplied order plans.
func PlanTaskPolicyChange(task core.Task, orders []core.WorkOrder, r SetupChangeRequest) (SetupChangeRequest, error) {
	if r.Policy == nil {
		return r, nil
	}
	if err := r.Policy.Validate(); err != nil {
		return r, err
	}
	if task.State == core.TaskApproved || (task.State == core.TaskAwaiting && task.NextStage == "") {
		return r, fmt.Errorf("%w: policy changes require a queued or submitted handoff", ErrSetupChangeConflict)
	}
	r.Setup = task.SetupContract
	if r.Policy.VerifyStage != nil {
		r.Setup.VerifyStage = *r.Policy.VerifyStage
	}
	if value, ok := r.Policy.StageTimeouts["verify"]; ok {
		r.Setup.ExecutionSettings.Verify.TimeoutText = strings.TrimSpace(value)
	}
	if r.Setup.ExecutionSettings.Verify.TimeoutText == "" {
		r.Setup.ExecutionSettings.Verify.TimeoutText = "1h"
	}
	r.ReviewTransition = "policy_only"
	r.NextStage = task.NextStage
	r.WorkOrderUpdates, r.NewJobs, r.NewWorkOrders = nil, nil, nil
	r.SupersedeWorkOrderIDs, r.RetainedWorkOrderIDs = nil, nil
	for _, order := range orders {
		if order.State == core.WorkOrderClaimed || (order.Stage == core.StageReview && order.State == core.WorkOrderSubmitted) {
			return r, fmt.Errorf("%w: task has an executing attempt or in-flight review verdict", ErrSetupChangeConflict)
		}
	}
	if task.NextStage != core.StageReview && task.NextStage != core.StageVerify {
		return r, nil
	}
	target := core.StageReview
	if r.Setup.VerifyStage {
		target = core.StageVerify
	}
	if target == task.NextStage {
		for _, order := range orders {
			if order.Stage == core.StageVerify && order.State == core.WorkOrderQueued {
				order.ExecutionTimeoutText = r.Setup.ExecutionSettings.Verify.TimeoutText
				r.WorkOrderUpdates = append(r.WorkOrderUpdates, order)
			}
		}
		return r, nil
	}
	// Only a submitted revision can be redirected. Historical review results
	// never authorize a new review round after enabling verification.
	if core.VerifyStageHead(task) == "" {
		return r, fmt.Errorf("%w: submitted head is required for policy handoff", ErrSetupChangeConflict)
	}
	verifyAttempt, reviewRound := 1, 1
	for _, order := range orders {
		if order.Stage == core.StageVerify {
			verifyAttempt++
		}
		if order.ReviewRound >= reviewRound {
			reviewRound = order.ReviewRound + 1
		}
		if order.Stage == task.NextStage && (order.State == core.WorkOrderQueued || (order.Stage == core.StageReview && order.State == core.WorkOrderCompleted)) {
			r.SupersedeWorkOrderIDs = append(r.SupersedeWorkOrderIDs, order.ID)
		}
	}
	r.NextStage, r.ReviewTransition = target, "policy_handoff"
	now := time.Now().UTC()
	count := 1
	timeout := r.Setup.ExecutionSettings.Verify.TimeoutText
	if target == core.StageReview {
		count = len(r.Setup.Review.Seats)
		if count == 0 {
			count = 1
		}
		timeout = r.Setup.ExecutionSettings.Review.TimeoutText
		r.ResultingReviewRound = reviewRound
	}
	for seat := 1; seat <= count; seat++ {
		id := fmt.Sprintf("%s-verify-%d", task.ID, verifyAttempt)
		if target == core.StageReview {
			id = fmt.Sprintf("%s-review-%d-seat-%d", task.ID, reviewRound, seat)
		}
		job := core.Job{ID: id, TaskID: task.ID, Stage: target, Harness: "external-mcp", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
		order := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: target, State: core.WorkOrderQueued, Claimable: true, HeadSHA: core.VerifyStageHead(task), ExecutionTimeoutText: timeout, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(config.DefaultWorkOrderQueueTimeout)}
		if target == core.StageReview {
			order.ReviewRound, order.ReviewSeat = reviewRound, seat
			order.ReviewScope, order.BaselineSHA = task.RefreshReviewScope, task.RefreshBaselineSHA
			if task.ApprovalStale {
				order.ReviewKind = "refresh"
			}
		}
		r.NewJobs = append(r.NewJobs, job)
		r.NewWorkOrders = append(r.NewWorkOrders, order)
	}
	return r, nil
}
