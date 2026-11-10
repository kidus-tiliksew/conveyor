package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TaskPolicyChange contains only the explicit, audited frozen-policy exception
// admitted by DEC-47 and DEC-43 (component-task-lifecycle). An absent
// ReviewDistinctModels leaves the stored replay identity of earlier changes
// unchanged.
//
// PlanState is the plan_state change of component-plan-state invariant 4: it
// holds needed or not_required and travels alone (component-task-lifecycle
// invariant 44; DEC-125(7)).
type TaskPolicyChange struct {
	PlanState            *core.PlanState   `json:"plan_state,omitempty"`
	VerifyStage          *bool             `json:"verify_stage,omitempty"`
	StageTimeouts        map[string]string `json:"stage_timeouts,omitempty"`
	ReviewDistinctModels *bool             `json:"review_distinct_models,omitempty"`
}

// OnlyDistinctModels reports a change holding review_distinct_models and no
// field that can move the task's stage (req-review-gates-evidence AC-2.11).
func (p TaskPolicyChange) OnlyDistinctModels() bool {
	return p.ReviewDistinctModels != nil && p.VerifyStage == nil && len(p.StageTimeouts) == 0
}

func (p TaskPolicyChange) Validate() error {
	if p.PlanState != nil {
		if *p.PlanState != core.PlanNeeded && *p.PlanState != core.PlanNotRequired {
			return fmt.Errorf("plan_state must be %s or %s", core.PlanNeeded, core.PlanNotRequired)
		}
		if p.VerifyStage != nil || p.StageTimeouts != nil || p.ReviewDistinctModels != nil {
			return fmt.Errorf("plan_state must be the only policy field in its change")
		}
		return nil
	}
	if p.VerifyStage == nil && len(p.StageTimeouts) == 0 && p.ReviewDistinctModels == nil {
		return fmt.Errorf("plan_state, verify_stage, stage_timeouts.verify, or review_distinct_models is required")
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
// Every derived field is reset first, so a caller-supplied execution setup,
// order update, or review plan never reaches persistence (DEC-56). A handoff
// to review resolves its round's seats through ResolveReviewRoundSeats from
// the seat inputs the backend loaded in the same transaction
// (feature-review-seat-minimum RS-2). A change holding only
// review_distinct_models returns before handoff resolution: it sets the frozen
// value for later seat claims and changes no order and opens no round, even on
// a task with verify_stage true waiting at review (req-review-gates-evidence
// AC-2.11).
func PlanTaskPolicyChange(task core.Task, orders []core.WorkOrder, r SetupChangeRequest, seats PolicyHandoffSeatInputs) (SetupChangeRequest, error) {
	if r.Policy == nil {
		return r, Refusef("a frozen-policy change is required")
	}
	if err := r.Policy.Validate(); err != nil {
		return r, err
	}
	if r.Policy.PlanState != nil {
		return planTaskPlanStateChange(task, orders, r)
	}
	if task.State == core.TaskApproved || (task.State == core.TaskAwaiting && task.NextStage == "") {
		return r, fmt.Errorf("%w: policy changes require a queued or submitted handoff", ErrSetupChangeConflict)
	}
	r.Setup = task.SetupContract
	if r.Policy.VerifyStage != nil {
		r.Setup.VerifyStage = *r.Policy.VerifyStage
	}
	if value, ok := r.Policy.StageTimeouts["verify"]; ok {
		r.Setup.StageTimeouts.Verify = strings.TrimSpace(value)
	}
	if r.Policy.ReviewDistinctModels != nil {
		r.Setup.Review.DistinctModels = *r.Policy.ReviewDistinctModels
	}
	onlyDistinctModels := r.Policy.OnlyDistinctModels()
	if r.Setup.StageTimeouts.Verify == "" && !onlyDistinctModels {
		r.Setup.StageTimeouts.Verify = "1h"
	}
	r.ReviewTransition = "policy_only"
	r.NextStage = task.NextStage
	r.WorkOrderUpdates, r.NewJobs, r.NewWorkOrders, r.SupersedeWorkOrderIDs = nil, nil, nil, nil
	r.PriorReviewRound, r.ResultingReviewRound = 0, 0
	r.ReviewRoundSeats = nil
	for _, order := range orders {
		if order.State == core.WorkOrderClaimed || (order.Stage == core.StageReview && order.State == core.WorkOrderSubmitted) {
			return r, fmt.Errorf("%w: task has an executing attempt or in-flight review verdict", ErrSetupChangeConflict)
		}
	}
	if onlyDistinctModels {
		return r, nil
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
				order.ExecutionTimeoutText = r.Setup.StageTimeouts.Verify
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
	baseline := task.RefreshBaselineSHA
	if task.ApprovalStale {
		if baseline == "" || task.RefreshHeadSHA == "" || (task.RefreshReviewScope != config.RefreshReviewDelta && task.RefreshReviewScope != config.RefreshReviewFull) {
			return r, fmt.Errorf("%w: refresh binding is required for policy handoff", ErrSetupChangeConflict)
		}
	} else {
		// Inherit the trusted current-stage binding that sealed review
		// acceptance checks (component-verification-service), never a branch
		// name or an approved head from a different review cycle.
		baseline = ""
		for _, order := range orders {
			if order.TaskID != task.ID || order.Stage != task.NextStage || order.HeadSHA != task.ReviewedHeadSHA || order.ReviewScope != task.RefreshReviewScope || order.BaselineSHA == "" || !policyHandoffBaselineState(order, task) {
				continue
			}
			if baseline != "" && baseline != order.BaselineSHA {
				return r, fmt.Errorf("%w: conflicting policy handoff baselines", ErrSetupChangeConflict)
			}
			baseline = order.BaselineSHA
		}
		if baseline == "" {
			return r, fmt.Errorf("%w: current order binding is required for policy handoff", ErrSetupChangeConflict)
		}
	}
	verifyAttempt, reviewRound := 1, 1
	for _, order := range orders {
		if order.Stage == core.StageVerify {
			verifyAttempt++
		}
		if order.ReviewRound >= reviewRound {
			reviewRound = order.ReviewRound + 1
		}
		if policyHandoffSupersedeState(order, task) {
			r.SupersedeWorkOrderIDs = append(r.SupersedeWorkOrderIDs, order.ID)
		}
	}
	r.NextStage, r.ReviewTransition = target, "policy_handoff"
	now := time.Now().UTC()
	count := 1
	timeout := r.Setup.StageTimeouts.Verify
	if target == core.StageReview {
		minimum := ReviewSeatMinimum(r.Setup, r.WorkspaceReviewSeats)
		resolved := ResolveReviewRoundSeats(ReviewRoundSeatInputsFor(minimum, reviewRound, orders, seats.Events, seats.Records, seats.PlannedSeatCount))
		if resolved.SeatCount < resolved.FloorSeatCount {
			return r, fmt.Errorf("%w: review round %d seat count %d is below the seat floor %d", ErrSetupChangeConflict, reviewRound, resolved.SeatCount, resolved.FloorSeatCount)
		}
		count = resolved.SeatCount
		r.ReviewRoundSeats = &resolved
		timeout = r.Setup.StageTimeouts.Review
		r.ResultingReviewRound = reviewRound
	}
	for seat := 1; seat <= count; seat++ {
		id := fmt.Sprintf("%s-verify-%d", task.ID, verifyAttempt)
		if target == core.StageReview {
			id = fmt.Sprintf("%s-review-%d-seat-%d", task.ID, reviewRound, seat)
		}
		job := core.Job{ID: id, TaskID: task.ID, Stage: target, Harness: "external-mcp", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
		order := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: target, State: core.WorkOrderQueued, Claimable: true, HeadSHA: core.VerifyStageHead(task), ReviewScope: task.RefreshReviewScope, BaselineSHA: baseline, ExecutionTimeoutText: timeout, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(config.DefaultWorkOrderQueueTimeout)}
		if target == core.StageReview {
			order.ReviewRound, order.ReviewSeat = reviewRound, seat
			if task.ApprovalStale {
				order.ReviewKind = "refresh"
			}
		}
		r.NewJobs = append(r.NewJobs, job)
		r.NewWorkOrders = append(r.NewWorkOrders, order)
	}
	return r, nil
}

func policyHandoffBaselineState(order core.WorkOrder, task core.Task) bool {
	if order.State == core.WorkOrderQueued || order.State == core.WorkOrderCompleted {
		return true
	}
	return task.NextStage == core.StageVerify && order.Stage == core.StageVerify &&
		(order.State == core.WorkOrderStale || order.State == core.WorkOrderTimedOut)
}

func policyHandoffSupersedeState(order core.WorkOrder, task core.Task) bool {
	if order.Stage != task.NextStage {
		return false
	}
	if order.State == core.WorkOrderQueued || (order.Stage == core.StageReview && order.State == core.WorkOrderCompleted) {
		return true
	}
	return task.NextStage == core.StageVerify && order.Stage == core.StageVerify &&
		(order.State == core.WorkOrderStale || order.State == core.WorkOrderTimedOut)
}

// PolicyHandoffRetireCommand returns the lifecycle command and event kind used
// to retire a superseded order during a frozen-policy handoff. Queued orders
// keep the historical cancel path. A stranded stale verify order uses W16
// (work_order.retired); a stranded timed_out verify order uses W15
// (work_order.cancelled).
func PolicyHandoffRetireCommand(order core.WorkOrder) (core.WorkOrderCommand, string, bool) {
	switch order.State {
	case core.WorkOrderQueued:
		return core.WorkOrderCmdCancel, "work_order.cancelled", true
	case core.WorkOrderStale:
		if order.Stage == core.StageVerify {
			return core.WorkOrderCmdPreempt, "work_order.retired", true
		}
	case core.WorkOrderTimedOut:
		if order.Stage == core.StageVerify {
			return core.WorkOrderCmdCancel, "work_order.cancelled", true
		}
	}
	return "", "", false
}

func PolicyHandoffSupersessionAllowed(order core.WorkOrder, fromStage core.Stage) bool {
	if order.State == core.WorkOrderQueued {
		return true
	}
	if order.Stage == core.StageReview && order.State == core.WorkOrderCompleted {
		return true
	}
	return fromStage == core.StageVerify && order.Stage == core.StageVerify &&
		(order.State == core.WorkOrderStale || order.State == core.WorkOrderTimedOut)
}

// planTaskPlanStateChange derives a plan_state change: the frozen setup is
// unchanged, and the effects row of component-plan-state invariant 4 or its
// refusal comes from PlanPlanStateChange.
func planTaskPlanStateChange(task core.Task, orders []core.WorkOrder, r SetupChangeRequest) (SetupChangeRequest, error) {
	r.Setup = task.SetupContract
	r.WorkOrderUpdates, r.NewJobs, r.NewWorkOrders, r.SupersedeWorkOrderIDs = nil, nil, nil, nil
	r.PriorReviewRound, r.ResultingReviewRound, r.ReviewRoundSeats = 0, 0, nil
	effects, err := PlanPlanStateChange(task, orders, *r.Policy.PlanState)
	if err != nil {
		return r, err
	}
	r.ReviewTransition = "plan_state"
	r.NextStage = task.NextStage
	if effects.Command != "" {
		r.NextStage = effects.NextStage
	}
	r.PlanStateEffects = &effects
	return r, nil
}
