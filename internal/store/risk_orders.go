package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RiskOrderStore creates, completes, cancels, and reads a task's risk orders
// (feature-risk-based-review RR-11, RR-15; component-risk-review invariant 12;
// req-risk-based-review REQ-12, AC-12.1 to AC-12.3). A risk order is a
// risk-stage work order and its queue job. It stands beside the task's serial
// stage: no command here changes the task's state, next stage, plan state,
// gates, or review rounds.
//
// Every command runs in one transaction under the task's operation lock, the
// lock RaiseTaskRiskTier and ClaimWorkOrderCommand take, so a completion, a
// cancellation, a claim, and a raise to high serialize. Each backend also has
// a transaction-local helper for creation and cancellation, so the submission
// and plan acts of sibling tasks can create or cancel a risk order inside their
// own transaction without a second commit:
//
//   - memory: createRiskOrderLocked and cancelRiskOrdersLocked under m.mu;
//   - PostgreSQL: createRiskOrderTx and cancelRiskOrdersTx in a transaction
//     that holds lockWorkOrderTaskTx;
//   - SingleStore: Store.createRiskOrderTx and cancelRiskOrdersTx inside
//     taskTx.
//
// The lease of each command is bound to one canonical command:
// CreateRiskOrderCommand to core.WorkOrderCmdCreate,
// CompleteRiskOrderCommand to core.WorkOrderCmdSubmitRiskRating, and
// CancelRiskOrderCommand to core.WorkOrderCmdCancel.
type RiskOrderStore interface {
	// CreateRiskOrderCommand creates the task's risk order for one subject: a
	// plan version, or a head with its base. The store derives the execution
	// timeout from the task's frozen review timeout and copies the request's
	// changed paths and matched rules for a head. The boolean is true only
	// when this call created the order. A task and subject that already have
	// a risk order return that order, in any state, and false. A task whose
	// frozen risk mode is off or absent, or whose tier is high, gets no order:
	// the call returns the zero work order, false, and no error
	// (req-risk-based-review AC-1.1, AC-12.3).
	CreateRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request RiskOrderCreateRequest) (core.WorkOrder, bool, error)
	// CompleteRiskOrderCommand completes a live risk order claim with a tier
	// and a one-line reason. It refuses a missing or unknown tier and a
	// blank, multi-line, or oversized reason and leaves the order claimed
	// (AC-12.2). An accepted completion appends one rating with source
	// risk_agent, the order, the model the claim reported, and for a head the
	// paths and rules stored on the order, with its activity event, and
	// completes the order and its job. A cancelled order returns
	// ErrWorkOrderCancelled and a lost or stale claim ErrWorkOrderClaimLost;
	// neither records a rating. A replay by the session that completed the
	// order returns the recorded rating and writes nothing. When the rating
	// brings the task's tier to high, the same transaction cancels every other
	// open risk order of the task (AC-12.3).
	CompleteRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request RiskOrderCompleteRequest) (core.RiskRating, error)
	// CancelRiskOrderCommand cancels a queued, claimed, stale, or timed-out
	// risk order and ends its live claim. The boolean is true only when this
	// call cancelled it. A completed or already cancelled order is returned
	// unchanged with false and no event. Cancellation records no rating.
	CancelRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request RiskOrderCancelRequest) (core.WorkOrder, bool, error)
	// ListTaskRiskOrders returns a task's risk orders with their subjects,
	// oldest first. LatestCompletedHeadRiskPaths reads the need decision's
	// rated paths from the result.
	ListTaskRiskOrders(ctx context.Context, taskID string) ([]core.RiskOrder, error)
}

// RiskOrderCreateRequest asks for one risk order. ChangedPaths and
// MatchedRules belong to a head subject and are empty for a plan version.
// QueueTimeout is the workspace queue timeout; zero takes the default. Now is
// the creation time; zero takes the store's clock.
type RiskOrderCreateRequest struct {
	TaskID       string
	Subject      core.RiskOrderSubject
	ChangedPaths []string
	MatchedRules []riskpolicy.RuleMatch
	QueueTimeout time.Duration
	Now          time.Time
}

// RiskOrderCompleteRequest completes one claimed risk order. Claim names the
// live claim. Tier and Reason are the risk agent's rating; nothing else in the
// rating comes from the caller.
type RiskOrderCompleteRequest struct {
	WorkOrderID string
	Claim       core.WorkOrderClaimIdentity
	Tier        riskpolicy.Tier
	Reason      string
	Now         time.Time
}

// RiskOrderCancelRequest cancels one risk order with a stated reason.
type RiskOrderCancelRequest struct {
	WorkOrderID string
	Reason      string
	Now         time.Time
}

// MaxRiskOrderPaths bounds the changed paths one head risk order copies.
// GitHub's compare read already refuses a list at 300 files
// (feature-risk-based-review RR-3), so this bound only rejects a corrupt
// request.
const MaxRiskOrderPaths = 3000

// RiskTierHighCancelReason is the cancellation reason when the task's tier
// reaches high (req-risk-based-review AC-12.3; feature-risk-based-review
// RR-15).
const RiskTierHighCancelReason = "task risk tier reached high"

// ErrRiskOrderNotIndependent refuses a risk order claim by a session ID or
// client token that claimed a plan order, an implementation order, or a
// review seat of the same task. The order stays claimable
// (req-risk-based-review AC-12.1; feature-risk-based-review RR-11).
var ErrRiskOrderNotIndependent = errors.New("risk order independence: the session ID or client token already claimed a plan, implementation, or review order of this task")

// ErrNotRiskOrder refuses a risk order command on an order of another stage.
var ErrNotRiskOrder = errors.New("work order is not a risk order")

// Validate checks a creation request before any read.
func (r RiskOrderCreateRequest) Validate() error {
	if strings.TrimSpace(r.TaskID) == "" {
		return Refusef("risk order task_id is required")
	}
	if err := r.Subject.Validate(); err != nil {
		return Refuse(err)
	}
	if r.QueueTimeout < 0 {
		return Refusef("risk order queue timeout must not be negative")
	}
	if r.Subject.PlanVersion > 0 && (len(r.ChangedPaths) != 0 || len(r.MatchedRules) != 0) {
		return Refusef("a plan version risk order carries no changed paths or matched rules")
	}
	if len(r.ChangedPaths) > MaxRiskOrderPaths {
		return Refusef("risk order carries %d changed paths; the limit is %d", len(r.ChangedPaths), MaxRiskOrderPaths)
	}
	for _, path := range r.ChangedPaths {
		if strings.TrimSpace(path) == "" || !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
			return Refusef("risk order changed paths must be non-blank UTF-8 text")
		}
	}
	return nil
}

// RiskOrderCreatePlan is what one creation writes. Created is false for a
// replay, which carries the existing order in Existing, and for a skip, which
// carries nothing.
type RiskOrderCreatePlan struct {
	Created  bool
	Existing *core.RiskOrder
	Job      core.Job
	Order    core.RiskOrder
}

// PlanRiskOrderCreate decides one creation from the task, its ratings as read
// under the task lock, and its existing risk orders. A replay comes first, so
// a task whose tier later reached high still returns its earlier order.
func PlanRiskOrderCreate(task core.Task, ratings []core.RiskRating, existing []core.RiskOrder, request RiskOrderCreateRequest) (RiskOrderCreatePlan, error) {
	if err := request.Validate(); err != nil {
		return RiskOrderCreatePlan{}, err
	}
	key := request.Subject.Key()
	for i := range existing {
		if existing[i].Subject.Key() == key {
			replay := existing[i].Clone()
			return RiskOrderCreatePlan{Existing: &replay}, nil
		}
	}
	if !task.SetupContract.Risk.Enabled() {
		return RiskOrderCreatePlan{}, nil
	}
	if setter, ok := core.RiskTierOf(ratings); ok && setter.Tier == riskpolicy.TierHigh {
		return RiskOrderCreatePlan{}, nil
	}
	if core.TaskTerminal(task.State) {
		return RiskOrderCreatePlan{}, Refusef("task %s is %s; it gets no risk order", task.ID, task.State)
	}
	n := 0
	for _, order := range existing {
		n = max(n, riskOrderOrdinal(task.ID, order.Order.ID))
	}
	now := request.Now.UTC()
	if request.Now.IsZero() {
		now = time.Now().UTC()
	}
	timeout := request.QueueTimeout
	if timeout <= 0 {
		timeout = config.DefaultWorkOrderQueueTimeout
	}
	id := core.RiskOrderID(task.ID, n+1)
	order := core.WorkOrder{
		ID: id, TaskID: task.ID, JobID: id, Stage: core.StageRisk, State: core.WorkOrderQueued, Claimable: true,
		HeadSHA: request.Subject.HeadSHA, BaselineSHA: request.Subject.BaseSHA,
		// A risk order takes the review stage's frozen execution timeout and
		// the workspace queue timeout (feature-risk-based-review RR-11).
		ExecutionTimeoutText: strings.TrimSpace(task.SetupContract.StageTimeouts.Review),
		QueueEnteredAt:       now, QueueDeadline: now.Add(timeout), CreatedAt: now, UpdatedAt: now,
	}
	risk := core.RiskOrder{Order: order, Subject: request.Subject, ChangedPaths: slices.Clone(request.ChangedPaths),
		MatchedRules: cloneRuleMatches(request.MatchedRules), CreatedAt: now}.Clone()
	job := core.Job{ID: id, TaskID: task.ID, Stage: core.StageRisk, Harness: "external-mcp", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
	return RiskOrderCreatePlan{Created: true, Job: job, Order: risk}, nil
}

// PrepareRiskOrderCompletion validates one completion against the stored risk
// order and the task's ratings read under the task lock, and returns the
// rating to append. It reads the subject, the paths, the rules, and the model
// from the stored order and its claim, never from the request. It returns the
// recorded rating and false for a replay by the completing session.
func PrepareRiskOrderCompletion(task core.Task, ratings []core.RiskRating, risk core.RiskOrder, request RiskOrderCompleteRequest, now time.Time) (core.RiskRating, bool, error) {
	order := risk.Order
	if order.Stage != core.StageRisk {
		return core.RiskRating{}, false, fmt.Errorf("%w: %s", ErrNotRiskOrder, order.ID)
	}
	key := core.RiskAgentKey(order.ID)
	switch order.State {
	case core.WorkOrderCancelled:
		return core.RiskRating{}, false, ErrWorkOrderCancelled
	case core.WorkOrderCompleted:
		for _, rating := range ratings {
			if rating.Key == key && order.SessionID != "" && order.SessionID == request.Claim.SessionID && order.ClaimantID == request.Claim.ClaimantID {
				return rating, false, nil
			}
		}
		return core.RiskRating{}, false, ErrWorkOrderClaimLost
	case core.WorkOrderClaimed:
	default:
		return core.RiskRating{}, false, ErrWorkOrderClaimLost
	}
	if order.SessionID == "" || order.SessionID != request.Claim.SessionID || order.ClaimantID != request.Claim.ClaimantID || order.WorkerID != request.Claim.WorkerID ||
		!order.LeaseExpiresAt.After(now) || (!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)) {
		return core.RiskRating{}, false, ErrWorkOrderClaimLost
	}
	if request.Tier == "" {
		return core.RiskRating{}, false, Refusef("tier is required")
	}
	if !request.Tier.Valid() {
		return core.RiskRating{}, false, Refusef("tier must be low, medium, or high")
	}
	if err := core.ValidateRiskReason("reason", request.Reason); err != nil {
		return core.RiskRating{}, false, Refuse(err)
	}
	if _, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdSubmitRiskRating); err != nil {
		return core.RiskRating{}, false, err
	}
	rating := core.RiskRating{
		TaskID: task.ID, Key: key, Source: core.RiskSourceRiskAgent, Tier: request.Tier, Reason: request.Reason,
		RiskWorkOrderID: order.ID, RiskModel: order.Model, PlanVersion: risk.Subject.PlanVersion,
		HeadSHA: risk.Subject.HeadSHA, BaseSHA: risk.Subject.BaseSHA,
		MatchedRules: cloneRuleMatches(risk.MatchedRules),
	}
	if risk.Subject.PlanVersion == 0 {
		rating.CoveredPaths = slices.Clone(risk.ChangedPaths)
		if rating.CoveredPaths == nil {
			rating.CoveredPaths = []string{}
		}
	}
	if err := rating.Validate(); err != nil {
		return core.RiskRating{}, false, err
	}
	for _, recorded := range ratings {
		if recorded.Key == key {
			return core.RiskRating{}, false, fmt.Errorf("risk order %s already recorded a rating", order.ID)
		}
	}
	return numberRiskRating(ratings, rating, now), true, nil
}

// ValidateRiskOrderCancel checks a cancellation request.
func ValidateRiskOrderCancel(request RiskOrderCancelRequest) error {
	if strings.TrimSpace(request.WorkOrderID) == "" {
		return Refusef("risk order work_order_id is required")
	}
	if strings.TrimSpace(request.Reason) == "" || strings.ContainsAny(request.Reason, "\r\n\x00") || utf8.RuneCountInString(request.Reason) > core.MaxRiskReasonRunes || !utf8.ValidString(request.Reason) {
		return Refusef("risk order cancellation reason must be one non-blank line of at most %d characters", core.MaxRiskReasonRunes)
	}
	return nil
}

// RiskOrderCancelledPayload is the work_order.cancelled payload of a risk
// order. It names the claim's session, never its client token.
func RiskOrderCancelledPayload(id, attemptID, sessionID string, from core.WorkOrderState, reason string) json.RawMessage {
	return core.JSONPayload(map[string]any{
		"id": id, "attempt_id": attemptID, "session_id": sessionID, "state": core.WorkOrderCancelled,
		"from": from, "command": core.WorkOrderCmdCancel, "reason": reason, "stage": core.StageRisk,
	})
}

// RiskTierReachesHigh reports whether ratings, as counted, give the task the
// tier high.
func RiskTierReachesHigh(ratings []core.RiskRating) bool {
	setter, ok := core.RiskTierOf(ratings)
	return ok && setter.Tier == riskpolicy.TierHigh
}

// MarkPendingPlanRiskRatings derives PlanPending on every rating: true for a
// risk agent rating of a plan version that approved does not hold, and false
// for every other rating. Every backend applies it on read, so a plan version
// rated before its approval counts once it is approved, in either order
// (req-risk-based-review AC-4.3, AC-5.5).
func MarkPendingPlanRiskRatings(ratings []core.RiskRating, approved func(version int) bool) []core.RiskRating {
	for i := range ratings {
		ratings[i].PlanPending = ratings[i].Source == core.RiskSourceRiskAgent && ratings[i].PlanVersion > 0 && !approved(ratings[i].PlanVersion)
	}
	return ratings
}

// RiskRatingsNeedPlanApprovals reports whether any rating's count depends on
// a plan version's approval.
func RiskRatingsNeedPlanApprovals(ratings []core.RiskRating) bool {
	for _, rating := range ratings {
		if rating.Source == core.RiskSourceRiskAgent && rating.PlanVersion > 0 {
			return true
		}
	}
	return false
}

// LatestCompletedHeadRiskPaths returns the changed paths that the task's
// latest completed head risk order rated, or nil when no head risk order is
// completed. Plan orders, open orders, cancelled orders, and review seats
// never supply this snapshot (component-risk-review invariant 12;
// req-risk-based-review AC-5.9).
func LatestCompletedHeadRiskPaths(orders []core.RiskOrder) *[]string {
	var latest *core.RiskOrder
	for i := range orders {
		order := &orders[i]
		if order.Order.State != core.WorkOrderCompleted || order.Subject.Kind() != riskpolicy.RiskSubjectHead {
			continue
		}
		if latest == nil || riskOrderOrdinal(order.Order.TaskID, order.Order.ID) > riskOrderOrdinal(latest.Order.TaskID, latest.Order.ID) {
			latest = order
		}
	}
	if latest == nil {
		return nil
	}
	paths := slices.Clone(latest.ChangedPaths)
	if paths == nil {
		paths = []string{}
	}
	return &paths
}

// SortRiskOrders orders a task's risk orders oldest first.
func SortRiskOrders(orders []core.RiskOrder) {
	sort.SliceStable(orders, func(i, j int) bool {
		a, b := riskOrderOrdinal(orders[i].Order.TaskID, orders[i].Order.ID), riskOrderOrdinal(orders[j].Order.TaskID, orders[j].Order.ID)
		if a != b {
			return a < b
		}
		return orders[i].Order.ID < orders[j].Order.ID
	})
}

// RiskClaimConflictStage reports whether a claim of an order at stage makes
// its session ID and client token ineligible for the task's risk orders: a
// plan order, an implementation order, or a review seat
// (req-risk-based-review AC-12.1).
func RiskClaimConflictStage(stage core.Stage) bool {
	return stage == core.StageSpec || stage == core.StageImplement || stage == core.StageReview
}

// ClaimIdentity is one accepted claim's identity in the task's claim ledger.
// Every backend records one per claim, so a later independence check sees
// session IDs and client-token hashes that the order row has since cleared.
// The hash never leaves the store: no event, read, or response carries it.
type ClaimIdentity struct {
	TaskID          string
	WorkOrderID     string
	AttemptID       string
	Stage           core.Stage
	SessionID       string
	ClientTokenHash string
	ClaimedAt       time.Time
}

// RiskClaimRefusal refuses a risk order claim whose session ID or client token
// hash matches a ledger identity, a current order row, or an earlier
// work_order.claimed event of a plan order, an implementation order, or a
// review seat of the same task.
func RiskClaimRefusal(order core.WorkOrder, sessionID, tokenHash string, ledger []ClaimIdentity, taskOrders []core.WorkOrder, claimedEvents []core.Event) error {
	if order.Stage != core.StageRisk {
		return nil
	}
	matches := func(stage core.Stage, session, hash string) bool {
		return RiskClaimConflictStage(stage) && ((sessionID != "" && session == sessionID) || (tokenHash != "" && hash == tokenHash))
	}
	for _, identity := range ledger {
		if identity.TaskID == order.TaskID && matches(identity.Stage, identity.SessionID, identity.ClientTokenHash) {
			return ErrRiskOrderNotIndependent
		}
	}
	for _, candidate := range taskOrders {
		if candidate.TaskID == order.TaskID && candidate.ID != order.ID && matches(candidate.Stage, candidate.SessionID, candidate.ClientTokenHash) {
			return ErrRiskOrderNotIndependent
		}
	}
	for _, event := range claimedEvents {
		if event.Kind != "work_order.claimed" {
			continue
		}
		var prior core.WorkOrder
		if json.Unmarshal(event.Payload, &prior) == nil && prior.ID != order.ID && matches(prior.Stage, prior.SessionID, "") {
			return ErrRiskOrderNotIndependent
		}
	}
	return nil
}

func riskOrderOrdinal(taskID, id string) int {
	suffix, ok := strings.CutPrefix(id, taskID+"-risk-")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func cloneRuleMatches(matches []riskpolicy.RuleMatch) []riskpolicy.RuleMatch {
	out := make([]riskpolicy.RuleMatch, len(matches))
	for i, match := range matches {
		match.Paths = slices.Clone(match.Paths)
		out[i] = match
	}
	return out
}

// Memory backend.

type memoryRiskOrder struct {
	workspace    string
	taskID       string
	subject      core.RiskOrderSubject
	changedPaths []string
	matchedRules []riskpolicy.RuleMatch
	createdAt    time.Time
}

func memoryRiskOrderKey(workspace, workOrderID string) string {
	return workspace + "\x00" + workOrderID
}

func (m *memory) riskOrderLocked(workspace string, order core.WorkOrder) (core.RiskOrder, bool) {
	record, ok := m.riskOrders[memoryRiskOrderKey(workspace, order.ID)]
	if !ok || record.taskID != order.TaskID {
		return core.RiskOrder{}, false
	}
	return core.RiskOrder{Order: order, Subject: record.subject, ChangedPaths: record.changedPaths, MatchedRules: record.matchedRules, CreatedAt: record.createdAt}.Clone(), true
}

func (m *memory) taskRiskOrdersLocked(workspace, taskID string) []core.RiskOrder {
	var orders []core.RiskOrder
	for _, order := range m.workOrders {
		if order.TaskID != taskID || order.Stage != core.StageRisk {
			continue
		}
		if risk, ok := m.riskOrderLocked(workspace, order); ok {
			orders = append(orders, risk)
		}
	}
	SortRiskOrders(orders)
	return orders
}

// countedRiskRatingsLocked returns a task's ratings with PlanPending derived
// from its approved plan versions.
func (m *memory) countedRiskRatingsLocked(ratings []core.RiskRating, taskID string) []core.RiskRating {
	ratings = slices.Clone(ratings)
	approved := map[int]bool{}
	for _, version := range m.specs[taskID] {
		if version.Approved {
			approved[version.Version] = true
		}
	}
	return MarkPendingPlanRiskRatings(ratings, func(version int) bool { return approved[version] })
}

// createRiskOrderLocked creates one risk order under m.mu. A sibling act calls
// it inside its own critical section.
func (m *memory) createRiskOrderLocked(ctx context.Context, task core.Task, request RiskOrderCreateRequest) (core.WorkOrder, bool, error) {
	workspace, _ := WorkspaceFromContext(ctx)
	plan, err := PlanRiskOrderCreate(task, m.riskRatingsLocked(ctx, task), m.taskRiskOrdersLocked(workspace, task.ID), request)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	if plan.Existing != nil {
		return plan.Existing.Order, false, nil
	}
	if !plan.Created {
		return core.WorkOrder{}, false, nil
	}
	order := plan.Order.Order
	if _, _, exists := m.findJobLocked(plan.Job.ID); exists {
		return core.WorkOrder{}, false, fmt.Errorf("%w: job %s already exists", ErrDispatchJobConflict, plan.Job.ID)
	}
	if _, exists := m.workOrders[order.ID]; exists {
		return core.WorkOrder{}, false, fmt.Errorf("%w: work order %s already exists", ErrDispatchJobConflict, order.ID)
	}
	now := order.CreatedAt
	m.jobs[task.ID] = append(m.jobs[task.ID], plan.Job)
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: plan.Job.ID, Kind: "job.created", Payload: core.JSONPayload(plan.Job), At: now})
	m.workOrders[order.ID] = order
	if m.riskOrders == nil {
		m.riskOrders = map[string]memoryRiskOrder{}
	}
	m.riskOrders[memoryRiskOrderKey(workspace, order.ID)] = memoryRiskOrder{workspace: workspace, taskID: task.ID, subject: plan.Order.Subject,
		changedPaths: plan.Order.ChangedPaths, matchedRules: plan.Order.MatchedRules, createdAt: now}
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: order.ID, Kind: "work_order.created", Payload: WorkOrderEventPayload(order), At: now})
	return order, true, nil
}

// cancelRiskOrdersLocked cancels the task's open risk orders under m.mu: only
// the order onlyID when it is set. A sibling act calls it inside its own
// critical section.
func (m *memory) cancelRiskOrdersLocked(ctx context.Context, taskID, onlyID, reason string, at time.Time) []string {
	actor := ActorFromContext(ctx)
	return m.cancelWorkOrdersLocked(ctx, taskID, func(order core.WorkOrder) bool {
		return order.Stage == core.StageRisk && (onlyID == "" || order.ID == onlyID)
	}, reason, func(id, attemptID, sessionID string, from core.WorkOrderState) json.RawMessage {
		return RiskOrderCancelledPayload(id, attemptID, sessionID, from, reason)
	}, actor.ID, actor.Role, at)
}

func (m *memory) riskOrderTaskLocked(ctx context.Context, workOrderID string) (core.Task, core.WorkOrder, error) {
	workspace, _ := WorkspaceFromContext(ctx)
	order, ok := m.workOrders[workOrderID]
	if !ok {
		return core.Task{}, core.WorkOrder{}, fmt.Errorf("work order %s: %w", workOrderID, ErrNotFound)
	}
	task, ok := m.tasks[order.TaskID]
	if !ok || (task.Workspace != "" && workspace != "" && task.Workspace != workspace) {
		return core.Task{}, core.WorkOrder{}, fmt.Errorf("work order %s: %w", workOrderID, ErrNotFound)
	}
	return task, order, nil
}

func (m *memory) CreateRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request RiskOrderCreateRequest) (core.WorkOrder, bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.WorkOrder{}, false, err
	}
	if err := request.Validate(); err != nil {
		return core.WorkOrder{}, false, err
	}
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return core.WorkOrder{}, false, fmt.Errorf("risk order creation requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, _, err := m.riskTaskLocked(ctx, request.TaskID)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	return m.createRiskOrderLocked(ctx, task, request)
}

func (m *memory) CompleteRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request RiskOrderCompleteRequest) (core.RiskRating, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.RiskRating{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, order, err := m.riskOrderTaskLocked(ctx, request.WorkOrderID)
	if err != nil {
		return core.RiskRating{}, err
	}
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdSubmitRiskRating)) {
		return core.RiskRating{}, fmt.Errorf("risk order completion requires a valid taskops lease")
	}
	workspace, _ := WorkspaceFromContext(ctx)
	now := request.Now.UTC()
	if request.Now.IsZero() {
		now = time.Now().UTC()
	}
	risk, ok := m.riskOrderLocked(workspace, order)
	if !ok {
		return core.RiskRating{}, fmt.Errorf("%w: %s", ErrNotRiskOrder, order.ID)
	}
	ratings := m.riskRatingsLocked(ctx, task)
	rating, created, err := PrepareRiskOrderCompletion(task, ratings, risk, request, now)
	if err != nil || !created {
		return rating, err
	}
	order.State, order.Claimable, order.UpdatedAt = core.WorkOrderCompleted, false, now
	m.workOrders[order.ID] = order
	if job, index, exists := m.findJobLocked(order.JobID); exists {
		job.State, job.EndedAt = core.JobDone, now
		m.jobs[job.TaskID][index] = job
	}
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: WorkOrderEventPayload(order), At: now})
	m.insertRiskRatingLocked(ctx, memoryScopedKey{workspace: workspace, id: task.ID}, rating)
	// A rating that brings the tier to high cancels every other open risk
	// order in the same act (req-risk-based-review AC-12.3).
	if RiskTierReachesHigh(m.riskRatingsLocked(ctx, task)) {
		m.cancelRiskOrdersLocked(ctx, task.ID, "", RiskTierHighCancelReason, now)
	}
	return cloneRiskRating(rating), nil
}

func (m *memory) CancelRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request RiskOrderCancelRequest) (core.WorkOrder, bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.WorkOrder{}, false, err
	}
	if err := ValidateRiskOrderCancel(request); err != nil {
		return core.WorkOrder{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, order, err := m.riskOrderTaskLocked(ctx, request.WorkOrderID)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdCancel)) {
		return core.WorkOrder{}, false, fmt.Errorf("risk order cancellation requires a valid taskops lease")
	}
	if order.Stage != core.StageRisk {
		return core.WorkOrder{}, false, fmt.Errorf("%w: %s", ErrNotRiskOrder, order.ID)
	}
	if order.State == core.WorkOrderCompleted || order.State == core.WorkOrderCancelled {
		return order, false, nil
	}
	now := request.Now.UTC()
	if request.Now.IsZero() {
		now = time.Now().UTC()
	}
	cancelled := m.cancelRiskOrdersLocked(ctx, order.TaskID, order.ID, request.Reason, now)
	return m.workOrders[order.ID], len(cancelled) == 1, nil
}

func (m *memory) ListTaskRiskOrders(ctx context.Context, taskID string) ([]core.RiskOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, _, err := m.riskTaskLocked(ctx, taskID); err != nil {
		return nil, err
	}
	workspace, _ := WorkspaceFromContext(ctx)
	orders := m.taskRiskOrdersLocked(workspace, taskID)
	now := time.Now().UTC()
	for i := range orders {
		orders[i].Order = ProjectWorkOrderAt(orders[i].Order, now)
	}
	if orders == nil {
		orders = []core.RiskOrder{}
	}
	return orders, nil
}

// recordClaimIdentityLocked appends one accepted claim to the memory claim
// ledger.
func (m *memory) recordClaimIdentityLocked(ctx context.Context, order core.WorkOrder, tokenHash string) {
	workspace, _ := WorkspaceFromContext(ctx)
	if m.claimIdentities == nil {
		m.claimIdentities = map[memoryScopedKey][]ClaimIdentity{}
	}
	key := memoryScopedKey{workspace: workspace, id: order.TaskID}
	m.claimIdentities[key] = append(m.claimIdentities[key], ClaimIdentity{TaskID: order.TaskID, WorkOrderID: order.ID, AttemptID: order.AttemptID,
		Stage: order.Stage, SessionID: order.SessionID, ClientTokenHash: tokenHash, ClaimedAt: order.UpdatedAt})
}

func (m *memory) riskClaimRefusalLocked(ctx context.Context, order core.WorkOrder, claim core.WorkOrderClaim) error {
	if order.Stage != core.StageRisk {
		return nil
	}
	workspace, _ := WorkspaceFromContext(ctx)
	hash := ""
	if claim.ClientToken != "" {
		hash = tokenHash(claim.ClientToken)
	}
	var orders []core.WorkOrder
	for _, candidate := range m.workOrders {
		if candidate.TaskID == order.TaskID {
			orders = append(orders, candidate)
		}
	}
	return RiskClaimRefusal(order, claim.SessionID, hash, m.claimIdentities[memoryScopedKey{workspace: workspace, id: order.TaskID}], orders, m.events[order.TaskID])
}
