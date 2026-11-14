package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Head risk orders (feature-risk-based-review RR-12, RR-15;
// component-risk-review invariants 12 and 13; req-risk-based-review AC-1.6,
// AC-5.8 to AC-5.11, AC-12.3). Submission and refresh ask the need decision
// after they record the head's path minimums, and the act that hands the head
// to review creates its risk order and cancels open risk orders of earlier
// heads in its own transaction. Every backend plans the act with
// PlanHeadRiskOrder from state it read under the task lock, so the decision
// never uses a task value read before the path rating.

// HeadRiskOrderRequest asks for the risk order of one submitted or refreshed
// head. Subject names the head with its observed base commit, never a branch
// name. ChangedPaths is the complete changed-path list of the head, and
// MatchedRules are the path rules its path rating matched.
type HeadRiskOrderRequest struct {
	TaskID       string
	Subject      core.RiskOrderSubject
	ChangedPaths []string
	MatchedRules []riskpolicy.RuleMatch
	QueueTimeout time.Duration
	Now          time.Time
}

// Validate refuses a request that names no head subject.
func (r HeadRiskOrderRequest) Validate() error {
	if err := r.createRequest().Validate(); err != nil {
		return err
	}
	if r.Subject.Kind() != riskpolicy.RiskSubjectHead {
		return Refusef("a head risk order rates a head with its base")
	}
	return nil
}

func (r HeadRiskOrderRequest) createRequest() RiskOrderCreateRequest {
	return RiskOrderCreateRequest{TaskID: r.TaskID, Subject: r.Subject, ChangedPaths: slices.Clone(r.ChangedPaths),
		MatchedRules: cloneRuleMatches(r.MatchedRules), QueueTimeout: r.QueueTimeout, Now: r.Now}
}

// HeadRiskOrderPlan is what one head handoff writes for risk: the open risk
// orders of other heads to cancel, and the creation request when the head
// needs a risk order.
type HeadRiskOrderPlan struct {
	CancelIDs []string
	Create    *RiskOrderCreateRequest
}

// RiskEarlierHeadCancelReason is the cancellation reason of an open risk order
// whose head a later head replaced (req-risk-based-review AC-5.11).
const RiskEarlierHeadCancelReason = "a later head was submitted for review"

// PlanHeadRiskOrder decides a head handoff's risk effects from the task, its
// counted ratings, and its risk orders, all read under the task lock. A task
// whose frozen mode is off plans nothing. Every open risk order of another
// head is cancelled, whatever the need decision answers (AC-5.11). The head
// gets a risk order when riskpolicy.NeedsRiskOrder answers yes for the frozen
// mode, the tier as it stands, the head's changed paths, and the paths of the
// task's latest completed head risk order (component-risk-review invariant 12;
// AC-5.8, AC-5.9, AC-12.3). A plan rating, an open order, and a reviewer
// rating never count as completed head coverage. A head whose risk order
// already exists replays that order through PlanRiskOrderCreate.
func PlanHeadRiskOrder(task core.Task, counted []core.RiskRating, orders []core.RiskOrder, request HeadRiskOrderRequest) (HeadRiskOrderPlan, error) {
	if !task.SetupContract.Risk.Enabled() {
		return HeadRiskOrderPlan{}, nil
	}
	if err := request.Validate(); err != nil {
		return HeadRiskOrderPlan{}, err
	}
	plan := HeadRiskOrderPlan{CancelIDs: []string{}}
	key := request.Subject.Key()
	for _, order := range orders {
		if order.Open() && order.Subject.Kind() == riskpolicy.RiskSubjectHead && order.Subject.Key() != key {
			plan.CancelIDs = append(plan.CancelIDs, order.Order.ID)
		}
	}
	tier := riskpolicy.Tier("")
	if setter, ok := core.RiskTierOf(counted); ok {
		tier = setter.Tier
	}
	need := riskpolicy.NeedsRiskOrder(riskpolicy.RiskOrderNeedInput{
		Mode: task.SetupContract.Risk.Mode, Tier: tier, SubjectKind: riskpolicy.RiskSubjectHead,
		ChangedPaths: request.ChangedPaths, LatestCompletedHeadPaths: LatestCompletedHeadRiskPaths(orders),
	})
	if need {
		create := request.createRequest()
		plan.Create = &create
	}
	return plan, nil
}

// HeadRiskOrderFor returns the task's risk order for one head, whatever its
// base, newest first. A submission replay names its order with it.
func HeadRiskOrderFor(orders []core.RiskOrder, head string) (core.RiskOrder, bool) {
	for i := len(orders) - 1; i >= 0; i-- {
		if head != "" && orders[i].Subject.Kind() == riskpolicy.RiskSubjectHead && orders[i].Subject.HeadSHA == head {
			return orders[i], true
		}
	}
	return core.RiskOrder{}, false
}

// ErrReviewRoundHeldByRiskOrder reports that an enforcing task's review round
// waits for its head's risk order. A round builder that meets it writes
// nothing: no job, seat order, round record, or round event
// (req-risk-based-review AC-5.10; feature-risk-based-review RR-12).
var ErrReviewRoundHeldByRiskOrder = errors.New("review round waits for the head's risk order")

// RiskRoundHoldApplies reports whether a task's review rounds can wait for a
// risk order: only while its frozen mode is enforce. A caller tests it before
// it reads any risk order, so under record and off no round builder reads one
// (component-risk-review invariant 13; req-risk-based-review AC-1.6).
func RiskRoundHoldApplies(task core.Task) bool {
	return task.SetupContract.Risk.Enforcing()
}

// ReviewRoundRiskHold returns the risk order that holds the review round of
// head: a head risk order for exactly that head that is queued or claimed,
// including the stale and timed-out forms the order clock recovers to queued.
// A completed or cancelled order, a plan order, and an order of another head
// hold nothing. It answers false for a task whose frozen mode is not enforce
// (req-risk-based-review AC-5.10; feature-risk-based-review RR-12).
func ReviewRoundRiskHold(task core.Task, orders []core.RiskOrder, head string) (core.WorkOrder, bool) {
	if !RiskRoundHoldApplies(task) || head == "" {
		return core.WorkOrder{}, false
	}
	for _, order := range orders {
		if order.Open() && order.Subject.Kind() == riskpolicy.RiskSubjectHead && order.Subject.HeadSHA == head {
			return order.Order, true
		}
	}
	return core.WorkOrder{}, false
}

// SubmissionComparisonEventKind is the task event that records a submitted
// head with the base commit its pull request compares against.
const SubmissionComparisonEventKind = "pull_request.opened"

// RecordedSubmissionComparison returns the base and head commits of the
// latest recorded submission comparison in events, oldest first: the newest
// pull_request.opened event that names both commits. Review input reads and
// the round's head use this pair, never a mutable branch name.
func RecordedSubmissionComparison(events []core.Event) (base, head string, ok bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != SubmissionComparisonEventKind {
			continue
		}
		var target struct {
			Base string `json:"base_sha"`
			Head string `json:"head_sha"`
		}
		if json.Unmarshal(events[i].Payload, &target) == nil && target.Base != "" && target.Head != "" {
			return target.Base, target.Head, true
		}
	}
	return "", "", false
}

// ReviewRoundDispatchComparison returns the commit pair that the next review
// round of task reviews. While the approval is stale, the round reviews the
// refresh baseline and head. Otherwise, it reviews the latest recorded
// submission comparison in events, and recorded reports that one was found.
// Without one, the refresh pair stands. Dispatch builds the round from this
// pair, and durable reconcile asks the same question, so the two always name
// the same head (feature-risk-based-review RR-12).
func ReviewRoundDispatchComparison(task core.Task, events []core.Event) (baseline, head string, recorded bool) {
	if !task.ApprovalStale {
		if base, submitted, ok := RecordedSubmissionComparison(events); ok {
			return base, submitted, true
		}
	}
	return task.RefreshBaselineSHA, task.RefreshHeadSHA, false
}

// QueuedReviewRiskHold reports whether durable reconcile leaves a queued task
// alone because its next review round waits for that round's head risk
// order, and returns the order. It applies the hold that dispatch applies
// before it builds a round: ReviewRoundRiskHold for the head that
// ReviewRoundDispatchComparison names. It calls no reader unless the task's
// next stage is review and its frozen mode is enforce. It calls
// readComparisons only while the approval is fresh, and readRiskOrders only
// when a head is known. A read error is returned and never treated as no
// hold. A completed, cancelled, or missing order, a plan order, and an order
// of another head hold nothing (feature-risk-based-review RR-12, RR-13;
// req-risk-based-review AC-1.6, AC-5.10; component-risk-review invariant 13;
// component-durable-queue "Reconciliation between tasks and jobs").
func QueuedReviewRiskHold(task core.Task, readComparisons func() ([]core.Event, error), readRiskOrders func() ([]core.RiskOrder, error)) (core.WorkOrder, bool, error) {
	if task.NextStage != core.StageReview || !RiskRoundHoldApplies(task) {
		return core.WorkOrder{}, false, nil
	}
	var events []core.Event
	if !task.ApprovalStale {
		var err error
		if events, err = readComparisons(); err != nil {
			return core.WorkOrder{}, false, fmt.Errorf("read recorded review comparisons: %w", err)
		}
	}
	_, head, _ := ReviewRoundDispatchComparison(task, events)
	if strings.TrimSpace(head) == "" {
		return core.WorkOrder{}, false, nil
	}
	orders, err := readRiskOrders()
	if err != nil {
		return core.WorkOrder{}, false, fmt.Errorf("read risk orders: %w", err)
	}
	order, held := ReviewRoundRiskHold(task, orders, head)
	return order, held, nil
}

// ReviewRoundHead is the head a new round's seat orders review: the first
// order's head. Every seat of one round reviews the same head.
func ReviewRoundHead(orders []core.WorkOrder) string {
	for _, order := range orders {
		if order.HeadSHA != "" {
			return order.HeadSHA
		}
	}
	return ""
}

// ReviewRoundHeldError names the risk order a round waits for.
func ReviewRoundHeldError(order core.WorkOrder) error {
	return fmt.Errorf("%w: risk order %s for head %s is %s", ErrReviewRoundHeldByRiskOrder, order.ID, order.HeadSHA, order.State)
}

// RiskSeatCountAtSubmission is the agent reviews an enforcing task's tier
// required when its implement order at head was admitted: the tier of the
// ratings recorded up to and including that head's path rating, which the
// submission records before it admits a declared count again. Without that
// path rating every rating counts, so no declaration is treated as admitted
// below a floor it may have met (feature-risk-based-review RR-12;
// req-review-gates-evidence AC-2.3).
func RiskSeatCountAtSubmission(task core.Task, ratings []core.RiskRating, head string) int {
	if !task.SetupContract.Risk.Enforcing() {
		return 0
	}
	key := core.PathRiskKey(head)
	limit := 0
	for _, rating := range ratings {
		if head != "" && rating.Key == key {
			limit = rating.Seq
			break
		}
	}
	if limit == 0 {
		return RiskSeatCount(task, ratings)
	}
	admitted := make([]core.RiskRating, 0, len(ratings))
	for _, rating := range ratings {
		if rating.Seq <= limit {
			admitted = append(admitted, rating)
		}
	}
	return RiskSeatCount(task, admitted)
}

// ApplyRiskSeatInputs sets the tier's agent reviews on a round's seat inputs:
// the count as it stands, and the count when the latest implement order's
// declaration was admitted. It reads ratings only for an enforcing task.
func ApplyRiskSeatInputs(in *ReviewRoundSeatInputs, task core.Task, ratings []core.RiskRating) {
	if !task.SetupContract.Risk.Enforcing() {
		in.RiskSeatCount, in.DeclaredRiskSeatCount = 0, 0
		return
	}
	in.RiskSeatCount = RiskSeatCount(task, ratings)
	in.DeclaredRiskSeatCount = RiskSeatCountAtSubmission(task, ratings, in.ImplementationHeadSHA)
}

// HeadRiskOrderStore applies a refreshed head's risk effects as one act
// (feature-risk-based-review RR-12). Submission applies the same effects
// inside SubmitImplementationCommand through ImplementationSubmission.HeadRisk.
type HeadRiskOrderStore interface {
	// EnsureHeadRiskOrderCommand cancels the task's open risk orders of other
	// heads and creates the head's risk order when PlanHeadRiskOrder answers
	// that it needs one, in one transaction under the task lock. The lease is
	// bound to core.WorkOrderCmdCreate. A replay returns the head's existing
	// order with Created false and cancels nothing again.
	EnsureHeadRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request HeadRiskOrderRequest) (HeadRiskOrderResult, error)
}

// HeadRiskOrderResult is what a head handoff wrote for risk. Order is the
// head's risk order, created now or earlier, and the zero order when the head
// needs none.
type HeadRiskOrderResult struct {
	Order     core.WorkOrder
	Created   bool
	Cancelled []string
}

// applyHeadRiskOrderLocked applies a head handoff's risk effects under m.mu,
// from state read under the same lock. A sibling act calls it inside its own
// critical section.
func (m *memory) applyHeadRiskOrderLocked(ctx context.Context, task core.Task, request HeadRiskOrderRequest) (HeadRiskOrderResult, error) {
	workspace, _ := WorkspaceFromContext(ctx)
	plan, err := m.planHeadRiskOrderLocked(ctx, task, request)
	if err != nil {
		return HeadRiskOrderResult{}, err
	}
	now := request.Now.UTC()
	if request.Now.IsZero() {
		now = time.Now().UTC()
	}
	result := HeadRiskOrderResult{Cancelled: []string{}}
	for _, id := range plan.CancelIDs {
		result.Cancelled = append(result.Cancelled, m.cancelRiskOrdersLocked(ctx, task.ID, id, RiskEarlierHeadCancelReason, now)...)
	}
	if plan.Create != nil {
		if result.Order, result.Created, err = m.createRiskOrderLocked(ctx, task, *plan.Create); err != nil {
			return HeadRiskOrderResult{}, err
		}
	} else if existing, ok := HeadRiskOrderFor(m.taskRiskOrdersLocked(workspace, task.ID), request.Subject.HeadSHA); ok && existing.Subject.Key() == request.Subject.Key() {
		result.Order = existing.Order
	}
	return result, nil
}

// planHeadRiskOrderLocked plans a head handoff's risk effects and checks the
// creation before the caller writes anything, so the memory backend keeps the
// all-or-nothing result of the SQL transactions.
func (m *memory) planHeadRiskOrderLocked(ctx context.Context, task core.Task, request HeadRiskOrderRequest) (HeadRiskOrderPlan, error) {
	workspace, _ := WorkspaceFromContext(ctx)
	ratings := m.riskRatingsLocked(ctx, task)
	orders := m.taskRiskOrdersLocked(workspace, task.ID)
	plan, err := PlanHeadRiskOrder(task, ratings, orders, request)
	if err != nil || plan.Create == nil {
		return plan, err
	}
	created, err := PlanRiskOrderCreate(task, ratings, orders, *plan.Create)
	if err != nil {
		return HeadRiskOrderPlan{}, err
	}
	if created.Created {
		if _, _, exists := m.findJobLocked(created.Job.ID); exists {
			return HeadRiskOrderPlan{}, fmt.Errorf("%w: job %s already exists", ErrDispatchJobConflict, created.Job.ID)
		}
		if _, exists := m.workOrders[created.Order.Order.ID]; exists {
			return HeadRiskOrderPlan{}, fmt.Errorf("%w: work order %s already exists", ErrDispatchJobConflict, created.Order.Order.ID)
		}
	}
	return plan, nil
}

func (m *memory) EnsureHeadRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request HeadRiskOrderRequest) (HeadRiskOrderResult, error) {
	if _, err := RequireActor(ctx); err != nil {
		return HeadRiskOrderResult{}, err
	}
	if err := request.Validate(); err != nil {
		return HeadRiskOrderResult{}, err
	}
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return HeadRiskOrderResult{}, fmt.Errorf("head risk order requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, _, err := m.riskTaskLocked(ctx, request.TaskID)
	if err != nil {
		return HeadRiskOrderResult{}, err
	}
	return m.applyHeadRiskOrderLocked(ctx, task, request)
}
