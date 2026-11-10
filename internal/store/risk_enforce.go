package store

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// Risk enforcement (req-risk-based-review REQ-7, REQ-8, AC-7.1, AC-7.2,
// AC-8.1, AC-8.2, AC-8.6; feature-risk-based-review RR-5, RR-6). Every
// backend reads a task's ratings inside the transaction that sizes a round,
// adds seats, or chooses the acceptance edge, and passes them here. Under the
// frozen modes off and record nothing in this file changes a seat, a gate, or
// a merge (AC-1.1, AC-1.6).

// riskRequirementInput builds the requirement inputs over a task's ratings:
// the tier the ratings set (low before any rating) and the human approval
// minimum of the latest path rating.
func riskRequirementInput(task core.Task, ratings []core.RiskRating, seatMinimum int) (riskpolicy.RequirementInput, riskpolicy.Tier) {
	sorted := slices.Clone(ratings)
	SortRiskRatings(sorted)
	pathApprovals := 0
	for _, rating := range sorted {
		if rating.Source == core.RiskSourcePathRule {
			pathApprovals = rating.HumanApprovals
		}
	}
	tier := riskpolicy.TierLow
	if setter, ok := core.RiskTierOf(sorted); ok {
		tier = setter.Tier
	}
	return riskpolicy.RequirementInput{
		Policy: task.SetupContract.Risk, Tier: tier, PathHumanApprovals: pathApprovals,
		Floors: riskpolicy.Floors{SeatMinimum: seatMinimum, MergeGate: task.MergeApproval},
	}, tier
}

// EnforcedRiskRequirements returns what an enforcing task's tier requires: the
// tier's agent reviews and the distinct human approvals req-risk-based-review
// REQ-8 requires, the largest of the tier's count, any matched path rule's
// min_human_approvals, and one when the frozen merge gate is on. ok is false
// for a task whose frozen mode is not enforce. The calculation is
// internal/riskpolicy's (component-risk-review, Boundary). Approvals from a
// trusted author (REQ-9) are not inputs yet.
func EnforcedRiskRequirements(task core.Task, ratings []core.RiskRating) (riskpolicy.Requirements, riskpolicy.Tier, bool) {
	if !task.SetupContract.Risk.Enforcing() {
		return riskpolicy.Requirements{}, "", false
	}
	in, tier := riskRequirementInput(task, ratings, 0)
	return riskpolicy.CalculateRequirements(in).Effective, tier, true
}

// RiskSeatCount is the agent reviews an enforcing task's tier requires, the
// seat floor input of req-risk-based-review REQ-7, and 0 for any other task.
func RiskSeatCount(task core.Task, ratings []core.RiskRating) int {
	required, _, ok := EnforcedRiskRequirements(task, ratings)
	if !ok {
		return 0
	}
	return required.AgentReviews
}

// RiskMergeHold returns the human approvals an enforcing task requires and
// whether that count holds an approved review at the merge gate: one or more
// waits there, and zero merges as req-review-gates-evidence AC-1.1 states
// (req-risk-based-review AC-8.1, AC-8.2). hold is false for every task whose
// frozen mode is not enforce.
func RiskMergeHold(task core.Task, ratings []core.RiskRating) (required int, hold bool) {
	requirements, _, ok := EnforcedRiskRequirements(task, ratings)
	if !ok {
		return 0, false
	}
	return requirements.HumanApprovals, requirements.HumanApprovals >= 1
}

// ErrRiskApprovalsRequired refuses a merge that an enforcing task's required
// human approvals do not allow: the automatic merge at any required count of
// one or more, and the single operator merge at a required count of two or
// more until counted approvals exist (req-risk-based-review AC-8.2, AC-8.7).
// The refusal records no approval and lands nothing.
var ErrRiskApprovalsRequired = errors.New("risk_approvals_required")

// RiskApprovalsRequiredError names the required count of a refused merge.
type RiskApprovalsRequiredError struct {
	Required int
	Tier     riskpolicy.Tier
	// Automatic is true for the factory's automatic merge.
	Automatic bool
}

func (e *RiskApprovalsRequiredError) Error() string {
	approvals := "human approvals"
	if e.Required == 1 {
		approvals = "human approval"
	}
	if e.Automatic {
		return fmt.Sprintf("the risk policy requires %d %s for this %s-tier task, so it waits for a person at the merge gate and does not merge automatically", e.Required, approvals, e.Tier)
	}
	return fmt.Sprintf("the risk policy requires %d %s for this %s-tier task; one person's merge cannot land it until approvals are counted, so nothing was merged or recorded", e.Required, approvals, e.Tier)
}

// Is matches ErrRiskApprovalsRequired and ErrRefusal, so every route answers
// the refusal with its text instead of a store failure.
func (e *RiskApprovalsRequiredError) Is(target error) bool {
	return target == ErrRiskApprovalsRequired || target == ErrRefusal
}

// RiskMergeRefusal returns the refusal for a merge of task under its ratings,
// or nil when the merge may go on. automatic is the factory's merge with the
// merge gate off; any required count of one or more refuses it. An operator's
// single merge is refused at a required count of two or more. A task whose
// frozen mode is not enforce is never refused here.
func RiskMergeRefusal(task core.Task, ratings []core.RiskRating, automatic bool) error {
	requirements, tier, ok := EnforcedRiskRequirements(task, ratings)
	if !ok {
		return nil
	}
	limit := 2
	if automatic {
		limit = 1
	}
	if requirements.HumanApprovals < limit {
		return nil
	}
	return &RiskApprovalsRequiredError{Required: requirements.HumanApprovals, Tier: tier, Automatic: automatic}
}

// RiskSeatTrigger names the act whose raised tier adds seats: a review seat's
// verdict or a person's raise. RequestID is the deterministic ledger key of
// the addition, so a replayed act never adds a second set of seats.
type RiskSeatTrigger struct {
	RequestID string
	Reason    string
	ActorID   string
	// Round is the round the act concerns; 0 means the task's latest round.
	Round int
}

// VerdictRiskSeatTrigger is the trigger of an accepted verdict.
func VerdictRiskSeatTrigger(decision core.ReviewDecision) RiskSeatTrigger {
	return RiskSeatTrigger{
		RequestID: "risk:" + decision.ReviewWorkOrderID,
		Reason:    fmt.Sprintf("review seat %d of round %d rated the task %s", decision.ReviewSeat, decision.ReviewRound, decision.RiskTier),
		ActorID:   ReviewRoundActorID(decision.ReviewRound), Round: decision.ReviewRound,
	}
}

// PersonRiskSeatTrigger is the trigger of a person's recorded raise.
func PersonRiskSeatTrigger(raise core.RiskRating) RiskSeatTrigger {
	return RiskSeatTrigger{
		RequestID: "risk:" + raise.TaskID + ":" + raise.Key,
		Reason:    fmt.Sprintf("a person raised the task to %s: %s", raise.Tier, raise.Reason),
		ActorID:   raise.ActorID,
	}
}

// RiskSeatAdditionInputs are the task state a backend reads inside the act
// that raised the tier, under the same task lock.
type RiskSeatAdditionInputs struct {
	Workspace string
	Task      core.Task
	// Orders are the task's review-stage orders.
	Orders               []core.WorkOrder
	Events               []core.Event
	Records              []ReviewRoundSeats
	Ratings              []core.RiskRating
	Trigger              RiskSeatTrigger
	WorkspaceReviewSeats int
	QueueTimeout         time.Duration
	Now                  time.Time
}

// PlanRiskSeatAddition plans the seats an enforcing task's raised tier adds to
// its open review round (req-risk-based-review AC-7.2;
// feature-risk-based-review RR-5). It returns false with no plan when the task
// does not enforce, has no open round, the act concerns an older round, the
// round settled or a policy handoff superseded it, or the round already holds
// the tier's count. The added seats are ordinary seats of that round, built by
// the same planner as an operator addition, and no seat is removed. It is
// pure.
func PlanRiskSeatAddition(in RiskSeatAdditionInputs) (ReviewSeatAdditionPlan, bool, error) {
	task := in.Task
	required := RiskSeatCount(task, in.Ratings)
	if required == 0 || core.TaskTerminal(task.State) {
		return ReviewSeatAdditionPlan{}, false, nil
	}
	round := 0
	for _, order := range in.Orders {
		if order.TaskID == task.ID && order.Stage == core.StageReview && order.ReviewRound > round {
			round = order.ReviewRound
		}
	}
	if round < 1 || (in.Trigger.Round != 0 && in.Trigger.Round != round) || reviewRoundSettled(in.Events, round) {
		return ReviewSeatAdditionPlan{}, false, nil
	}
	superseded := SupersededReviewWorkOrders(in.Events)
	members := 0
	for _, order := range in.Orders {
		if order.TaskID != task.ID || order.Stage != core.StageReview || order.ReviewRound != round {
			continue
		}
		if superseded[order.ID] {
			return ReviewSeatAdditionPlan{}, false, nil
		}
		members++
	}
	if members >= required {
		return ReviewSeatAdditionPlan{}, false, nil
	}
	plan, err := PlanReviewSeatAddition(ReviewSeatAdditionInputs{
		Workspace: in.Workspace, Task: task, Orders: in.Orders, Events: in.Events, Records: in.Records,
		Request: ReviewSeatAdditionRequest{
			TaskID: task.ID, RequestID: in.Trigger.RequestID, Reason: in.Trigger.Reason, Add: required - members,
			WorkspaceReviewSeats: in.WorkspaceReviewSeats, QueueTimeout: in.QueueTimeout,
		},
		ActorID: in.Trigger.ActorID, Now: in.Now,
	})
	if err != nil {
		return ReviewSeatAdditionPlan{}, false, err
	}
	// The tier now sets the round's seat count (AC-7.1).
	plan.Record.SeatCountSource = ReviewSeatSourceRisk
	_, tier, _ := EnforcedRiskRequirements(task, in.Ratings)
	plan.Payload["source"], plan.Payload["risk_tier"] = ReviewSeatSourceRisk, string(tier)
	return plan, true, nil
}
