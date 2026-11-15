package store

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"gopkg.in/yaml.v3"
)

// Risk enforcement (req-risk-based-review REQ-7, REQ-8, AC-7.1, AC-7.2,
// AC-8.1, AC-8.2, AC-8.6, AC-9.2; component-review invariants 1, 21, and 42;
// component-risk-review invariants 16 and 19; component-task-lifecycle
// invariant 5). Every backend reads a task's ratings inside the transaction
// that sizes a round, adds seats, or chooses the acceptance edge, and passes
// them here. The acceptance edge also reads the task's authors and the live
// trust state in that transaction (RiskAcceptanceReads). Under the
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
// internal/riskpolicy's (component-risk-review, Boundary). The one approval an
// untrusted author makes necessary depends on the task's authors and the live
// trust state, so it is not an input here: RiskAcceptanceHold adds it at
// acceptance and EvaluateTaskApprovals adds it at every approval evaluation.
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

// RiskMergeHold returns the human approvals an enforcing task's tier, latest
// path rating, and frozen merge gate require, and whether that count alone
// holds an approved review at the merge gate: one or more waits there, and
// zero merges as req-review-gates-evidence AC-1.1 states
// (req-risk-based-review AC-8.1, AC-8.2). It reads no author, so the
// acceptance edge uses RiskAcceptanceHold. hold is false for every task whose
// frozen mode is not enforce.
func RiskMergeHold(task core.Task, ratings []core.RiskRating) (required int, hold bool) {
	requirements, _, ok := EnforcedRiskRequirements(task, ratings)
	if !ok {
		return 0, false
	}
	return requirements.HumanApprovals, requirements.HumanApprovals >= 1
}

// RiskAcceptanceReads are the reads the acceptance edge of an enforcing task
// makes inside the backend's serialized acceptance operation, in the same
// transaction or under the same lock that settles the round. Each read
// answers the state current at that call, and nothing caches an answer, so an
// edit of the trust list or of a person's role before acceptance changes the
// edge (component-risk-review invariant 19; req-risk-based-review AC-9.3).
// A read error fails the acceptance, which then records nothing.
type RiskAcceptanceReads struct {
	// Claims returns the task's work_order.claimed events and its current
	// implementation orders, the inputs of TaskAuthors.
	Claims func() ([]core.Event, []core.WorkOrder, error)
	// TrustedAuthors returns the task repository's live trusted_authors
	// list. An empty list trusts every person who holds operate_gates.
	TrustedAuthors func() ([]string, error)
	// OperateGates answers whether a person holds operate_gates in the task's
	// workspace now.
	OperateGates func(userID string) (bool, error)
}

// RiskAcceptanceHold returns the human approvals an enforcing task requires
// when its approved review round settles, and whether that count holds the
// round at the merge gate. The count is the one component-risk-review
// invariant 16 states: the largest of the tier's count, the latest path
// rating's minimum, one when the frozen merge gate is on, and one when any
// author is not trusted. An author whose claim names no person is never
// trusted (invariant 19). One or more takes gate.merge, a refresh round
// included, even when recorded approvals already meet it (component-review
// invariant 21; component-task-lifecycle invariant 5; req-risk-based-review
// AC-8.2, AC-9.2). A task whose frozen mode is not enforce makes no read and
// is never held (AC-1.1, AC-1.6). The author reads run only when the other
// floors require no approval, because they can only add the one approval.
func RiskAcceptanceHold(task core.Task, ratings []core.RiskRating, reads RiskAcceptanceReads) (required int, hold bool, err error) {
	required, hold = RiskMergeHold(task, ratings)
	if !task.SetupContract.Risk.Enforcing() || hold {
		return required, hold, nil
	}
	events, orders, err := reads.Claims()
	if err != nil {
		return 0, false, fmt.Errorf("read the task's authors: %w", err)
	}
	authors, unknown := TaskAuthors(events, orders)
	if len(authors) == 0 && !unknown {
		return required, hold, nil
	}
	trusted, err := reads.TrustedAuthors()
	if err != nil {
		return 0, false, fmt.Errorf("read the repository's trusted authors: %w", err)
	}
	gatekeepers := map[string]bool{}
	if len(trusted) == 0 {
		// An empty list trusts every person who holds operate_gates now
		// (req-risk-based-review AC-9.1).
		for _, author := range authors {
			allowed, err := reads.OperateGates(author)
			if err != nil {
				return 0, false, fmt.Errorf("read an author's operate_gates capability: %w", err)
			}
			gatekeepers[author] = allowed
		}
	}
	status := riskpolicy.EvaluateApprovals(riskpolicy.ApprovalInput{
		Required: required, Authors: authors, UnknownAuthor: unknown, TrustedAuthors: trusted, Gatekeepers: gatekeepers,
	})
	return status.Required, status.Required >= 1, nil
}

// StoredTrustedAuthors reads one repository's trusted_authors list from a
// stored workspace configuration document. Empty data, or a document that
// does not name the repository, is the empty list. A document that cannot be
// parsed is an error, so the caller never assumes trust.
func StoredTrustedAuthors(data []byte, repo string) ([]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var document struct {
		Repos []struct {
			Name           string   `yaml:"name"`
			TrustedAuthors []string `yaml:"trusted_authors"`
		} `yaml:"repos"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode stored workspace config: %w", err)
	}
	for _, stored := range document.Repos {
		if stored.Name == repo {
			return slices.Clone(stored.TrustedAuthors), nil
		}
	}
	return nil, nil
}

// ErrRiskApprovalsRequired refuses the factory's automatic merge of an
// enforcing task that requires one or more human approvals, an untrusted
// author's included (req-risk-based-review AC-8.2, AC-9.2). The refusal
// records no approval and lands nothing. A person's merge counts approvals
// instead (AC-8.7).
var ErrRiskApprovalsRequired = errors.New("risk_approvals_required")

// RiskApprovalsRequiredError names the required count of a refused automatic
// merge.
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
	return fmt.Sprintf("the risk policy requires %d %s for this %s-tier task, so it waits for a person at the merge gate and does not merge automatically", e.Required, approvals, e.Tier)
}

// Is matches ErrRiskApprovalsRequired and ErrRefusal, so every route answers
// the refusal with its text instead of a store failure.
func (e *RiskApprovalsRequiredError) Is(target error) bool {
	return target == ErrRiskApprovalsRequired || target == ErrRefusal
}

// RiskSeatTrigger names the act whose raised tier adds seats: a person's
// raise. A review verdict states no tier and adds no seat
// (component-review invariant 13). RequestID is the deterministic ledger key of
// the addition, so a replayed act never adds a second set of seats.
type RiskSeatTrigger struct {
	RequestID string
	Reason    string
	ActorID   string
	// Round is the round the act concerns; 0 means the task's latest round.
	Round int
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
// component-review invariant 42). It returns false with no plan when the task
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
