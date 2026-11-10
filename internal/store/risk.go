package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// RiskRatingStore records and reads a task's risk ratings
// (req-risk-based-review AC-5.5, AC-5.6, REQ-6; component-risk-review, State
// and failure model; feature-risk-based-review RR-3, RR-8). Every backend
// writes a rating, its activity event, and its uniqueness check in one
// transaction under the task's operation lock. Ratings are append-only. A
// reviewer rating is written only by AcceptReviewDecisionCommand, in the
// verdict's own transaction.
type RiskRatingStore interface {
	// RecordRiskRating records a path or plan rating once per key. A rating
	// whose key is already recorded returns the recorded rating and false and
	// writes nothing, so a replayed head reuses its path rating. A task whose
	// frozen risk mode is off or absent is refused with nothing recorded.
	RecordRiskRating(ctx context.Context, rating core.RiskRating) (core.RiskRating, bool, error)
	// RaiseTaskRiskTier records a person's raise. Under the task lock it reads
	// the task's current tier, refuses a tier at or below it with
	// *core.RiskTierNotRaisedError naming it, and otherwise records the raise
	// with the previous tier and its activity event.
	RaiseTaskRiskTier(ctx context.Context, raise core.RiskRating) (core.RiskRating, error)
	// ListTaskRiskRatings returns a task's ratings in recorded order.
	ListTaskRiskRatings(ctx context.Context, taskID string) ([]core.RiskRating, error)
}

// ErrRiskNotEnabled refuses a rating for a task whose frozen risk mode is off
// or absent (req-risk-based-review AC-1.1).
var ErrRiskNotEnabled = errors.New("the task's frozen risk policy is off; the task is not rated")

// PrepareRiskRating validates a path or plan rating against the task and its
// recorded ratings. It returns the recorded rating and false when the key
// exists, and otherwise the rating to insert, numbered after the recorded
// ones.
func PrepareRiskRating(task core.Task, existing []core.RiskRating, rating core.RiskRating, now time.Time) (core.RiskRating, bool, error) {
	if rating.Source != core.RiskSourcePathRule && rating.Source != core.RiskSourcePlan {
		return core.RiskRating{}, false, fmt.Errorf("risk rating source %q is not recorded through RecordRiskRating", rating.Source)
	}
	if !task.SetupContract.Risk.Enabled() {
		return core.RiskRating{}, false, ErrRiskNotEnabled
	}
	rating.TaskID = task.ID
	if err := rating.Validate(); err != nil {
		return core.RiskRating{}, false, err
	}
	for _, recorded := range existing {
		if recorded.Key == rating.Key {
			return recorded, false, nil
		}
	}
	return numberRiskRating(existing, rating, now), true, nil
}

// PrepareRiskRaise validates a person's raise against the task's recorded
// ratings. The current tier of a task with no rating is low, so a raise must
// name medium or high (req-risk-based-review AC-6.1, AC-6.2;
// component-risk-review invariant 4).
func PrepareRiskRaise(task core.Task, existing []core.RiskRating, raise core.RiskRating, now time.Time) (core.RiskRating, error) {
	if !task.SetupContract.Risk.Enabled() {
		return core.RiskRating{}, ErrRiskNotEnabled
	}
	if !raise.Tier.Valid() {
		return core.RiskRating{}, errors.New("risk tier must be low, medium, or high")
	}
	if err := core.ValidateRiskReason("reason", raise.Reason); err != nil {
		return core.RiskRating{}, err
	}
	current := riskpolicy.TierLow
	if setter, ok := core.RiskTierOf(existing); ok {
		current = setter.Tier
	}
	if raise.Tier.Rank() <= current.Rank() {
		return core.RiskRating{}, &core.RiskTierNotRaisedError{Current: current}
	}
	raise.TaskID, raise.Source, raise.Key, raise.PreviousTier = task.ID, core.RiskSourcePerson, core.PersonRiskKey(raise.Tier), current
	raise.MatchedRules = []riskpolicy.RuleMatch{}
	if err := raise.Validate(); err != nil {
		return core.RiskRating{}, err
	}
	for _, recorded := range existing {
		if recorded.Key == raise.Key {
			// Unreachable while tiers only rise; kept so a corrupted ledger
			// never records a second raise to one tier.
			return core.RiskRating{}, &core.RiskTierNotRaisedError{Current: current}
		}
	}
	return numberRiskRating(existing, raise, now), nil
}

// ReviewerRiskRating returns the reviewer rating a verdict records, and false
// when the verdict states no tier. Acceptance calls it only for a decision not
// yet completed; the head, seat, and model come from the claimed review order,
// never from the verdict (req-risk-based-review AC-5.3).
func ReviewerRiskRating(task core.Task, existing []core.RiskRating, decision core.ReviewDecision, now time.Time) (core.RiskRating, bool, error) {
	if decision.RiskTier == "" {
		return core.RiskRating{}, false, nil
	}
	if !task.SetupContract.Risk.Enabled() {
		return core.RiskRating{}, false, ErrRiskNotEnabled
	}
	if err := core.ValidateStatedRisk("risk_tier", "risk_reason", decision.RiskTier, decision.RiskReason); err != nil {
		return core.RiskRating{}, false, Refuse(err)
	}
	head := decision.HeadSHA
	if head == "" {
		head = decision.ReviewedCommitSHA
	}
	rating := core.RiskRating{
		TaskID: task.ID, Key: core.ReviewerRiskKey(decision.ReviewWorkOrderID), Source: core.RiskSourceReviewer,
		Tier: riskpolicy.Tier(decision.RiskTier), HeadSHA: head, Reason: decision.RiskReason,
		MatchedRules: []riskpolicy.RuleMatch{}, ReviewWorkOrderID: decision.ReviewWorkOrderID,
		ReviewRound: decision.ReviewRound, ReviewSeat: decision.ReviewSeat, ReviewerModel: decision.ReviewerModel,
	}
	if err := rating.Validate(); err != nil {
		return core.RiskRating{}, false, err
	}
	for _, recorded := range existing {
		if recorded.Key == rating.Key {
			return recorded, false, nil
		}
	}
	return numberRiskRating(existing, rating, now), true, nil
}

func numberRiskRating(existing []core.RiskRating, rating core.RiskRating, now time.Time) core.RiskRating {
	seq := 0
	for _, recorded := range existing {
		seq = max(seq, recorded.Seq)
	}
	rating.Seq = seq + 1
	rating.CreatedAt = now.UTC()
	if rating.MatchedRules == nil {
		rating.MatchedRules = []riskpolicy.RuleMatch{}
	}
	return rating
}

// RiskRatedEvent is the activity event of one recorded rating. The event
// actor is the request's authenticated actor, and a raise's payload also names
// the person (req-risk-based-review AC-6.1).
func RiskRatedEvent(rating core.RiskRating) core.Event {
	return core.Event{TaskID: rating.TaskID, Kind: core.RiskRatedEvent, Payload: core.JSONPayload(core.RiskRatedPayload(rating))}
}

// SortRiskRatings orders ratings by their recorded sequence.
func SortRiskRatings(ratings []core.RiskRating) {
	slices.SortFunc(ratings, func(a, b core.RiskRating) int { return a.Seq - b.Seq })
}

func cloneRiskRating(rating core.RiskRating) core.RiskRating {
	rating.MatchedRules = slices.Clone(rating.MatchedRules)
	if rating.MatchedRules == nil {
		rating.MatchedRules = []riskpolicy.RuleMatch{}
	}
	return rating
}

func (m *memory) riskTaskLocked(ctx context.Context, taskID string) (core.Task, memoryScopedKey, error) {
	workspace, _ := WorkspaceFromContext(ctx)
	task, ok := m.tasks[taskID]
	if !ok || (task.Workspace != "" && task.Workspace != workspace) {
		return core.Task{}, memoryScopedKey{}, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	return task, memoryScopedKey{workspace: workspace, id: taskID}, nil
}

func (m *memory) RecordRiskRating(ctx context.Context, rating core.RiskRating) (core.RiskRating, bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.RiskRating{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, key, err := m.riskTaskLocked(ctx, rating.TaskID)
	if err != nil {
		return core.RiskRating{}, false, err
	}
	prepared, created, err := PrepareRiskRating(task, m.riskRatings[key], rating, time.Now())
	if err != nil || !created {
		return cloneRiskRating(prepared), false, err
	}
	m.insertRiskRatingLocked(ctx, key, prepared)
	return cloneRiskRating(prepared), true, nil
}

func (m *memory) RaiseTaskRiskTier(ctx context.Context, raise core.RiskRating) (core.RiskRating, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.RiskRating{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, key, err := m.riskTaskLocked(ctx, raise.TaskID)
	if err != nil {
		return core.RiskRating{}, err
	}
	prepared, err := PrepareRiskRaise(task, m.riskRatings[key], raise, time.Now())
	if err != nil {
		return core.RiskRating{}, err
	}
	m.insertRiskRatingLocked(ctx, key, prepared)
	return cloneRiskRating(prepared), nil
}

func (m *memory) ListTaskRiskRatings(ctx context.Context, taskID string) ([]core.RiskRating, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, key, err := m.riskTaskLocked(ctx, taskID)
	if err != nil {
		return nil, err
	}
	ratings := make([]core.RiskRating, 0, len(m.riskRatings[key]))
	for _, rating := range m.riskRatings[key] {
		ratings = append(ratings, cloneRiskRating(rating))
	}
	SortRiskRatings(ratings)
	return ratings, nil
}

// recordReviewerRiskLocked records a verdict's reviewer rating inside the
// memory verdict acceptance.
func (m *memory) recordReviewerRiskLocked(ctx context.Context, task core.Task, decision core.ReviewDecision) error {
	workspace, _ := WorkspaceFromContext(ctx)
	key := memoryScopedKey{workspace: workspace, id: task.ID}
	rating, created, err := ReviewerRiskRating(task, m.riskRatings[key], decision, time.Now())
	if err != nil || !created {
		return err
	}
	m.insertRiskRatingLocked(ctx, key, rating)
	return nil
}

func (m *memory) insertRiskRatingLocked(ctx context.Context, key memoryScopedKey, rating core.RiskRating) {
	if m.riskRatings == nil {
		m.riskRatings = map[memoryScopedKey][]core.RiskRating{}
	}
	m.riskRatings[key] = append(m.riskRatings[key], cloneRiskRating(rating))
	m.appendEventLocked(ctx, RiskRatedEvent(rating))
}

// ProjectTaskRisk builds the task read field risk from a task, its recorded
// ratings, and its frozen seat minimum. It returns nil for a task whose frozen
// risk mode is off or absent (req-risk-based-review AC-1.1, REQ-11). The tier
// is the largest recorded rating, and a task with no rating states no tier
// rather than an invented one. The head is the latest rated path head, or the
// task's reviewed head before any head is rated.
func ProjectTaskRisk(task core.Task, ratings []core.RiskRating, seatMinimum int) *core.TaskRisk {
	policy := task.SetupContract.Risk
	if !policy.Enabled() {
		return nil
	}
	SortRiskRatings(ratings)
	projection := &core.TaskRisk{Mode: policy.Mode, Head: task.ReviewedHeadSHA, Ratings: make([]core.RiskRating, 0, len(ratings))}
	pathApprovals := 0
	for _, rating := range ratings {
		projection.Ratings = append(projection.Ratings, cloneRiskRating(rating))
		if rating.Source == core.RiskSourcePathRule {
			projection.Head, pathApprovals = rating.HeadSHA, rating.HumanApprovals
		}
	}
	tier := riskpolicy.Tier("")
	if setter, ok := core.RiskTierOf(ratings); ok {
		tier = setter.Tier
		projection.Tier, projection.Source, projection.Reason = setter.Tier, setter.Source, setter.Reason
	}
	inputTier := tier
	if inputTier == "" {
		inputTier = riskpolicy.TierLow
	}
	floors := riskpolicy.Floors{SeatMinimum: seatMinimum, MergeGate: task.MergeApproval}
	// Ratings change no seat, gate, or merge decision yet, under record and
	// enforce alike, so the requirements in effect are the frozen floors
	// (req-risk-based-review AC-1.6, REQ-2). The calculated counts show what
	// the tier and matched path rules would require.
	inEffect := riskpolicy.CalculateRequirements(riskpolicy.RequirementInput{Floors: floors}).Effective
	calculated := riskpolicy.CalculateRequirements(riskpolicy.RequirementInput{
		Policy: policy, Tier: inputTier, PathHumanApprovals: pathApprovals, Floors: floors,
	}).Calculated
	projection.RequiredAgentReviews, projection.RequiredHumanApprovals = inEffect.AgentReviews, inEffect.HumanApprovals
	projection.CalculatedAgentReviews, projection.CalculatedHumanApprovals = calculated.AgentReviews, calculated.HumanApprovals
	return projection
}
