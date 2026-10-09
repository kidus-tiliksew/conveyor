package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

const (
	ReviewClaimedWithoutVerdict = "claimed_without_verdict"
	ReviewExpiredWithoutVerdict = "expired_without_verdict"
)

// ReviewVerdictDiagnostic describes a review claim that has not reached the
// authoritative submit_review_verdict lifecycle operation. It is derived from
// work-order state and audit events, never from child-process output.
type ReviewVerdictDiagnostic struct {
	Status         string    `json:"status"`
	WorkOrderID    string    `json:"work_order_id"`
	ReviewRound    int       `json:"review_round,omitempty"`
	ReviewSeat     int       `json:"review_seat,omitempty"`
	ClaimedAt      time.Time `json:"claimed_at,omitempty"`
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitempty"`
	Reason         string    `json:"reason"`
}

// ReviewVerdictDiagnostics derives missing-verdict state from durable
// work-order projections and lifecycle events. A queued order is classified as
// expired only when its latest claim lease elapsed without a later release or
// accepted review decision.
func ReviewVerdictDiagnostics(orders []core.WorkOrder, events []core.Event, now time.Time) []ReviewVerdictDiagnostic {
	var diagnostics []ReviewVerdictDiagnostic
	for _, order := range orders {
		if order.Stage != core.StageReview || (order.State != core.WorkOrderClaimed && order.State != core.WorkOrderQueued) {
			continue
		}
		claimedAt := order.ExecutionStartedAt
		leaseExpiresAt := order.LeaseExpiresAt
		claimIndex, releaseIndex, terminalIndex := -1, -1, -1
		for eventIndex, event := range events {
			if !reviewLifecycleEventMatches(order, event) {
				continue
			}
			switch event.Kind {
			case "work_order.claimed":
				claimIndex, claimedAt = eventIndex, event.At
				var claimed core.WorkOrder
				if json.Unmarshal(event.Payload, &claimed) == nil && !claimed.LeaseExpiresAt.IsZero() {
					leaseExpiresAt = claimed.LeaseExpiresAt
				}
			case "work_order.lease_renewed":
				if claimIndex >= 0 && eventIndex > claimIndex {
					var payload struct {
						LeaseExpiresAt time.Time `json:"lease_expires_at"`
					}
					if json.Unmarshal(event.Payload, &payload) == nil && !payload.LeaseExpiresAt.IsZero() {
						leaseExpiresAt = payload.LeaseExpiresAt
					}
				}
			case "work_order.released":
				releaseIndex = eventIndex
			case "review.completed", "review.accepted":
				terminalIndex = eventIndex
			}
		}
		base := ReviewVerdictDiagnostic{
			WorkOrderID: order.ID, ReviewRound: order.ReviewRound, ReviewSeat: order.ReviewSeat,
			ClaimedAt: claimedAt, LeaseExpiresAt: leaseExpiresAt,
		}
		if order.State == core.WorkOrderClaimed {
			if claimIndex >= 0 && terminalIndex > claimIndex {
				continue
			}
			base.Status = ReviewClaimedWithoutVerdict
			base.Reason = "review claim is active without a successful submit_review_verdict response"
			diagnostics = append(diagnostics, base)
			continue
		}
		if claimIndex < 0 || leaseExpiresAt.IsZero() || leaseExpiresAt.After(now) ||
			releaseIndex > claimIndex || terminalIndex > claimIndex {
			continue
		}
		base.Status = ReviewExpiredWithoutVerdict
		base.Reason = "review claim lease expired without terminal verdict submission"
		diagnostics = append(diagnostics, base)
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].LeaseExpiresAt.Equal(diagnostics[j].LeaseExpiresAt) {
			return diagnostics[i].WorkOrderID < diagnostics[j].WorkOrderID
		}
		return diagnostics[i].LeaseExpiresAt.Before(diagnostics[j].LeaseExpiresAt)
	})
	return diagnostics
}

func reviewLifecycleEventMatches(order core.WorkOrder, event core.Event) bool {
	if event.JobID != "" && event.JobID == order.JobID {
		return true
	}
	var payload struct {
		ID                string `json:"id"`
		ReviewWorkOrderID string `json:"review_work_order_id"`
	}
	return json.Unmarshal(event.Payload, &payload) == nil && (payload.ID == order.ID || payload.ReviewWorkOrderID == order.ID)
}

// InterruptedReviewRecoveryState describes only the latest round's incomplete
// seats whose worker attempts expired or were retry-suppressed. Completed seats
// remain authoritative and are never recreated.
type InterruptedReviewRecoveryState struct {
	Needed         bool             `json:"needed"`
	ReviewRound    int              `json:"review_round"`
	Reason         string           `json:"reason"`
	EligibleOrders []core.WorkOrder `json:"eligible_orders"`
	RetainedOrders []core.WorkOrder `json:"retained_orders"`
}

type InterruptedReviewRecoveryRequest struct {
	TaskID    string
	RequestID string
	Round     int
	Refreezes map[string]*RecoveryRefreeze
}

type InterruptedReviewRecoveryResult struct {
	RequestID       string           `json:"request_id"`
	TaskID          string           `json:"task_id"`
	ReviewRound     int              `json:"review_round"`
	RecoveredOrders []core.WorkOrder `json:"recovered_orders"`
	RetainedOrders  []core.WorkOrder `json:"retained_orders"`
}

var ErrConflictReviewRecovery = errors.New("interrupted review recovery owns conflict dispatch")

type memoryInterruptedReviewRecovery struct {
	Workspace string
	Request   InterruptedReviewRecoveryRequest
	Result    InterruptedReviewRecoveryResult
}

func InterruptedReviewRecoveryNeeded(task core.Task, orders []core.WorkOrder, events []core.Event) *InterruptedReviewRecoveryState {
	// Recovery is an operator action for live work only. Historical review
	// evidence remains visible after terminal delivery, but cannot mint more
	// agent work (REQ-1/AC-1.3, REQ-6/AC-6.2; component-persistence).
	if core.TaskTerminal(task.State) {
		return nil
	}
	latest := 0
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound > latest {
			latest = order.ReviewRound
		}
	}
	if latest == 0 {
		return nil
	}
	state := &InterruptedReviewRecoveryState{
		Needed:         true,
		ReviewRound:    latest,
		Reason:         "latest review round has interrupted seats whose claims are no longer authorized",
		EligibleOrders: make([]core.WorkOrder, 0),
		RetainedOrders: make([]core.WorkOrder, 0),
	}
	verdicts := completedReviewWorkOrderIDs(events, latest)
	for _, order := range orders {
		if order.Stage != core.StageReview || order.ReviewRound != latest {
			continue
		}
		// The append-only verdict is authoritative even if an older projection
		// still says the seat is queued or claimed. Never recreate that seat.
		if verdicts[order.ID] {
			state.RetainedOrders = append(state.RetainedOrders, order)
			continue
		}
		if order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted {
			return nil
		}
		if order.State == core.WorkOrderTimedOut || order.State == core.WorkOrderStale {
			return nil
		}
		if order.State == core.WorkOrderQueued && order.RetrySuppressed && order.SessionID == "" && order.WorkerID == "" {
			state.EligibleOrders = append(state.EligibleOrders, order)
		}
		if order.State == core.WorkOrderCompleted {
			state.RetainedOrders = append(state.RetainedOrders, order)
		}
	}
	if len(state.EligibleOrders) == 0 {
		return nil
	}
	sort.Slice(state.EligibleOrders, func(i, j int) bool { return state.EligibleOrders[i].ReviewSeat < state.EligibleOrders[j].ReviewSeat })
	sort.Slice(state.RetainedOrders, func(i, j int) bool { return state.RetainedOrders[i].ReviewSeat < state.RetainedOrders[j].ReviewSeat })
	return state
}

func completedReviewWorkOrderIDs(events []core.Event, round int) map[string]bool {
	result := map[string]bool{}
	for _, event := range events {
		if event.Kind != "review.completed" {
			continue
		}
		var payload struct {
			ReviewWorkOrderID string `json:"review_work_order_id"`
			ReviewRound       int    `json:"review_round"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ReviewWorkOrderID != "" && payload.ReviewRound == round {
			result[payload.ReviewWorkOrderID] = true
		}
	}
	return result
}

// ReviewRecoveryState is the actionable projection of a latest review round
// that cannot finish because a seat timed out or durable terminal facts
// contradict a later child outcome.
// Prior work orders remain immutable; recovery always creates a new round.
type ReviewRecoveryState struct {
	Needed             bool             `json:"needed"`
	PriorRound         int              `json:"prior_round"`
	Reason             string           `json:"reason"`
	TimedOutOrders     []core.WorkOrder `json:"timed_out_orders"`
	InconsistentOrders []core.WorkOrder `json:"inconsistent_orders"`
}

type ReviewRoundRetryRequest struct {
	TaskID     string
	RequestID  string
	Reason     string
	PriorRound int
	PRHead     string
}

type ReviewRoundRetryResult struct {
	RequestID  string           `json:"request_id"`
	TaskID     string           `json:"task_id"`
	PriorRound int              `json:"prior_round"`
	NewRound   int              `json:"new_round"`
	PRHead     string           `json:"pr_head"`
	WorkOrders []core.WorkOrder `json:"work_orders"`
}

type memoryReviewRoundRetry struct {
	Workspace string
	Request   ReviewRoundRetryRequest
	Result    ReviewRoundRetryResult
}

// ReviewRecoveryNeeded derives the retry gate from the latest review round.
// A round is terminal only when no seat is queued, claimed, or submitted.
func ReviewRecoveryNeeded(orders []core.WorkOrder, eventSets ...[]core.Event) *ReviewRecoveryState {
	latest := 0
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound > latest {
			latest = order.ReviewRound
		}
	}
	if latest == 0 {
		return nil
	}
	for _, events := range eventSets {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Kind != "review.round_completed" {
				continue
			}
			var resolution struct {
				ReviewRound int `json:"review_round"`
			}
			if json.Unmarshal(events[i].Payload, &resolution) == nil && resolution.ReviewRound == latest {
				return nil
			}
		}
	}
	state := &ReviewRecoveryState{
		Needed: true, PriorRound: latest,
		Reason:             "latest review round is terminal after a reviewer timed out",
		TimedOutOrders:     make([]core.WorkOrder, 0),
		InconsistentOrders: make([]core.WorkOrder, 0),
	}
	for _, order := range orders {
		if order.Stage != core.StageReview || order.ReviewRound != latest {
			continue
		}
		if order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted {
			return nil
		}
		switch order.State {
		case core.WorkOrderTimedOut:
			state.TimedOutOrders = append(state.TimedOutOrders, order)
		case core.WorkOrderCompleted:
			failedOutcome := core.WorkOrderOutcomeConsumesRetry(order.LastAttemptOutcome) || order.RetrySuppressed || order.LastFailureMessage != ""
			contradictsTerminalAttempt := order.AttemptID == "" || order.LastAttemptID == order.AttemptID
			if failedOutcome && contradictsTerminalAttempt {
				state.InconsistentOrders = append(state.InconsistentOrders, order)
			}
		}
	}
	if len(state.TimedOutOrders) == 0 && len(state.InconsistentOrders) == 0 {
		return nil
	}
	if len(state.InconsistentOrders) > 0 {
		state.Reason = "latest review round is non-progressing because a completed seat has a contradictory failed child outcome"
	}
	return state
}

func (m *memory) CreateWorkerPairing(ctx context.Context, pairing core.WorkerPairing) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pairing.Workspace != workspaceOrDefault(ctx, pairing.Workspace) {
		return fmt.Errorf("pairing workspace mismatch")
	}
	if _, exists := m.pairings[pairing.TokenHash]; exists {
		return fmt.Errorf("pairing already exists")
	}
	if pairing.CreatedAt.IsZero() {
		pairing.CreatedAt = time.Now().UTC()
	}
	m.pairings[pairing.TokenHash] = pairing
	m.appendEventLocked(ctx, core.Event{Kind: "worker.pairing_issued", Payload: core.JSONPayload(map[string]any{"expires_at": pairing.ExpiresAt})})
	return nil
}

func (m *memory) ConsumeWorkerPairing(_ context.Context, tokenHash string, now time.Time) (core.WorkerPairing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pairing, ok := m.pairings[tokenHash]
	if !ok || !pairing.ConsumedAt.IsZero() || !pairing.ExpiresAt.After(now) {
		return core.WorkerPairing{}, ErrPairingInvalid
	}
	pairing.ConsumedAt = now
	m.pairings[tokenHash] = pairing
	return pairing, nil
}

func (m *memory) CreateWorker(ctx context.Context, worker core.Worker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if worker.Workspace != workspaceOrDefault(ctx, worker.Workspace) {
		return fmt.Errorf("worker workspace mismatch")
	}
	for _, existing := range m.workers {
		if existing.Workspace == worker.Workspace && existing.Name == worker.Name && existing.RevokedAt.IsZero() {
			return fmt.Errorf("worker name %q already exists", worker.Name)
		}
	}
	if worker.CreatedAt.IsZero() {
		worker.CreatedAt = time.Now().UTC()
	}
	m.workers[worker.ID] = worker
	m.appendEventLocked(ctx, core.Event{Kind: "worker.enrolled", ActorRole: core.ActorWorker, ActorID: WorkerActorID(worker.ID), Payload: core.JSONPayload(map[string]string{"worker_id": worker.ID, "name": worker.Name})})
	return nil
}

func (m *memory) ListWorkers(ctx context.Context) ([]core.Worker, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, _ := WorkspaceFromContext(ctx)
	var result []core.Worker
	for _, worker := range m.workers {
		if worker.Workspace == workspace {
			result = append(result, worker)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (m *memory) AuthenticateWorker(ctx context.Context, credentialHash string) (core.Worker, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, _ := WorkspaceFromContext(ctx)
	for _, worker := range m.workers {
		if (workspace == "" || worker.Workspace == workspace) && worker.CredentialHash == credentialHash && worker.RevokedAt.IsZero() {
			return worker, nil
		}
	}
	return core.Worker{}, ErrWorkerUnauthorized
}

func (m *memory) HeartbeatWorker(ctx context.Context, id string, leaseExpires time.Time, probes []core.HarnessProbe) (core.Worker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	worker, ok := m.workers[id]
	workspace, _ := WorkspaceFromContext(ctx)
	if !ok || worker.Workspace != workspace || !worker.RevokedAt.IsZero() {
		return core.Worker{}, ErrWorkerUnauthorized
	}
	worker.LastSeenAt = time.Now().UTC()
	worker.LeaseExpiresAt = leaseExpires
	worker.Probes = append([]core.HarnessProbe(nil), probes...)
	m.workers[id] = worker
	return worker, nil
}

func (m *memory) ListHarnessModelFailures(ctx context.Context) ([]core.HarnessModelFailure, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, _ := WorkspaceFromContext(ctx)
	result := make([]core.HarnessModelFailure, 0, len(m.harnessModelFailures))
	for key, failure := range m.harnessModelFailures {
		if strings.HasPrefix(key, workspace+"\x00") {
			result = append(result, failure)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Harness != result[j].Harness {
			return result[i].Harness < result[j].Harness
		}
		return result[i].Model < result[j].Model
	})
	return result, nil
}

func (m *memory) RevokeWorker(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	worker, ok := m.workers[id]
	workspace, _ := WorkspaceFromContext(ctx)
	if !ok || worker.Workspace != workspace {
		return fmt.Errorf("worker %s not found", id)
	}
	if worker.RevokedAt.IsZero() {
		worker.RevokedAt = time.Now().UTC()
		worker.LeaseExpiresAt = time.Time{}
		m.workers[id] = worker
	}
	m.appendEventLocked(ctx, core.Event{Kind: "worker.revoked", Payload: core.JSONPayload(map[string]string{"worker_id": id})})
	return nil
}

func (m *memory) RenewWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, lease time.Duration) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	now := time.Now().UTC()
	if ok && !taskLease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdRenew)) {
		return core.WorkOrder{}, fmt.Errorf("work-order renewal requires a valid taskops lease")
	}
	if ok {
		order = m.refreshWorkOrderLocked(ctx, order, now)
	}
	if ok && order.State == core.WorkOrderCancelled &&
		((order.LastAttemptID != "" && order.LastAttemptID == claim.SessionID) ||
			m.cancelledSessionMatchesLocked(order, claim.SessionID) ||
			(order.WorkerID == claim.WorkerID && order.ClaimantID == claim.ClaimantID && order.SessionID == claim.SessionID)) {
		return core.WorkOrder{}, ErrWorkOrderCancelled
	}
	if ok {
		for i := len(m.events[order.TaskID]) - 1; i >= 0; i-- {
			if WorkOrderPreemptEventMatches(m.events[order.TaskID][i], workOrderID, claim.WorkerID, claim.SessionID) {
				return core.WorkOrder{}, ErrWorkOrderPreempted
			}
		}
	}
	if ok && deliberateCheckpointRelease(order) {
		for i := len(m.events[order.TaskID]) - 1; i >= 0; i-- {
			if ReleasedCheckpointClaimEventMatches(m.events[order.TaskID][i], order, claim) {
				return core.WorkOrder{}, ErrWorkOrderReleasedAtCheckpoint
			}
		}
	}
	if !ok || order.WorkerID != claim.WorkerID || order.ClaimantID != claim.ClaimantID || order.SessionID == "" || order.SessionID != claim.SessionID {
		return core.WorkOrder{}, ErrWorkOrderClaimLost
	}
	if order.State == core.WorkOrderSubmitted || order.State == core.WorkOrderCompleted {
		return order, nil
	}
	if order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, ErrWorkOrderClaimLost
	}
	if _, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdRenew); err != nil {
		return core.WorkOrder{}, err
	}
	expires := now.Add(lease)
	if !order.ExecutionDeadline.IsZero() && expires.After(order.ExecutionDeadline) {
		expires = order.ExecutionDeadline
	}
	order.LeaseExpiresAt = expires
	order.UpdatedAt = now
	m.workOrders[workOrderID] = order
	m.appendEventLocked(workerClaimActorContext(ctx, claim.WorkerID), core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.lease_renewed", Payload: core.JSONPayload(map[string]any{"attempt_id": order.AttemptID, "lease_expires_at": expires})})
	return order, nil
}

func (m *memory) RecordWorkOrderContinuation(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, continuation core.WorkOrderContinuation) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	now := time.Now().UTC()
	if ok {
		order = m.refreshWorkOrderLocked(ctx, order, now)
	}
	if !ok || order.Stage != core.StageImplement || order.State != core.WorkOrderClaimed || order.SessionID == "" ||
		order.SessionID != claim.SessionID || order.WorkerID != claim.WorkerID || order.ClaimantID != claim.ClaimantID ||
		!order.LeaseExpiresAt.After(now) || (!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)) {
		return core.WorkOrder{}, ErrWorkOrderClaimLost
	}
	if continuation.AttemptID != order.AttemptID {
		return core.WorkOrder{}, fmt.Errorf("continuation attempt does not match the active work-order attempt")
	}
	expectedHarness := order.Agent
	if expectedHarness == "" {
		expectedHarness = order.RequiredHarness
	}
	if expectedHarness != "" && continuation.Harness != expectedHarness {
		return core.WorkOrder{}, fmt.Errorf("continuation harness %q does not match active harness %q", continuation.Harness, expectedHarness)
	}
	order.ContinuationSessionID = continuation.SessionID
	order.ContinuationAttemptID = continuation.AttemptID
	order.ContinuationHarness = continuation.Harness
	order.ContinuationLaunchEnvironment = continuation.LaunchEnvironment
	order.UpdatedAt = now
	m.workOrders[workOrderID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.continuation_reported", Payload: core.JSONPayload(map[string]any{
		"work_order_id": order.ID, "attempt_id": continuation.AttemptID, "harness": continuation.Harness,
		"launch_environment": continuation.LaunchEnvironment,
	}), At: now})
	return order, nil
}

func (m *memory) ReleaseWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, release core.WorkOrderRelease) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	now := time.Now().UTC()
	if ok && !taskLease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdRelease)) {
		return core.WorkOrder{}, fmt.Errorf("work-order release requires a valid taskops lease")
	}
	if ok {
		order = m.refreshWorkOrderLocked(ctx, order, now)
	}
	if ok && order.Stage == core.StageReview && m.reviewSeatAcceptedLocked(order) {
		return core.WorkOrder{}, fmt.Errorf("accepted review seat %s is terminal", workOrderID)
	}
	if ok && order.State == core.WorkOrderCancelled &&
		((order.LastAttemptID != "" && order.LastAttemptID == release.SessionID) ||
			m.cancelledSessionMatchesLocked(order, release.SessionID) ||
			(order.WorkerID == claim.WorkerID && order.SessionID == release.SessionID)) {
		return core.WorkOrder{}, ErrWorkOrderCancelled
	}
	if !ok || order.SessionID == "" || order.SessionID != release.SessionID || order.State != core.WorkOrderClaimed ||
		!order.LeaseExpiresAt.After(now) || (!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)) {
		return core.WorkOrder{}, ErrWorkOrderClaimLost
	}
	if order.WorkerID != claim.WorkerID || order.ClaimantID != claim.ClaimantID {
		return core.WorkOrder{}, ErrWorkOrderClaimUnauthorized
	}
	queueTimeout := order.QueueDeadline.Sub(order.QueueEnteredAt)
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdRelease)
	if err != nil {
		return core.WorkOrder{}, err
	}
	attemptID := order.AttemptID
	order.LastAttemptID = attemptID
	clearActiveAttempt(&order)
	order.OperatorDirection = ""
	order.Checkpoint = release.Checkpoint
	order.State = next
	previousOutcome := order.LastAttemptOutcome
	progressed := m.attemptReportedProgressLocked(order)
	previousTransientFailures := m.previousTransientFailuresLocked(order)
	order.LastAttemptOutcome = release.Outcome
	order.NextRetryAt = time.Time{}
	order.RetrySuppressionReason = ""
	if core.WorkOrderOutcomeConsumesRetry(release.Outcome) {
		previousDetail := order.LastFailureDetail
		detail := strings.TrimSpace(release.FailureDetail)
		order.LastFailureMessage = strings.TrimSpace(release.Reason)
		order.LastFailureCategory = strings.TrimSpace(release.FailureCategory)
		order.LastFailureDetail = detail
		order.LastFailureExitStatus = release.ExitStatus
		order.LastFailureAt = now
		limit := release.AutomaticRetryLimit
		if limit <= 0 {
			limit = 3
		}
		transientConnectivity := order.LastFailureCategory == core.WorkOrderFailureTransientConnectivity
		consecutiveTransientFailures := 0
		if transientConnectivity {
			consecutiveTransientFailures = 1
			if !progressed && previousOutcome == release.Outcome {
				consecutiveTransientFailures += previousTransientFailures
			}
		}
		identical := detail != "" && previousOutcome == release.Outcome && detail == previousDetail && !transientConnectivity
		if order.AutomaticRetryCount < limit {
			order.AutomaticRetryCount++
			if identical {
				order.RetrySuppressed = true
				order.RetrySuppressionReason = core.IdenticalFailureSuppressionReason
			} else {
				delay := workOrderRetryDelay(release, order.AutomaticRetryCount)
				if transientConnectivity {
					delay = core.TransientConnectivityRetryDelay(consecutiveTransientFailures)
				}
				order.NextRetryAt = now.Add(delay)
				order.RetrySuppressed = false
			}
		} else {
			order.RetrySuppressed = true
		}
		if release.ModelRejection && order.RequiredHarness != "" && order.RequiredModel != "" && detail != "" {
			workspace, _ := WorkspaceFromContext(ctx)
			key := workspace + "\x00" + order.RequiredHarness + "\x00" + order.RequiredModel
			m.harnessModelFailures[key] = core.HarnessModelFailure{Harness: order.RequiredHarness, Model: order.RequiredModel, Detail: detail, WorkOrderID: order.ID, ObservedAt: now}
		}
		if transientConnectivity {
			order.LastFailureDetail = core.TransientConnectivityFailureDetail(detail, consecutiveTransientFailures, order.NextRetryAt)
		}
	} else {
		order.LastFailureCategory = ""
		order.LastFailureMessage = strings.TrimSpace(release.Reason)
		order.LastFailureDetail = strings.TrimSpace(release.FailureDetail)
		order.LastFailureExitStatus = nil
		order.LastFailureAt = now
		order.RetrySuppressed = true
	}
	order.ClearExecutionPins()
	order.UpdatedAt = now
	order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
	order.Claimable = order.ClaimableAt(now)
	m.workOrders[workOrderID] = order
	for taskID, jobs := range m.jobs {
		for i := range jobs {
			if jobs[i].ID == order.JobID {
				jobs[i].State = core.JobPending
				jobs[i].StartedAt = time.Time{}
				jobs[i].EndedAt = time.Time{}
				m.jobs[taskID] = jobs
			}
		}
	}
	kind := "work_order.released"
	if core.WorkOrderOutcomeConsumesRetry(release.Outcome) {
		kind = "work_order.child_failed"
		if release.Outcome == core.WorkOrderOutcomeStalled {
			kind = "work_order.stalled"
		}
	}
	payload := map[string]any{"attempt_id": attemptID, "session_id": release.SessionID, "reason": release.Reason, "release_cause": release.Cause, "detail": order.LastFailureDetail, "outcome": release.Outcome, "failure_category": order.LastFailureCategory, "consecutive_transient_failures": core.ConsecutiveTransientFailureCount(order.LastFailureCategory, previousTransientFailures, progressed, previousOutcome == release.Outcome), "exit_status": release.ExitStatus, "automatic_retry_count": order.AutomaticRetryCount, "next_retry_at": order.NextRetryAt, "retry_suppressed": order.RetrySuppressed, "suppression_reason": order.RetrySuppressionReason}
	if release.Checkpoint != nil {
		payload["checkpoint"] = release.Checkpoint
	}
	m.appendEventLocked(workerClaimActorContext(ctx, claim.WorkerID), core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: kind, Payload: core.JSONPayload(payload), At: now})
	return order, nil
}

func (m *memory) attemptReportedProgressLocked(order core.WorkOrder) bool {
	events := ChronologicalTaskEvents(m.events[order.TaskID])
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.JobID != order.JobID {
			continue
		}
		if event.Kind == "work_order.progress_reported" {
			return true
		}
		if event.Kind == "work_order.claimed" {
			return false
		}
	}
	return false
}

func (m *memory) previousTransientFailuresLocked(order core.WorkOrder) int {
	if order.LastFailureCategory != core.WorkOrderFailureTransientConnectivity {
		return 0
	}
	events := ChronologicalTaskEvents(m.events[order.TaskID])
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.JobID != order.JobID || (event.Kind != "work_order.child_failed" && event.Kind != "work_order.stalled") {
			continue
		}
		var payload struct {
			Consecutive int `json:"consecutive_transient_failures"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil {
			return payload.Consecutive
		}
		return 0
	}
	return 0
}

func (m *memory) RecordWorkOrderAttemptCheckpoint(ctx context.Context, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	if !ok {
		return false, ErrWorkOrderClaimLost
	}
	for _, event := range m.events[order.TaskID] {
		if event.Kind == "work_order.writer_admitted" {
			return false, ErrWorkOrderClaimLost
		}
	}
	authorized := order.AuthorizesAttemptCheckpoint(workerID, checkpoint, time.Now().UTC())
	if !authorized {
		for _, event := range m.events[order.TaskID] {
			if AttemptCheckpointClaimEventMatches(event, order.ID, workerID, checkpoint) {
				authorized = true
				break
			}
		}
	}
	if !authorized {
		return false, ErrWorkOrderClaimLost
	}
	for _, event := range m.events[order.TaskID] {
		if AttemptCheckpointEventMatches(event, order.ID, checkpoint.AttemptID, checkpoint.CommitSHA) {
			return false, nil
		}
	}
	m.appendEventLocked(workerClaimActorContext(ctx, workerID), core.Event{
		TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.attempt_checkpointed",
		Payload: AttemptCheckpointPayload(order, checkpoint), At: time.Now().UTC(),
	})
	return true, nil
}

func (m *memory) UpsertWorkOrderActivitySnapshot(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	if !ok || order.State != core.WorkOrderClaimed || order.WorkerID != claim.WorkerID ||
		order.ClaimantID != claim.ClaimantID || order.SessionID != claim.SessionID ||
		order.AttemptID == "" || !order.LeaseExpiresAt.After(time.Now().UTC()) {
		return ErrWorkOrderClaimLost
	}
	if selected, hasWorkspace := WorkspaceFromContext(ctx); hasWorkspace && m.tasks[order.TaskID].Workspace != selected {
		return ErrWorkOrderClaimLost
	}
	m.workOrderActivitySnapshots[workOrderID] = core.WorkOrderActivitySnapshot{
		AttemptID: order.AttemptID, Content: content, CapturedAt: time.Now().UTC(),
	}
	return nil
}

func (m *memory) FinalizeWorkOrderAttemptObservability(ctx context.Context, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	if !ok {
		return ErrWorkOrderClaimLost
	}
	if selected, hasWorkspace := WorkspaceFromContext(ctx); hasWorkspace && m.tasks[order.TaskID].Workspace != selected {
		return ErrWorkOrderClaimLost
	}
	claimed, checkpointed := false, false
	for _, event := range m.events[order.TaskID] {
		if AttemptCheckpointClaimEventMatches(event, order.ID, workerID, checkpoint) {
			claimed = true
		}
		if AttemptCheckpointEventMatches(event, order.ID, checkpoint.AttemptID, checkpoint.CommitSHA) {
			checkpointed = true
		}
	}
	if !claimed || !checkpointed {
		return ErrWorkOrderClaimLost
	}
	if current, exists := m.workOrderActivitySnapshots[workOrderID]; exists && current.AttemptID == checkpoint.AttemptID {
		delete(m.workOrderActivitySnapshots, workOrderID)
	}
	if checkpoint.Transcript == nil {
		return nil
	}
	for _, capture := range m.workOrderTranscriptCaptures[workOrderID] {
		if capture.AttemptID == checkpoint.AttemptID {
			return nil
		}
	}
	m.workOrderTranscriptCaptures[workOrderID] = append(m.workOrderTranscriptCaptures[workOrderID], core.WorkOrderTranscriptCapture{
		AttemptID: checkpoint.AttemptID, Content: checkpoint.Transcript.Content,
		TerminationReason: checkpoint.TerminationReason, Truncated: checkpoint.Transcript.Truncated,
		CapturedAt: time.Now().UTC(),
	})
	return nil
}

func (m *memory) RecordWorkOrderAttemptCapture(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, capture core.WorkOrderAttemptCapture) (core.WorkOrderAttemptCaptureResult, error) {
	capture = NormalizeAttemptCapture(capture)
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	if !ok {
		return core.WorkOrderAttemptCaptureResult{}, ErrWorkOrderClaimUnauthorized
	}
	if selected, hasWorkspace := WorkspaceFromContext(ctx); hasWorkspace && m.tasks[order.TaskID].Workspace != selected {
		return core.WorkOrderAttemptCaptureResult{}, ErrWorkOrderClaimUnauthorized
	}
	authorized := false
	for _, event := range m.events[order.TaskID] {
		if AttemptCaptureClaimEventMatches(event, order, claim, capture) {
			authorized = true
			break
		}
	}
	if !authorized {
		return core.WorkOrderAttemptCaptureResult{}, ErrWorkOrderClaimUnauthorized
	}
	existingReason, existing := "", false
	for _, prior := range m.workOrderTranscriptCaptures[workOrderID] {
		if prior.AttemptID == capture.AttemptID {
			existingReason, existing = prior.TerminationReason, true
			break
		}
	}
	reason, err := AttemptCaptureReason(order, capture, existingReason, existing)
	if err != nil {
		return core.WorkOrderAttemptCaptureResult{}, err
	}
	if current, exists := m.workOrderActivitySnapshots[workOrderID]; exists && current.AttemptID == capture.AttemptID {
		delete(m.workOrderActivitySnapshots, workOrderID)
	}
	result := core.WorkOrderAttemptCaptureResult{TerminationReason: reason}
	if capture.Transcript == nil || existing {
		return result, nil
	}
	m.workOrderTranscriptCaptures[workOrderID] = append(m.workOrderTranscriptCaptures[workOrderID], core.WorkOrderTranscriptCapture{
		AttemptID: capture.AttemptID, Content: capture.Transcript.Content, TerminationReason: reason,
		Truncated: capture.Transcript.Truncated, CapturedAt: time.Now().UTC(),
	})
	result.Created = true
	return result, nil
}

func (m *memory) GetWorkOrderActivitySnapshot(ctx context.Context, workOrderID string) (core.WorkOrderActivitySnapshot, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	order, ok := m.workOrders[workOrderID]
	if !ok {
		return core.WorkOrderActivitySnapshot{}, false, nil
	}
	if selected, hasWorkspace := WorkspaceFromContext(ctx); hasWorkspace && m.tasks[order.TaskID].Workspace != selected {
		return core.WorkOrderActivitySnapshot{}, false, nil
	}
	snapshot, exists := m.workOrderActivitySnapshots[workOrderID]
	return snapshot, exists, nil
}

func (m *memory) ListWorkOrderTranscriptCaptures(ctx context.Context, workOrderID string) ([]core.WorkOrderTranscriptCapture, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	order, ok := m.workOrders[workOrderID]
	if !ok {
		return []core.WorkOrderTranscriptCapture{}, nil
	}
	if selected, hasWorkspace := WorkspaceFromContext(ctx); hasWorkspace && m.tasks[order.TaskID].Workspace != selected {
		return []core.WorkOrderTranscriptCapture{}, nil
	}
	return append([]core.WorkOrderTranscriptCapture(nil), m.workOrderTranscriptCaptures[workOrderID]...), nil
}

func workerClaimActorContext(ctx context.Context, workerID string) context.Context {
	if workerID == "" {
		return ctx
	}
	return WithActor(ctx, Actor{ID: WorkerActorID(workerID), Role: core.ActorWorker})
}

// AttemptCheckpointClaimEventMatches authorizes late audit reconciliation
// only from the immutable claim event for the exact worker, session, and
// attempt that created the preservation commit. This lets an attempt record a
// successful push after observing authority loss without restoring lifecycle
// authority or admitting a different task attempt.
func AttemptCheckpointClaimEventMatches(event core.Event, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) bool {
	if event.Kind != "work_order.claimed" {
		return false
	}
	var claimed core.WorkOrder
	return json.Unmarshal(event.Payload, &claimed) == nil && claimed.ID == workOrderID &&
		claimed.Stage == core.StageImplement && claimed.WorkerID == workerID &&
		claimed.SessionID == checkpoint.SessionID && claimed.AttemptID == checkpoint.AttemptID
}

// ReleasedCheckpointClaimEventMatches binds a post-release renewal outcome to
// the immutable claim event for the retained last attempt. This prevents a
// later or differently authenticated session from inheriting graceful exit.
func ReleasedCheckpointClaimEventMatches(event core.Event, order core.WorkOrder, claim core.WorkOrderClaimIdentity) bool {
	if event.Kind != "work_order.claimed" || event.JobID != order.JobID || order.LastAttemptID == "" {
		return false
	}
	var claimed core.WorkOrder
	if json.Unmarshal(event.Payload, &claimed) != nil {
		return false
	}
	workerIdentityMatches := claimed.WorkerID != "" && claim.WorkerID != "" && claimed.WorkerID == claim.WorkerID
	runIdentityMatches := claimed.WorkerID == "" && claim.WorkerID == "" &&
		core.IsTaskRunClaimantID(claimed.ClaimantID) && core.IsTaskRunClaimantID(claim.ClaimantID)
	return claimed.ID == order.ID && claimed.AttemptID == order.LastAttemptID &&
		(workerIdentityMatches || runIdentityMatches) && claimed.ClaimantID == claim.ClaimantID &&
		claimed.SessionID == claim.SessionID
}

// ReleasedCheckpointClaimMatches verifies a deliberate queued release against
// the immutable claim event for the exact retained attempt. It is read-only so
// reconciliation can distinguish a child's own checkpoint handoff from real
// authority loss without renewing or restoring the claim.
func ReleasedCheckpointClaimMatches(ctx context.Context, st Store, order core.WorkOrder, claim core.WorkOrderClaimIdentity) (bool, error) {
	if !deliberateCheckpointRelease(order) {
		return false, nil
	}
	events, err := st.ListEvents(ctx, order.TaskID)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if ReleasedCheckpointClaimEventMatches(events[i], order, claim) {
			return true, nil
		}
	}
	return false, nil
}

func deliberateCheckpointRelease(order core.WorkOrder) bool {
	return order.State == core.WorkOrderQueued && order.WorkerID == "" && order.ClaimantID == "" && order.SessionID == "" &&
		(order.LastFailureMessage == core.WorkOrderReleaseReasonOperatorCheckpointReached ||
			order.LastFailureMessage == core.WorkOrderReleaseReasonPlanRevisionRequested)
}

func AttemptCheckpointPayload(order core.WorkOrder, checkpoint core.WorkOrderAttemptCheckpoint) json.RawMessage {
	return core.JSONPayload(map[string]any{
		"task_id": order.TaskID, "work_order_id": order.ID, "attempt_id": checkpoint.AttemptID,
		"termination_reason": checkpoint.TerminationReason, "commit_sha": checkpoint.CommitSHA,
		"push_result": checkpoint.PushResult,
	})
}

func AttemptCheckpointEventMatches(event core.Event, workOrderID, attemptID, commitSHA string) bool {
	if event.Kind != "work_order.attempt_checkpointed" {
		return false
	}
	var payload struct {
		WorkOrderID string `json:"work_order_id"`
		AttemptID   string `json:"attempt_id"`
		CommitSHA   string `json:"commit_sha"`
	}
	return json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID == workOrderID &&
		payload.AttemptID == attemptID && payload.CommitSHA == commitSHA
}

func clearActiveAttempt(order *core.WorkOrder) {
	clearClaimOwnership(order)
	order.ExecutionStartedAt, order.ExecutionDeadline = time.Time{}, time.Time{}
}

func (m *memory) cancelledSessionMatchesLocked(order core.WorkOrder, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	for i := len(m.events[order.TaskID]) - 1; i >= 0; i-- {
		event := m.events[order.TaskID][i]
		if event.Kind != "work_order.cancelled" || event.JobID != order.JobID {
			continue
		}
		var payload struct {
			SessionID string `json:"session_id"`
		}
		return json.Unmarshal(event.Payload, &payload) == nil && payload.SessionID == sessionID
	}
	return false
}

func clearClaimOwnership(order *core.WorkOrder) {
	order.ClaimantID, order.SessionID, order.AttemptID, order.ClientTokenHash = "", "", "", ""
	order.Agent, order.Model, order.WorkerID, order.ModelEnforcement = "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
}

func workOrderRetryDelay(release core.WorkOrderRelease, retry int) time.Duration {
	initial := release.InitialRetryDelay
	if initial <= 0 {
		initial = time.Second
	}
	maximum := release.MaximumRetryDelay
	if maximum < initial {
		maximum = initial
	}
	delay := initial
	for attempt := 1; attempt < retry && delay < maximum; attempt++ {
		delay *= 2
		if delay > maximum {
			delay = maximum
		}
	}
	return delay
}

func (m *memory) AcceptReviewDecisionCommand(ctx context.Context, lease taskops.TaskLease, decision core.ReviewDecision) error {
	if !lease.ValidFor(decision.TaskID) {
		return fmt.Errorf("review lifecycle mutation requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[decision.TaskID]
	if !ok {
		return fmt.Errorf("task %s not found", decision.TaskID)
	}
	if !m.verifyReviewReadyLocked(task) {
		return fmt.Errorf("review requires completed verification for the submitted head")
	}
	lookup := ExecutionDocumentLookup(func(ctx context.Context, id string, version int) (core.SpecVersion, bool, error) {
		if selected, scoped := WorkspaceFromContext(ctx); scoped && m.tasks[id].Workspace != selected {
			return core.SpecVersion{}, false, nil
		}
		versions := m.specs[id]
		for i := len(versions) - 1; i >= 0; i-- {
			spec := versions[i]
			if (version == 0 && spec.Approved) || (version > 0 && spec.Version == version) {
				return spec, true, nil
			}
		}
		return core.SpecVersion{}, false, nil
	})
	if err := ValidateReviewAcceptance(ctx, lookup, task, &decision, m.verificationReviewStateLocked(task, decision.ReviewWorkOrderID)); err != nil {
		return err
	}
	job, _, ok := m.findJobLocked(decision.JobID)
	if !ok || job.TaskID != decision.TaskID {
		return fmt.Errorf("job %s does not belong to task %s", decision.JobID, decision.TaskID)
	}
	order := m.workOrders[decision.ReviewWorkOrderID]
	if decision.ClaimSession != "" {
		if order.ID == "" || order.TaskID != decision.TaskID || order.JobID != decision.JobID || order.Stage != core.StageReview {
			return fmt.Errorf("review work order %s does not belong to task %s", decision.ReviewWorkOrderID, decision.TaskID)
		}
		if order.State != core.WorkOrderCompleted {
			if _, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
				return err
			}
		}
		if job.State != core.JobDone {
			if err := ValidateJobTransition(job.State, core.JobDone); err != nil {
				return err
			}
		}
	}
	completed := false
	accepted := false
	for _, event := range m.events[decision.TaskID] {
		var prior struct {
			ReviewWorkOrderID string `json:"review_work_order_id"`
		}
		if json.Unmarshal(event.Payload, &prior) != nil || prior.ReviewWorkOrderID != decision.ReviewWorkOrderID {
			continue
		}
		if event.Kind == "review.accepted" {
			accepted = true
		}
		completed = completed || event.Kind == "review.completed"
	}
	if accepted {
		if decision.ClaimSession != "" {
			return m.settleAcceptedReviewLocked(ctx, decision, order, job, time.Now().UTC())
		}
		return nil
	}
	if !completed {
		if decision.ClaimSession != "" {
			if order.State != core.WorkOrderClaimed || order.SessionID != decision.ClaimSession || !order.LeaseExpiresAt.After(time.Now()) {
				return ErrWorkOrderClaimLost
			}
			if task.State == core.TaskQueued {
				from := task.State
				task.State = core.TaskRunning
				m.tasks[task.ID] = task
				m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: decision.JobID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{
					"from": from, "to": task.State, "command": core.TaskOrderClaim, "claim_authority_work_order_id": order.ID,
				})})
			}
		}
		payload := reviewDecisionPayload(decision)
		m.appendEventLocked(ctx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.completed", Payload: payload})
	}
	if decision.PublicationEligible {
		if _, exists := m.publications[decision.ReviewWorkOrderID]; !exists {
			now := time.Now().UTC()
			publication := reviewPublicationFromDecision(decision)
			publication.State, publication.CreatedAt, publication.UpdatedAt = core.ReviewPublicationQueued, now, now
			m.publications[publication.ReviewWorkOrderID] = publication
			m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: decision.JobID, Kind: "review.publication_queued", Payload: core.JSONPayload(publication)})
		}
	}
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: decision.JobID, Kind: "review.accepted", Payload: core.JSONPayload(map[string]any{"review_work_order_id": decision.ReviewWorkOrderID, "review_round": decision.ReviewRound, "review_seat": decision.ReviewSeat})})
	if decision.ClaimSession != "" {
		if err := m.settleAcceptedReviewLocked(ctx, decision, order, job, time.Now().UTC()); err != nil {
			return err
		}
	}

	reviews, required := m.completedReviewRoundLocked(decision.TaskID, decision.ReviewRound, decision.ReviewWorkOrderID)
	if len(reviews) < required {
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "review.round_pending", Payload: core.JSONPayload(map[string]any{"review_round": decision.ReviewRound, "completed": len(reviews), "required": required})})
		return nil
	}
	for _, event := range m.events[decision.TaskID] {
		var payload struct {
			ReviewRound int `json:"review_round"`
		}
		if event.Kind == "review.round_completed" && json.Unmarshal(event.Payload, &payload) == nil && payload.ReviewRound == decision.ReviewRound {
			return nil
		}
	}
	aggregate := aggregateReviewRound(decision.ReviewRound, reviews)
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: decision.JobID, Kind: "review.round_completed", Payload: core.JSONPayload(aggregate)})

	command, next, recovery := core.TaskGateMerge, core.Stage(""), core.StageImplement
	autoApprove := false
	if aggregate.Verdict == "changes_requested" {
		count := 0
		for _, event := range m.events[decision.TaskID] {
			if event.Kind == "pipeline.bounced" {
				count++
			}
		}
		// The check-in comparison uses bounces since the last human
		// intervention, not the lifetime count; the recorded
		// count in the event payload stays lifetime for the timeline.
		window := m.countEventsSinceHumanInterventionLocked(decision.TaskID, "pipeline.bounced")
		actorID := fmt.Sprintf("review:round:%d", decision.ReviewRound)
		count++
		window++
		plan := PlanChangesRequested(ChangesRequestedInput{
			TaskID: decision.TaskID, JobID: decision.JobID, ActorID: actorID, ActorRole: core.ActorAgent,
			ReasonCode: aggregate.ReasonCode, Feedback: aggregate.Feedback, Source: "mcp-review-panel",
			Count: count, Window: window, MaxBounces: decision.MaxBounces, ReviewRound: decision.ReviewRound,
			Reviews: aggregate.Reviews, Requeue: core.TaskStageAdvance, EnforceLimit: true,
		})
		m.nextReviewID++
		plan.Intervention.ID = m.nextReviewID
		m.interventions[decision.TaskID] = append(m.interventions[decision.TaskID], plan.Intervention)
		for _, event := range plan.Events {
			m.appendEventLocked(ctx, event)
		}
		command, next, recovery = plan.Command, plan.NextStage, plan.Recovery
	} else if decision.ReviewKind == "refresh" || (decision.PolicyVersion > 0 && !decision.MergeApproval) || (decision.PolicyVersion == 0 && decision.Level == core.L0) {
		autoApprove, recovery = true, ""
	}
	fromState, fromStage := task.State, task.NextStage
	state, err := core.TransitionTask(fromState, command)
	if err != nil {
		return err
	}
	if autoApprove {
		approved, approveErr := core.TransitionTask(state, core.TaskInterventionApproveReview)
		if approveErr != nil {
			return approveErr
		}
		// Auto-approval currently projects running -> awaiting_human with
		// gate.merge even though the merge gate is off. The table has no direct
		// running -> approved edge; keep this explicit gap workaround visible
		// until a table amendment supplies the intended command.
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": command})})
		fromState, state = state, approved
		command = core.TaskInterventionApproveReview
	}
	nonAdvancingRefresh := NonAdvancingRefreshBinding(decision, aggregate.ApprovedHeadSHA)
	if nonAdvancingRefresh {
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: decision.JobID, Kind: "review.refresh_binding_not_advanced", Payload: core.JSONPayload(map[string]any{
			"review_work_order_id": decision.ReviewWorkOrderID, "review_round": decision.ReviewRound,
			"baseline_sha": decision.BaselineSHA, "head_sha": decision.HeadSHA, "bound_sha": aggregate.ApprovedHeadSHA,
		})})
	}
	if aggregate.Verdict == "approve" && aggregate.ApprovedHeadSHA != "" {
		task.ReviewedHeadSHA = aggregate.ApprovedHeadSHA
		if state == core.TaskApproved && !nonAdvancingRefresh {
			task.ApprovedHeadSHA, task.ApprovalStale = aggregate.ApprovedHeadSHA, false
			task.RefreshBaselineSHA, task.RefreshHeadSHA, task.RefreshReviewScope = "", "", ""
		}
	}
	task.State, task.NextStage, task.RecoveryStage = state, next, recovery
	m.tasks[task.ID] = task
	// When autoApprove is true, this second projection records an intervention
	// command without a human intervention. It is the paired gap workaround for
	// the absent running -> approved table edge.
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": command})})
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": fromStage, "next_stage": next, "recovery_stage": recovery, "state": state, "review_work_order_id": decision.ReviewWorkOrderID})})
	return nil
}

func (m *memory) settleAcceptedReviewLocked(ctx context.Context, decision core.ReviewDecision, order core.WorkOrder, job core.Job, now time.Time) error {
	if order.State != core.WorkOrderCompleted {
		next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdSubmitReviewVerdict)
		if err != nil {
			return err
		}
		order.State, order.Claimable, order.OperatorDirection, order.UpdatedAt = next, false, "", now
		order.ContinuationSessionID, order.ContinuationAttemptID = "", ""
		order.ContinuationHarness, order.ContinuationLaunchEnvironment = "", ""
		m.workOrders[order.ID] = order
		m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order), At: now})
	}
	if job.State != core.JobDone {
		if err := ValidateJobTransition(job.State, core.JobDone); err != nil {
			return err
		}
		job.State, job.EndedAt = core.JobDone, now
		job.TokensIn, job.TokensOut = order.TokensIn, order.TokensOut
		if cost := order.HistoricalCostUSD(); cost != nil {
			job.CostUSD = cost
		}
		_, index, _ := m.findJobLocked(job.ID)
		m.jobs[job.TaskID][index] = job
		m.appendEventLocked(ctx, core.Event{TaskID: job.TaskID, JobID: job.ID, Kind: "job.updated", Payload: core.JSONPayload(job), At: now})
	}
	for id, implementation := range m.workOrders {
		if implementation.TaskID != decision.TaskID || implementation.Stage != core.StageImplement || implementation.State != core.WorkOrderSubmitted {
			continue
		}
		if implementation.ContinuationSessionID == "" && implementation.ContinuationAttemptID == "" &&
			implementation.ContinuationHarness == "" && implementation.ContinuationLaunchEnvironment == "" {
			continue
		}
		implementation.ContinuationSessionID, implementation.ContinuationAttemptID = "", ""
		implementation.ContinuationHarness, implementation.ContinuationLaunchEnvironment = "", ""
		implementation.UpdatedAt = now
		m.workOrders[id] = implementation
		m.appendEventLocked(ctx, core.Event{TaskID: implementation.TaskID, JobID: implementation.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(implementation), At: now})
	}
	return nil
}

type completedReview struct {
	ReviewWorkOrderID string `json:"review_work_order_id"`
	Verdict           string `json:"verdict"`
	ReasonCode        string `json:"reason_code"`
	Summary           string `json:"summary"`
	Feedback          string `json:"feedback"`
	ReviewerModel     string `json:"reviewer_model"`
	ReviewRound       int    `json:"review_round"`
	ReviewSeat        int    `json:"review_seat"`
	RequiredModel     string `json:"required_model"`
	RequiredHarness   string `json:"required_harness"`
	RequiredEffort    string `json:"required_effort"`
	ModelEnforcement  string `json:"model_enforcement"`
	ReviewedCommitSHA string `json:"reviewed_commit_sha"`
}

type reviewRoundResult struct {
	ReviewRound     int               `json:"review_round"`
	Verdict         string            `json:"verdict"`
	ReasonCode      string            `json:"reason_code"`
	Summary         string            `json:"summary"`
	Feedback        string            `json:"feedback,omitempty"`
	Reviews         []completedReview `json:"reviews"`
	ApprovedHeadSHA string            `json:"approved_head_sha,omitempty"`
}

func (m *memory) completedReviewRoundLocked(taskID string, round int, workOrderID string) ([]completedReview, int) {
	superseded := SupersededReviewWorkOrders(m.events[taskID])
	required := 0
	for _, order := range m.workOrders {
		if order.TaskID == taskID && order.Stage == core.StageReview && order.ReviewRound == round && !superseded[order.ID] && (round > 0 || order.ID == workOrderID) {
			required++
		}
	}
	var reviews []completedReview
	for _, event := range m.events[taskID] {
		if event.Kind != "review.completed" {
			continue
		}
		var review completedReview
		if json.Unmarshal(event.Payload, &review) == nil && review.ReviewRound == round && !superseded[review.ReviewWorkOrderID] && (round > 0 || review.ReviewWorkOrderID == workOrderID) {
			reviews = append(reviews, review)
		}
	}
	if required == 0 {
		required = 1
	}
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].ReviewSeat < reviews[j].ReviewSeat })
	return reviews, required
}

func aggregateReviewRound(round int, reviews []completedReview) reviewRoundResult {
	if round == 0 && len(reviews) == 1 {
		review := reviews[0]
		result := reviewRoundResult{ReviewRound: round, Verdict: review.Verdict, ReasonCode: review.ReasonCode, Summary: review.Summary, Feedback: review.Feedback, Reviews: reviews}
		if review.Verdict == "approve" {
			result.ApprovedHeadSHA = review.ReviewedCommitSHA
		}
		return result
	}
	result := reviewRoundResult{ReviewRound: round, Verdict: "approve", ReasonCode: "approved", Summary: "All review panel seats approved.", Reviews: reviews}
	var feedback []string
	for _, review := range reviews {
		if review.Verdict == "changes_requested" {
			result.Verdict, result.ReasonCode, result.Summary = "changes_requested", "panel_changes_requested", "The review panel requested changes."
		}
		if strings.TrimSpace(review.Feedback) != "" {
			feedback = append(feedback, fmt.Sprintf("Seat %d (%s, %s): %s", review.ReviewSeat, review.RequiredModel, review.ModelEnforcement, strings.TrimSpace(review.Feedback)))
		}
	}
	if result.Verdict == "approve" && len(reviews) > 0 {
		result.ApprovedHeadSHA = reviews[0].ReviewedCommitSHA
		for _, review := range reviews[1:] {
			if review.ReviewedCommitSHA != result.ApprovedHeadSHA {
				result.Verdict, result.ReasonCode, result.Summary, result.ApprovedHeadSHA = "changes_requested", "review_head_mismatch", "Review seats evaluated different pull-request heads.", ""
				break
			}
		}
	}
	result.Feedback = strings.Join(feedback, "\n")
	return result
}

func reviewDecisionPayload(decision core.ReviewDecision) []byte {
	return core.JSONPayload(map[string]any{
		"review_work_order_id": decision.ReviewWorkOrderID, "verdict": decision.Verdict,
		"reason_code": decision.ReasonCode, "summary": decision.Summary, "feedback": decision.Feedback,
		"reviewed_commit_sha": decision.ReviewedCommitSHA, "reviewer": decision.Reviewer,
		"evidence_ids":            decision.EvidenceIDs,
		"requirement_citations":   decision.RequirementCitations,
		"done_criteria_coverage":  decision.DoneCriteriaAssessment,
		"governance_assessment":   decision.GovernanceAssessment,
		"verification_assessment": decision.VerificationAssessment,
		"reviewer_model":          decision.ReviewerModel, "reviewer_session": decision.ReviewerSession,
		"same_model_as_implementer": decision.SameModelAsImplementer,
		"review_round":              decision.ReviewRound, "review_seat": decision.ReviewSeat,
		"review_kind": decision.ReviewKind, "review_scope": decision.ReviewScope,
		"baseline_sha": decision.BaselineSHA, "head_sha": decision.HeadSHA,
		"required_model": decision.RequiredModel, "required_harness": decision.RequiredHarness,
		"required_effort":      decision.RequiredEffort,
		"model_enforcement":    decision.ModelEnforcement,
		"publication_eligible": decision.PublicationEligible,
	})
}

func (m *memory) CreateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder) error {
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdCreate)) {
		return fmt.Errorf("work-order create requires a valid taskops lease")
	}
	if !core.ValidWorkOrderStage(order.Stage) {
		return fmt.Errorf("invalid work-order stage %s", order.Stage)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[order.TaskID]
	if !ok {
		return fmt.Errorf("task %s not found", order.TaskID)
	}
	if selected, present := WorkspaceFromContext(ctx); present && task.Workspace != selected {
		return fmt.Errorf("task %s belongs to workspace %s, not %s", order.TaskID, task.Workspace, selected)
	}
	job, _, ok := m.findJobLocked(order.JobID)
	if !ok {
		return fmt.Errorf("job %s not found", order.JobID)
	}
	if job.TaskID != order.TaskID || job.Stage != order.Stage {
		return fmt.Errorf("work order job %s is not linked to task %s at stage %s", order.JobID, order.TaskID, order.Stage)
	}
	if _, exists := m.workOrders[order.ID]; exists {
		return fmt.Errorf("work order %s already exists", order.ID)
	}
	now := time.Now().UTC()
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	if order.QueueEnteredAt.IsZero() {
		order.QueueEnteredAt = order.CreatedAt
	}
	if order.QueueDeadline.IsZero() {
		order.QueueDeadline = order.QueueEnteredAt.Add(24 * time.Hour)
	}
	order.UpdatedAt = now
	expected, err := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
	if err != nil {
		return err
	}
	if order.State == "" {
		order.State = expected
	} else if order.State != expected {
		return &core.ErrInvalidTransition{Space: core.WorkOrderLifecycle, From: "", Command: string(core.WorkOrderCmdCreate), Allowed: []core.TransitionAlternative{{Command: string(core.WorkOrderCmdCreate), To: string(expected)}}}
	}
	if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && m.taskBlockedLocked(order.TaskID) {
		order.QueueBlockedAt = order.QueueEnteredAt
	}
	if order.Stage == core.StageImplement {
		m.repinTaskDesignContextLocked(ctx, order.TaskID, order.QueueEnteredAt)
	}
	order.Claimable = order.ClaimableAt(now)
	if !order.QueueBlockedAt.IsZero() {
		order.Claimable = false
	}
	order.OperatorDirection = m.firstOrderDirectionLocked(order.TaskID, order.OperatorDirection)
	m.workOrders[order.ID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	m.retireWorkOrderSiblingsLocked(ctx, order, "superseded by successor creation", now, true)
	return nil
}

func (m *memory) CreateReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	if !lease.ValidForCommand(taskID, string(core.WorkOrderCmdCreate)) {
		return fmt.Errorf("review-round create requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	if selected, present := WorkspaceFromContext(ctx); present && selected != "" && task.Workspace != selected {
		return fmt.Errorf("task %s belongs to workspace %s, not %s", taskID, task.Workspace, selected)
	}
	var verifyOrders []core.WorkOrder
	for _, order := range m.workOrders {
		verifyOrders = append(verifyOrders, order)
	}
	if !core.VerifyReviewReady(task, verifyOrders) {
		return fmt.Errorf("review requires completed verification for the submitted head")
	}
	if len(jobs) == 0 || len(jobs) != len(orders) {
		return fmt.Errorf("review round requires one job per work order")
	}
	existing := 0
	for _, order := range orders {
		if _, ok := m.workOrders[order.ID]; ok {
			existing++
		}
	}
	if existing == len(orders) {
		return nil
	}
	if existing != 0 {
		return fmt.Errorf("review round %d is only partially persisted", orders[0].ReviewRound)
	}
	for i, job := range jobs {
		if job.TaskID != taskID || job.Stage != core.StageReview || orders[i].TaskID != taskID || orders[i].JobID != job.ID {
			return fmt.Errorf("invalid review round member %d", i)
		}
		if _, _, exists := m.findJobLocked(job.ID); exists {
			return fmt.Errorf("%w: job %s already exists", ErrDispatchJobConflict, job.ID)
		}
	}
	now := time.Now().UTC()
	for i, job := range jobs {
		m.jobs[taskID] = append(m.jobs[taskID], job)
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job)})
		order := orders[i]
		if order.CreatedAt.IsZero() {
			order.CreatedAt = now
		}
		if order.QueueEnteredAt.IsZero() {
			order.QueueEnteredAt = order.CreatedAt
		}
		if order.QueueDeadline.IsZero() {
			order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
		}
		state, transitionErr := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
		if transitionErr != nil {
			return transitionErr
		}
		order.State, order.Claimable, order.UpdatedAt = state, true, now
		m.workOrders[order.ID] = order
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	}
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "review.round_created", Payload: core.JSONPayload(map[string]any{"review_round": orders[0].ReviewRound, "seat_count": len(orders)})})
	return nil
}

func (m *memory) CreateStageWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, job core.Job, order core.WorkOrder) (bool, error) {
	if !lease.ValidForCommand(job.TaskID, string(core.WorkOrderCmdCreate)) {
		return false, fmt.Errorf("stage work-order create requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[job.TaskID]
	if !ok {
		return false, fmt.Errorf("task %s not found", job.TaskID)
	}
	if selected, present := WorkspaceFromContext(ctx); present && selected != "" && task.Workspace != selected {
		return false, fmt.Errorf("task %s belongs to workspace %s, not %s", job.TaskID, task.Workspace, selected)
	}
	if !core.ValidWorkOrderStage(order.Stage) || job.Stage == core.StageReview || order.Stage != job.Stage || order.TaskID != job.TaskID || order.JobID != job.ID || order.ID != job.ID {
		return false, fmt.Errorf("invalid stage work order %s", order.ID)
	}
	if err := core.ValidateVerifyDispatch(task, order); err != nil {
		return false, err
	}
	for _, existing := range m.workOrders {
		if existing.TaskID == job.TaskID && existing.Stage == job.Stage &&
			(existing.State == core.WorkOrderQueued || existing.State == core.WorkOrderClaimed) {
			return false, nil
		}
	}
	if _, exists := m.workOrders[order.ID]; exists {
		return false, nil
	}
	if _, _, exists := m.findJobLocked(job.ID); exists {
		return false, fmt.Errorf("%w: job %s already exists without work order", ErrDispatchJobConflict, job.ID)
	}
	now := time.Now().UTC()
	m.jobs[job.TaskID] = append(m.jobs[job.TaskID], job)
	m.appendEventLocked(ctx, core.Event{TaskID: job.TaskID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job)})
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	if order.QueueEnteredAt.IsZero() {
		order.QueueEnteredAt = order.CreatedAt
	}
	if order.QueueDeadline.IsZero() {
		order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
	}
	state, err := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
	if err != nil {
		return false, err
	}
	order.State, order.Claimable, order.UpdatedAt = state, true, now
	if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && m.taskBlockedLocked(order.TaskID) {
		order.QueueBlockedAt, order.Claimable = order.QueueEnteredAt, false
	}
	order.OperatorDirection = m.firstOrderDirectionLocked(order.TaskID, order.OperatorDirection)
	m.workOrders[order.ID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: job.TaskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	return true, nil
}

func (m *memory) taskBlockedLocked(taskID string) bool {
	for dependencyID := range m.dependencies[taskID] {
		if dependency, exists := m.tasks[dependencyID]; exists && dependency.State != core.TaskMerged {
			return true
		}
	}
	return false
}

func (m *memory) RetryReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, request ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (ReviewRoundRetryResult, error) {
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return ReviewRoundRetryResult{}, fmt.Errorf("review retry requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	request.RequestID = strings.TrimSpace(request.RequestID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.PRHead = strings.TrimSpace(request.PRHead)
	if request.RequestID == "" || request.Reason == "" || request.PRHead == "" {
		return ReviewRoundRetryResult{}, fmt.Errorf("review retry request_id, reason, and verified PR head are required")
	}
	workspace, _ := WorkspaceFromContext(ctx)
	key := request.RequestID
	if prior, ok := m.reviewRetries[key]; ok {
		if prior.Workspace != workspace || prior.Request != request {
			return ReviewRoundRetryResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", ErrReviewRetryConflict, request.RequestID)
		}
		return prior.Result, nil
	}
	task, ok := m.tasks[request.TaskID]
	if !ok || (workspace != "" && task.Workspace != workspace) {
		return ReviewRoundRetryResult{}, fmt.Errorf("task %s not found", request.TaskID)
	}
	if len(jobs) == 0 || len(jobs) != len(orders) {
		return ReviewRoundRetryResult{}, fmt.Errorf("review retry requires one job per work order")
	}
	now := time.Now().UTC()
	var taskOrders []core.WorkOrder
	for id, order := range m.workOrders {
		if order.TaskID != request.TaskID || order.Stage != core.StageReview {
			continue
		}
		order = m.refreshWorkOrderLocked(ctx, order, now)
		m.workOrders[id] = order
		taskOrders = append(taskOrders, order)
	}
	recovery := ReviewRecoveryNeeded(taskOrders, m.events[request.TaskID])
	if recovery == nil || recovery.PriorRound != request.PriorRound {
		return ReviewRoundRetryResult{}, fmt.Errorf("%w: task %s has no matching terminal timed-out review round", ErrReviewRetryConflict, request.TaskID)
	}
	newRound := request.PriorRound + 1
	for i, job := range jobs {
		order := orders[i]
		if job.TaskID != request.TaskID || job.Stage != core.StageReview || order.TaskID != request.TaskID || order.JobID != job.ID || order.Stage != core.StageReview || order.ReviewRound != newRound || order.ReviewSeat != i+1 {
			return ReviewRoundRetryResult{}, fmt.Errorf("invalid review retry member %d", i)
		}
		if _, _, exists := m.findJobLocked(job.ID); exists {
			return ReviewRoundRetryResult{}, fmt.Errorf("%w: %w: job %s already exists", ErrReviewRetryConflict, ErrDispatchJobConflict, job.ID)
		}
		if _, exists := m.workOrders[order.ID]; exists {
			return ReviewRoundRetryResult{}, fmt.Errorf("%w: work order %s already exists", ErrReviewRetryConflict, order.ID)
		}
	}
	created := make([]core.WorkOrder, 0, len(orders))
	for i, job := range jobs {
		m.jobs[request.TaskID] = append(m.jobs[request.TaskID], job)
		m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job), At: now})
		order := orders[i]
		if order.CreatedAt.IsZero() {
			order.CreatedAt = now
		}
		if order.QueueEnteredAt.IsZero() {
			order.QueueEnteredAt = order.CreatedAt
		}
		if order.QueueDeadline.IsZero() {
			order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
		}
		state, transitionErr := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
		if transitionErr != nil {
			return ReviewRoundRetryResult{}, transitionErr
		}
		order.State, order.Claimable, order.UpdatedAt = state, true, now
		m.workOrders[order.ID] = order
		created = append(created, order)
		m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order), At: now})
	}
	actor := ActorFromContext(ctx)
	timedOutIDs := make([]string, 0, len(recovery.TimedOutOrders))
	for _, order := range recovery.TimedOutOrders {
		timedOutIDs = append(timedOutIDs, order.ID)
	}
	inconsistentIDs := make([]string, 0, len(recovery.InconsistentOrders))
	for _, order := range recovery.InconsistentOrders {
		inconsistentIDs = append(inconsistentIDs, order.ID)
	}
	payload := map[string]any{"request_id": request.RequestID, "workspace_id": workspace, "task_id": request.TaskID, "actor": actor.ID, "reason": request.Reason, "prior_round": request.PriorRound, "new_round": newRound, "pr_head": request.PRHead, "timed_out_work_order_ids": timedOutIDs, "inconsistent_work_order_ids": inconsistentIDs, "setup_name": task.SetupName, "setup_contract": task.SetupContract}
	m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, Kind: "review.round_retried", Payload: core.JSONPayload(payload), At: now})
	m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, Kind: "review.round_created", Payload: core.JSONPayload(map[string]any{"review_round": newRound, "seat_count": len(created), "retry_request_id": request.RequestID}), At: now})
	result := ReviewRoundRetryResult{RequestID: request.RequestID, TaskID: request.TaskID, PriorRound: request.PriorRound, NewRound: newRound, PRHead: request.PRHead, WorkOrders: created}
	m.reviewRetries[key] = memoryReviewRoundRetry{Workspace: workspace, Request: request, Result: result}
	return result, nil
}

func (m *memory) RecoverInterruptedReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, request InterruptedReviewRecoveryRequest, queueTimeout time.Duration) (InterruptedReviewRecoveryResult, error) {
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdRecover)) {
		return InterruptedReviewRecoveryResult{}, fmt.Errorf("interrupted review recovery requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.RequestID == "" || request.TaskID == "" || request.Round <= 0 || queueTimeout <= 0 {
		return InterruptedReviewRecoveryResult{}, fmt.Errorf("interrupted review recovery requires task, request_id, round, and queue timeout")
	}
	workspace, _ := WorkspaceFromContext(ctx)
	key := request.RequestID
	if prior, ok := m.interruptedReviewRecoveries[key]; ok {
		if prior.Workspace != workspace || prior.Request.TaskID != request.TaskID || prior.Request.RequestID != request.RequestID || prior.Request.Round != request.Round {
			return InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", ErrReviewRetryConflict, request.RequestID)
		}
		return prior.Result, nil
	}
	task, ok := m.tasks[request.TaskID]
	if !ok || task.Workspace != workspace {
		return InterruptedReviewRecoveryResult{}, fmt.Errorf("task %s not found", request.TaskID)
	}
	if core.TaskTerminal(task.State) {
		return InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: terminal task %s cannot recover review work", ErrReviewRetryConflict, request.TaskID)
	}
	now := time.Now().UTC()
	latestRound := 0
	for _, order := range m.workOrders {
		if order.TaskID == request.TaskID && order.Stage == core.StageReview && order.ReviewRound > latestRound {
			latestRound = order.ReviewRound
		}
	}
	verdicts := completedReviewWorkOrderIDs(m.events[request.TaskID], latestRound)
	var taskOrders []core.WorkOrder
	for id, order := range m.workOrders {
		if order.TaskID != request.TaskID || order.Stage != core.StageReview {
			continue
		}
		// Durable verdict evidence owns the seat even when an old queue row's
		// clocks have elapsed. Recovery must not rewrite that historical row.
		if !verdicts[order.ID] {
			order = m.refreshWorkOrderLocked(ctx, order, now)
			m.workOrders[id] = order
		}
		taskOrders = append(taskOrders, order)
	}
	recovery := InterruptedReviewRecoveryNeeded(task, CurrentReviewOrders(taskOrders, m.events[request.TaskID]), m.events[request.TaskID])
	if recovery == nil || recovery.ReviewRound != request.Round {
		return InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: task %s has no matching interrupted review round", ErrReviewRetryConflict, request.TaskID)
	}
	result := InterruptedReviewRecoveryResult{
		RequestID:       request.RequestID,
		TaskID:          request.TaskID,
		ReviewRound:     request.Round,
		RecoveredOrders: make([]core.WorkOrder, 0, len(recovery.EligibleOrders)),
		RetainedOrders:  append(make([]core.WorkOrder, 0, len(recovery.RetainedOrders)), recovery.RetainedOrders...),
	}
	actor := ActorFromContext(ctx)
	for _, eligible := range recovery.EligibleOrders {
		change := request.Refreezes[eligible.ID]
		if change == nil {
			continue
		}
		priorContract := task.SetupContract
		task.SetupContract = change.Setup
		m.tasks[task.ID] = task
		if !reflect.DeepEqual(priorContract, change.Setup) {
			m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: eligible.JobID, Kind: "task.setup.refrozen", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"prior": priorContract, "new": change.Setup, "request_id": request.RequestID, "work_order_id": eligible.ID, "actor": actor.ID}), At: now})
		}
		break
	}
	for _, eligible := range recovery.EligibleOrders {
		order := m.workOrders[eligible.ID]
		priorOutcome := order.LastAttemptOutcome
		order.LastAttemptOutcome = ""
		order.RetrySuppressed = false
		order.RetrySuppressionReason = ""
		order.AutomaticRetryCount = 0
		order.NextRetryAt = time.Time{}
		order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
		order.RedispatchCount++
		order.UpdatedAt, order.Claimable = now, true
		order.ClearExecutionPins()
		if change := request.Refreezes[order.ID]; change != nil {
			order.ExecutionTimeoutText = change.ExecutionTimeoutText
		}
		m.workOrders[order.ID] = order
		result.RecoveredOrders = append(result.RecoveredOrders, order)
		m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, JobID: order.JobID, Kind: "review.seat_recovered", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "review_round": request.Round, "review_seat": order.ReviewSeat, "work_order_id": order.ID, "request_id": request.RequestID, "prior_state": core.WorkOrderQueued, "prior_outcome": priorOutcome, "resulting_state": order.State, "outcome": "recovered", "setup_name": task.SetupName, "setup_contract": task.SetupContract}), At: now})
	}
	for _, retained := range recovery.RetainedOrders {
		m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, JobID: retained.JobID, Kind: "review.seat_recovery_skipped", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "review_round": request.Round, "review_seat": retained.ReviewSeat, "work_order_id": retained.ID, "request_id": request.RequestID, "prior_state": retained.State, "resulting_state": retained.State, "outcome": "retained_completed", "setup_name": task.SetupName, "setup_contract": task.SetupContract}), At: now})
	}
	m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, Kind: "review.round_recovered", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "review_round": request.Round, "request_id": request.RequestID, "actor": actor.ID, "recovered_seats": len(result.RecoveredOrders), "retained_completed_seats": len(result.RetainedOrders)}), At: now})
	m.interruptedReviewRecoveries[key] = memoryInterruptedReviewRecovery{Workspace: workspace, Request: request, Result: result}
	return result, nil
}

func (m *memory) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	if selected, present := WorkspaceFromContext(ctx); present && selected != "" && m.tasks[order.TaskID].Workspace != selected {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	order.Assignee = m.tasks[order.TaskID].Assignee
	return ProjectWorkOrderAt(order, time.Now().UTC()), nil
}

func (m *memory) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now()
	orders := make([]core.WorkOrder, 0, len(m.workOrders))
	for _, order := range m.workOrders {
		order.Assignee = m.tasks[order.TaskID].Assignee
		orders = append(orders, ProjectWorkOrderAt(order, now))
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.Before(orders[j].CreatedAt) })
	return orders, nil
}

func (m *memory) ListWorkOrdersForTasks(ctx context.Context, taskIDs []string) ([]core.WorkOrder, error) {
	if len(taskIDs) == 0 {
		return []core.WorkOrder{}, nil
	}
	wanted := make(map[string]bool, len(taskIDs))
	for _, taskID := range taskIDs {
		wanted[taskID] = true
	}
	orders, err := m.ListWorkOrders(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(orders, func(order core.WorkOrder) bool { return !wanted[order.TaskID] }), nil
}

func (m *memory) ListCheckpointContextCandidates(ctx context.Context, requirementID string) ([]CheckpointContextCandidate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	result := []CheckpointContextCandidate{}
	for _, task := range m.tasks {
		if task.Workspace != workspace || core.TaskTerminal(task.State) {
			continue
		}
		attached, _ := ActiveTaskContextReferences(ChronologicalTaskEvents(m.events[task.ID]))
		if attached[requirementID] {
			continue
		}
		var latest *core.WorkOrder
		for _, order := range m.workOrders {
			if order.TaskID != task.ID || latest != nil &&
				(latest.CreatedAt.After(order.CreatedAt) || latest.CreatedAt.Equal(order.CreatedAt) && latest.ID >= order.ID) {
				continue
			}
			copy := order
			latest = &copy
		}
		if latest == nil || latest.State != core.WorkOrderQueued ||
			latest.LastAttemptOutcome != core.WorkOrderOutcomeReleased ||
			latest.LastFailureMessage != core.WorkOrderReleaseReasonOperatorCheckpointReached {
			continue
		}
		result = append(result, CheckpointContextCandidate{ID: task.ID, Title: task.Title, State: task.State})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// ProjectWorkOrderAt applies elapsed clock semantics to a copy for
// observational responses. It performs no store writes; the queue order clock
// persists the same canonical commands asynchronously.
func ProjectWorkOrderAt(order core.WorkOrder, now time.Time) core.WorkOrder {
	if (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed) &&
		!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now) {
		if order.State == core.WorkOrderClaimed {
			order.LastAttemptID = order.AttemptID
			clearClaimOwnership(&order)
		}
		order.State = core.WorkOrderTimedOut
		order.Claimable = false
		order.LeaseExpiresAt = time.Time{}
		return order
	}
	if order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.IsZero() && !order.LeaseExpiresAt.After(now) {
		taskRunClaim := core.IsTaskRunClaimantID(order.ClaimantID)
		order.LastAttemptID = order.AttemptID
		clearActiveAttempt(&order)
		order.ClearExecutionPins()
		order.State = core.WorkOrderQueued
		order.Claimable = taskRunClaim
		order.LastAttemptOutcome = core.WorkOrderOutcomeExpired
		order.NextRetryAt = time.Time{}
		order.RetrySuppressed = !taskRunClaim
		return order
	}
	if order.State == core.WorkOrderQueued && !order.QueueBlockedAt.IsZero() {
		order.Claimable = false
		return order
	}
	if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
		order.QueueBlockedAt.IsZero() && !order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
		order.State = core.WorkOrderStale
		order.Claimable = false
		return order
	}
	order.Claimable = order.ClaimableAt(now)
	return order
}

func (m *memory) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, workspaceSelected := WorkspaceFromContext(ctx)
	now := time.Now().UTC()
	out := make([]core.WorkOrder, 0)
	for _, order := range m.workOrders {
		task, ok := m.tasks[order.TaskID]
		if !ok || order.TaskID != taskID || workspaceSelected && workspace != "" && task.Workspace != workspace {
			continue
		}
		order.Assignee = task.Assignee
		out = append(out, ProjectWorkOrderAt(order, now))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (m *memory) ListTaskWorkOrdersSnapshot(_ context.Context, taskID string) ([]core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	orders := make([]core.WorkOrder, 0)
	for _, order := range m.workOrders {
		if order.TaskID == taskID {
			orders = append(orders, ProjectWorkOrderAt(order, now))
		}
	}
	sort.Slice(orders, func(i, j int) bool {
		if orders[i].CreatedAt.Equal(orders[j].CreatedAt) {
			return orders[i].ID < orders[j].ID
		}
		return orders[i].CreatedAt.Before(orders[j].CreatedAt)
	})
	return orders, nil
}

func (m *memory) ListElapsedWorkOrderTaskIDs(ctx context.Context, now time.Time) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	selected, _ := WorkspaceFromContext(ctx)
	seen := map[string]struct{}{}
	for _, order := range m.workOrders {
		if selected != "" && m.tasks[order.TaskID].Workspace != selected {
			continue
		}
		elapsedExecution := (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed) &&
			!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)
		elapsedClaim := order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.IsZero() && !order.LeaseExpiresAt.After(now)
		elapsedQueue := order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
			order.QueueBlockedAt.IsZero() && !order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now)
		if elapsedExecution || elapsedClaim || elapsedQueue {
			seen[order.TaskID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for taskID := range seen {
		result = append(result, taskID)
	}
	sort.Strings(result)
	return result, nil
}

func (m *memory) ApplyWorkOrderClock(ctx context.Context, lease taskops.TaskLease, taskID string, now time.Time) (int, error) {
	if !lease.ValidFor(taskID) {
		return 0, fmt.Errorf("work-order lifecycle mutation requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for id, order := range m.workOrders {
		if order.TaskID != taskID {
			continue
		}
		before := order.State
		order = m.refreshWorkOrderLocked(ctx, order, now)
		m.workOrders[id] = order
		if order.State != before {
			count++
		}
	}
	return count, nil
}

func (m *memory) ClaimWorkOrderCommand(ctx context.Context, lifecycleLease taskops.TaskLease, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	if !lifecycleLease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdClaim)) {
		return core.WorkOrder{}, fmt.Errorf("work-order claim requires a valid taskops lease")
	}
	if task := m.tasks[order.TaskID]; task.Assignee != nil && task.Assignee.UserID != claim.OwnerUserID {
		return core.WorkOrder{}, fmt.Errorf("task %s is assigned to %s; only that assignee may claim its work orders", order.TaskID, task.Assignee.UserID)
	}
	if !core.ValidWorkOrderStage(order.Stage) {
		return core.WorkOrder{}, fmt.Errorf("invalid work-order stage %s", order.Stage)
	}
	if err := core.ValidateVerifyDispatch(m.tasks[order.TaskID], order); err != nil {
		return core.WorkOrder{}, err
	}
	for _, other := range m.workOrders {
		if core.ConflictingExecutorClaims(order, other) {
			return core.WorkOrder{}, fmt.Errorf("conflicting claimed order %s", other.ID)
		}
	}
	now := time.Now().UTC()
	if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && !order.QueueBlockedAt.IsZero() && !m.taskBlockedLocked(order.TaskID) {
		order.QueueDeadline = order.QueueDeadline.Add(now.Sub(order.QueueBlockedAt))
		order.QueueBlockedAt = time.Time{}
		m.workOrders[id] = order
	}
	order = m.refreshWorkOrderLocked(ctx, order, now)
	if order.Stage == core.StageReview && m.reviewSeatAcceptedLocked(order) {
		return core.WorkOrder{}, fmt.Errorf("accepted review seat %s is terminal and cannot be claimed", id)
	}
	if order.Stage == core.StageReview || order.Stage == core.StageVerify {
		// The same predicate and read the listing uses (req-260810-70ce2f
		// AC-1.1; component-work-orders).
		if blocking := m.claimBlockingProposalsLocked(workspaceOrDefault(ctx, ""), order.TaskID); len(blocking) > 0 {
			return core.WorkOrder{}, ClaimBlockingProposalError(order.TaskID, blocking[0])
		}
	}
	if order.State == core.WorkOrderStale {
		return core.WorkOrder{}, fmt.Errorf("%w: %s", ErrWorkOrderStale, id)
	}
	if order.State == core.WorkOrderTimedOut {
		return core.WorkOrder{}, fmt.Errorf("%w: %s", ErrWorkOrderTimedOut, id)
	}
	if order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(now) {
		return core.WorkOrder{}, fmt.Errorf("work order %s is already claimed", id)
	}
	if order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not claimable", id)
	}
	if !order.ClaimableAt(now) {
		if order.RetrySuppressed {
			return core.WorkOrder{}, fmt.Errorf("work order %s automatic retry is suppressed; operator recovery is required", id)
		}
		return core.WorkOrder{}, fmt.Errorf("work order %s is in retry backoff until %s", id, order.NextRetryAt.Format(time.RFC3339Nano))
	}
	if order.Stage == core.StageImplement || order.Stage == core.StageVerify {
		var blockingTaskIDs []string
		for dependencyID := range m.dependencies[order.TaskID] {
			if dependency, ok := m.tasks[dependencyID]; ok && dependency.State != core.TaskMerged {
				blockingTaskIDs = append(blockingTaskIDs, dependencyID)
			}
		}
		if len(blockingTaskIDs) > 0 {
			sort.Strings(blockingTaskIDs)
			return core.WorkOrder{}, fmt.Errorf("task %s is blocked by unmerged dependencies: %s", order.TaskID, strings.Join(blockingTaskIDs, ", "))
		}
	}
	if order.Stage == core.StageReview || order.Stage == core.StageVerify {
		if order.ServedRequirementSnapshot == nil && claim.Requirements != nil {
			order.ServedRequirementSnapshot = append([]core.ServedRequirementContext{}, claim.Requirements...)
		}
		if claim.Governance != nil {
			if order.GovernanceSnapshot == nil {
				copy := *claim.Governance
				copy.Designs = append([]core.GovernanceDesignContext(nil), claim.Governance.Designs...)
				copy.Decisions = append([]core.Decision(nil), claim.Governance.Decisions...)
				copy.PendingDesignProposals = append([]core.PendingSystemDesignProposal(nil), claim.Governance.PendingDesignProposals...)
				copy.ResolutionNotes = append([]string(nil), claim.Governance.ResolutionNotes...)
				order.GovernanceSnapshot = &copy
			} else {
				// Proposal observations are claim-time-fresh, unlike the frozen
				// System Design and decision authority.
				order.GovernanceSnapshot.PendingDesignProposals = append([]core.PendingSystemDesignProposal(nil), claim.Governance.PendingDesignProposals...)
				order.GovernanceSnapshot.ResolutionNotes = append([]string(nil), claim.Governance.ResolutionNotes...)
			}
		}
	}
	if order.Stage == core.StageReview {
		for _, candidate := range m.workOrders {
			if candidate.ID != order.ID && candidate.TaskID == order.TaskID &&
				(candidate.Stage == core.StageImplement || (candidate.Stage == core.StageReview && candidate.ReviewRound == order.ReviewRound)) &&
				((claim.SessionID != "" && candidate.SessionID == claim.SessionID) || (claim.ClientToken != "" && candidate.ClientTokenHash == tokenHash(claim.ClientToken))) {
				return core.WorkOrder{}, fmt.Errorf("self-review forbidden: review session independence requires a fresh session and client token")
			}
		}
		for _, event := range m.events[order.TaskID] {
			if event.Kind != "work_order.claimed" {
				continue
			}
			var prior core.WorkOrder
			if json.Unmarshal(event.Payload, &prior) == nil && prior.ID != order.ID &&
				(prior.Stage == core.StageImplement || (prior.Stage == core.StageReview && prior.ReviewRound == order.ReviewRound)) &&
				claim.SessionID != "" && prior.SessionID == claim.SessionID {
				return core.WorkOrder{}, fmt.Errorf("self-review forbidden: review session independence requires a session not used by another seat or the implementer")
			}
		}
		if claim.WorkerID != "" {
			if order.RequiredModel != "" && claim.Model != order.RequiredModel {
				return core.WorkOrder{}, fmt.Errorf("worker review model %q does not match pinned seat model %q", claim.Model, order.RequiredModel)
			}
			order.ModelEnforcement = "worker-pinned"
		} else {
			order.ModelEnforcement = "self-reported"
		}
	}
	if !order.ExecutionStartedAt.IsZero() && order.ExecutionDeadline.IsZero() && claim.ExecutionTimeout > 0 {
		order.ExecutionDeadline = order.ExecutionStartedAt.Add(claim.ExecutionTimeout)
		m.workOrders[id] = order
		order = m.refreshWorkOrderLocked(ctx, order, now)
		if order.State == core.WorkOrderTimedOut {
			return core.WorkOrder{}, fmt.Errorf("%w: %s", ErrWorkOrderTimedOut, id)
		}
	}
	lease := claim.Lease
	if lease <= 0 {
		lease = core.DefaultWorkOrderClaimLease
	}
	next, transitionErr := core.TransitionWorkOrder(order.State, core.WorkOrderCmdClaim)
	if transitionErr != nil {
		return core.WorkOrder{}, transitionErr
	}
	order.State, order.ClaimantID, order.SessionID, order.AttemptID = next, claim.ClaimantID, claim.SessionID, core.NewWorkOrderAttemptID()
	order.Agent, order.Model, order.WorkerID, order.LeaseExpiresAt, order.UpdatedAt = claim.Agent, claim.Model, claim.WorkerID, now.Add(lease), now
	if claim.ClientToken != "" {
		order.ClientTokenHash = tokenHash(claim.ClientToken)
	}
	if order.ExecutionStartedAt.IsZero() {
		order.ExecutionStartedAt = now
		if claim.ExecutionTimeout > 0 {
			order.ExecutionDeadline = now.Add(claim.ExecutionTimeout)
		}
		if job, index, exists := m.findJobLocked(order.JobID); exists {
			job.StartedAt = now
			job.State = core.JobRunning
			job.ModelTier = claim.Model
			m.jobs[job.TaskID][index] = job
		}
	}
	order.Claimable = false
	m.workOrders[id] = order
	if task := m.tasks[order.TaskID]; task.State == core.TaskQueued {
		from := task.State
		state, taskErr := core.TransitionTask(from, core.TaskOrderClaim)
		if taskErr != nil {
			return core.WorkOrder{}, taskErr
		}
		task.State = state
		m.tasks[task.ID] = task
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": from, "to": state, "command": core.TaskOrderClaim})})
	}
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(order)})
	return order, nil
}

func (m *memory) RedispatchWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, queueTimeout time.Duration) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdRedispatch)) {
		return core.WorkOrder{}, fmt.Errorf("work-order redispatch requires a valid taskops lease")
	}
	now := time.Now().UTC()
	order = m.refreshWorkOrderLocked(ctx, order, now)
	if order.State != core.WorkOrderStale {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not stale and cannot be redispatched", id)
	}
	if !order.ExecutionStartedAt.IsZero() {
		return core.WorkOrder{}, fmt.Errorf("work order %s was already claimed and requires operator recovery", id)
	}
	var taskOrders []core.WorkOrder
	for _, candidate := range m.workOrders {
		if candidate.TaskID == order.TaskID {
			taskOrders = append(taskOrders, candidate)
		}
	}
	if err := WorkOrderRecoverySupersessionError(m.tasks[order.TaskID], order, taskOrders); err != nil {
		return core.WorkOrder{}, err
	}
	if queueTimeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	next, transitionErr := core.TransitionWorkOrder(order.State, core.WorkOrderCmdRedispatch)
	if transitionErr != nil {
		return core.WorkOrder{}, transitionErr
	}
	order.State, order.Claimable = next, true
	order.ClaimantID, order.SessionID, order.ClientTokenHash = "", "", ""
	order.Agent, order.Model, order.WorkerID, order.Progress = "", "", "", ""
	order.ModelEnforcement = ""
	order.LeaseExpiresAt = time.Time{}
	order.ClearExecutionPins()
	order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
	order.ExecutionStartedAt, order.ExecutionDeadline = time.Time{}, time.Time{}
	order.RedispatchCount++
	order.UpdatedAt = now
	if order.Stage == core.StageImplement {
		m.repinTaskDesignContextLocked(ctx, order.TaskID, now)
	}
	m.workOrders[id] = order
	if job, index, exists := m.findJobLocked(order.JobID); exists {
		job.State, job.StartedAt, job.EndedAt = core.JobPending, time.Time{}, time.Time{}
		m.jobs[job.TaskID][index] = job
	}
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.redispatched", Payload: core.JSONPayload(map[string]any{"work_order_id": id, "prior_state": core.WorkOrderStale, "new_state": order.State, "command": core.WorkOrderCmdRedispatch, "reason": "stale never-claimed queue redispatch"}), At: now})
	m.retireWorkOrderSiblingsLocked(ctx, order, "superseded by stale redispatch", now, false)
	return order, nil
}

func (m *memory) reviewSeatAcceptedLocked(order core.WorkOrder) bool {
	for i := len(m.events[order.TaskID]) - 1; i >= 0; i-- {
		event := m.events[order.TaskID][i]
		if event.Kind != "review.accepted" {
			continue
		}
		var payload struct {
			WorkOrderID string `json:"review_work_order_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID == order.ID {
			return true
		}
	}
	return false
}

func (m *memory) RecoverWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id, requestID, direction string, queueTimeout time.Duration, refreeze ...*RecoveryRefreeze) (core.WorkOrder, error) {
	clean, sanitationErr := SanitizeVerificationRecovery(ctx, nil)
	if sanitationErr != nil {
		return core.WorkOrder{}, sanitationErr
	}
	ctx = clean
	m.mu.Lock()
	defer m.mu.Unlock()
	var err error
	direction, err = core.NormalizeWorkOrderOperatorDirection(direction)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if strings.TrimSpace(requestID) == "" {
		return core.WorkOrder{}, fmt.Errorf("recovery request_id is required")
	}
	if queueTimeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	workspace, _ := WorkspaceFromContext(ctx)
	var verificationRows []VerificationRow
	var verificationEvents []core.Event
	if VerificationRecoveryFromContext(ctx) != nil {
		actor := ActorFromContext(ctx)
		member := memoryScopedKey{workspace: workspace, id: strings.TrimPrefix(actor.ID, "user:")}
		if actor.Role != core.ActorUser || !m.workspaceMembers[member] || !core.RoleAllows(m.workspaceMemberRoles[member], core.CapabilityOperateGates) {
			return core.WorkOrder{}, ErrVerificationAccess
		}
		old := m.workOrders[id]
		verificationRows, verificationEvents, err = PrepareVerificationRecovery(ctx, m.tasks[old.TaskID], old, m.verificationRowsLocked(workspace, old.TaskID), requestID, time.Now().UTC())
		if err != nil {
			return core.WorkOrder{}, err
		}
		if err = VerificationFault(ctx, "recovery"); err != nil {
			return core.WorkOrder{}, err
		}
	}
	key := workspace + "/" + id + "/" + requestID
	if _, exists := m.recoveries[key]; exists {
		order, ok := m.workOrders[id]
		if !ok {
			return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
		}
		if workspace != "" && m.tasks[order.TaskID].Workspace != workspace {
			return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
		}
		return order, nil
	}
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdRecover)) {
		return core.WorkOrder{}, fmt.Errorf("work-order recovery requires a valid taskops lease")
	}
	if workspace != "" && m.tasks[order.TaskID].Workspace != workspace {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	if VerificationRecoveryFromContext(ctx) == nil {
		order = m.refreshWorkOrderLocked(ctx, order, time.Now().UTC())
	}
	eligibleQueued := order.State == core.WorkOrderQueued && (order.LastAttemptOutcome != "" || order.RetrySuppressed || !order.NextRetryAt.IsZero())
	if !eligibleQueued && order.State != core.WorkOrderStale && order.State != core.WorkOrderTimedOut {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not released, expired, or retry-suppressed", id)
	}
	var taskOrders []core.WorkOrder
	for _, candidate := range m.workOrders {
		if candidate.TaskID == order.TaskID {
			taskOrders = append(taskOrders, candidate)
		}
	}
	if err = WorkOrderRecoverySupersessionError(m.tasks[order.TaskID], order, taskOrders); err != nil {
		return core.WorkOrder{}, err
	}
	now := time.Now().UTC()
	prior := order.LastAttemptOutcome
	priorAttemptID := order.LastAttemptID
	priorState := order.State
	priorFailureCategory := order.LastFailureCategory
	priorTransientFailures := m.previousTransientFailuresLocked(order)
	priorNextRetryAt := order.NextRetryAt
	clearActiveAttempt(&order)
	lifecycleCommand := core.WorkOrderCmdRecover
	eventKind := "work_order.recovered"
	if priorState == core.WorkOrderQueued {
		// Resetting retry metadata on an already-queued order is not a lifecycle
		// transition. Keep the historical event kind without mislabeling this
		// operator action as W14.
		lifecycleCommand = ""
		eventKind = "work_order.redispatched"
	}
	next := priorState
	if lifecycleCommand != "" {
		var transitionErr error
		next, transitionErr = core.TransitionWorkOrder(priorState, lifecycleCommand)
		if transitionErr != nil {
			return core.WorkOrder{}, transitionErr
		}
	}
	order.State = next
	order.LastAttemptOutcome = ""
	order.RetrySuppressed = false
	order.RetrySuppressionReason = ""
	order.AutomaticRetryCount = 0
	order.NextRetryAt = time.Time{}
	order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
	order.RedispatchCount++
	order.OperatorDirection = direction
	order.UpdatedAt = now
	order.Claimable = true
	order.ClearExecutionPins()
	if order.Stage == core.StageImplement {
		m.repinTaskDesignContextLocked(ctx, order.TaskID, now)
	}
	if len(refreeze) != 0 && refreeze[0] != nil {
		change := refreeze[0]
		task := m.tasks[order.TaskID]
		priorContract := task.SetupContract
		task.SetupContract = change.Setup
		m.tasks[task.ID] = task
		order.ExecutionTimeoutText = change.ExecutionTimeoutText
		if !reflect.DeepEqual(priorContract, change.Setup) {
			actor := ActorFromContext(ctx)
			m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "task.setup.refrozen", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"prior": priorContract, "new": change.Setup, "request_id": requestID, "work_order_id": order.ID, "actor": actor.ID}), At: now})
		}
	}
	for _, row := range verificationRows {
		m.verificationRows[workspace+"\x00"+row.Table+"\x00"+row.ID] = row
	}
	for _, event := range verificationEvents {
		m.appendEventLocked(ctx, event)
	}
	m.workOrders[id] = order
	if job, index, exists := m.findJobLocked(order.JobID); exists {
		job.State, job.StartedAt, job.EndedAt = core.JobPending, time.Time{}, time.Time{}
		m.jobs[job.TaskID][index] = job
	}
	m.recoveries[key] = struct{}{}
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: eventKind, Payload: core.JSONPayload(map[string]any{"attempt_id": priorAttemptID, "workspace_id": workspace, "work_order_id": id, "request_id": requestID, "prior_state": priorState, "prior_outcome": prior, "new_state": order.State, "command": lifecycleCommand, "reason": "operator recovery", "direction": direction, "failure_category": priorFailureCategory, "consecutive_transient_failures": priorTransientFailures, "next_retry_at": priorNextRetryAt}), At: now})
	return order, nil
}

func (m *memory) repinTaskDesignContextLocked(ctx context.Context, taskID string, at time.Time) {
	task, ok := m.tasks[taskID]
	if !ok {
		return
	}
	_, pinned := ActiveTaskContextReferences(m.events[taskID])
	confirmed := make(map[string]int, len(pinned))
	for id := range pinned {
		if document, exists := m.systemDesigns[memoryScopedKey{workspace: task.Workspace, id: id}]; exists {
			confirmed[id] = document.CurrentVersion
		}
	}
	for _, design := range AdvancedTaskContextDesignPins(pinned, confirmed) {
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextDesignAdded, At: at,
			Payload: core.JSONPayload(map[string]any{"id": design.ID, "version": design.Version})})
	}
}

func (m *memory) refreshWorkOrderLocked(ctx context.Context, order core.WorkOrder, now time.Time) core.WorkOrder {
	if (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed) &&
		!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now) {
		next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdTimeout)
		if err != nil {
			return order
		}
		attemptID := ""
		if order.State == core.WorkOrderClaimed {
			attemptID = order.AttemptID
			order.LastAttemptID = attemptID
			clearClaimOwnership(&order)
		}
		order.State, order.Claimable = next, false
		order.LeaseExpiresAt = time.Time{}
		order.UpdatedAt = now
		m.workOrders[order.ID] = order
		if job, index, exists := m.findJobLocked(order.JobID); exists {
			job.State, job.EndedAt = core.JobFailed, order.ExecutionDeadline
			m.jobs[job.TaskID][index] = job
		}
		eventOrder := order
		eventOrder.AttemptID = attemptID
		m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.timed_out", Payload: core.JSONPayload(eventOrder), At: now})
		return order
	}
	if order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.After(now) {
		next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdExpire)
		if err != nil {
			return order
		}
		attemptID := order.AttemptID
		taskRunClaim := core.IsTaskRunClaimantID(order.ClaimantID)
		order.LastAttemptID = attemptID
		clearActiveAttempt(&order)
		order.ClearExecutionPins()
		order.State, order.Claimable = next, taskRunClaim
		order.LastAttemptOutcome = core.WorkOrderOutcomeExpired
		order.NextRetryAt = time.Time{}
		order.RetrySuppressed = !taskRunClaim
		order.UpdatedAt = now
		m.workOrders[order.ID] = order
		if job, index, exists := m.findJobLocked(order.JobID); exists {
			job.State, job.StartedAt, job.EndedAt = core.JobPending, time.Time{}, time.Time{}
			m.jobs[job.TaskID][index] = job
		}
		m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.expired", Payload: core.JSONPayload(map[string]any{"attempt_id": attemptID, "outcome": order.LastAttemptOutcome, "release_cause": core.WorkOrderReleaseCauseLeaseLoss, "retry_suppressed": order.RetrySuppressed}), At: now})
	}
	if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
		order.QueueBlockedAt.IsZero() && !order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
		next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdMarkStale)
		if err != nil {
			return order
		}
		order.State, order.Claimable, order.UpdatedAt = next, false, now
		m.workOrders[order.ID] = order
		m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.stale", Payload: core.JSONPayload(order), At: now})
		return order
	}
	order.Claimable = order.ClaimableAt(now)
	return order
}

func tokenHash(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

func (m *memory) UpdateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder, commands ...core.WorkOrderCommand) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateWorkOrderCommandLocked(ctx, lease, order, commands...)
}

func (m *memory) updateWorkOrderCommandLocked(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder, commands ...core.WorkOrderCommand) error {
	current, ok := m.workOrders[order.ID]
	if !ok {
		return fmt.Errorf("work order %s not found", order.ID)
	}
	current = m.refreshWorkOrderLocked(ctx, current, time.Now().UTC())
	if current.State == core.WorkOrderTimedOut {
		return fmt.Errorf("%w: %s", ErrWorkOrderTimedOut, order.ID)
	}
	if current.State == core.WorkOrderStale {
		return fmt.Errorf("%w: %s", ErrWorkOrderStale, order.ID)
	}
	if current.State == core.WorkOrderCancelled {
		return fmt.Errorf("%w: %s", ErrWorkOrderCancelled, order.ID)
	}
	if updateRequiresClaim(order.State, current.State) &&
		(current.State != core.WorkOrderClaimed || current.SessionID == "" || current.SessionID != order.SessionID) {
		return fmt.Errorf("work order %s is not claimed by this session", order.ID)
	}
	command := taskops.WorkOrderMetadataCommand
	if len(commands) == 1 {
		command = commands[0]
	} else if current.State != order.State {
		if inferred, inferredOK := InferWorkOrderUpdateCommand(current, order); inferredOK {
			command = inferred
		}
	}
	if !lease.ValidForCommand(order.TaskID, string(command)) {
		return fmt.Errorf("work-order update requires a valid taskops lease")
	}
	if current.State != order.State {
		if len(commands) == 0 {
			if inferred, ok := InferWorkOrderUpdateCommand(current, order); ok {
				commands = []core.WorkOrderCommand{inferred}
			}
		}
		if len(commands) != 1 {
			return fmt.Errorf("work order %s state change requires exactly one lifecycle command", order.ID)
		}
		to, err := core.TransitionWorkOrder(current.State, commands[0])
		if err != nil {
			return err
		}
		if to != order.State {
			return &core.ErrInvalidTransition{Space: core.WorkOrderLifecycle, From: string(current.State), Command: string(commands[0]), Allowed: core.WorkOrderTransitionAlternatives(current.State)}
		}
	}
	if order.State == core.WorkOrderCompleted {
		order.OperatorDirection = ""
		order.ContinuationSessionID, order.ContinuationAttemptID = "", ""
		order.ContinuationHarness, order.ContinuationLaunchEnvironment = "", ""
	}
	if command == core.WorkOrderCmdSubmitForReview && order.Stage == core.StageImplement && order.HeadSHA != "" {
		task := m.tasks[order.TaskID]
		task.ReviewedHeadSHA = order.HeadSHA
		m.supersedeVerificationLocked(ctx, task.ID, order.HeadSHA)
		m.tasks[order.TaskID] = task
	}
	order.UpdatedAt = time.Now().UTC()
	m.workOrders[order.ID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order)})
	if current.State != core.WorkOrderCompleted && order.State == core.WorkOrderCompleted {
		m.retireWorkOrderSiblingsLocked(ctx, order, "stage completed", order.UpdatedAt, false)
	}
	return nil
}

func updateRequiresClaim(next, current core.WorkOrderState) bool {
	if next == core.WorkOrderClaimed {
		return true
	}
	return current != next && (next == core.WorkOrderSubmitted || next == core.WorkOrderCompleted)
}

// InferWorkOrderUpdateCommand preserves the legacy whole-record update API
// while routing every actual state change through a named lifecycle command
// (component-persistence).
func InferWorkOrderUpdateCommand(current, next core.WorkOrder) (core.WorkOrderCommand, bool) {
	switch {
	case current.State == core.WorkOrderQueued && next.State == core.WorkOrderClaimed:
		return core.WorkOrderCmdClaim, true
	case current.State == core.WorkOrderClaimed && next.State == core.WorkOrderSubmitted:
		return core.WorkOrderCmdSubmitForReview, true
	case current.State == core.WorkOrderClaimed && next.State == core.WorkOrderCompleted && next.Stage == core.StageSpec:
		return core.WorkOrderCmdSubmitSpec, true
	case current.State == core.WorkOrderClaimed && next.State == core.WorkOrderCompleted && next.Stage == core.StageVerify:
		return core.WorkOrderCmdSubmitVerification, true
	case current.State == core.WorkOrderClaimed && next.State == core.WorkOrderCompleted && next.Stage == core.StageReview:
		return core.WorkOrderCmdSubmitReviewVerdict, true
	case current.State == core.WorkOrderSubmitted && next.State == core.WorkOrderCompleted:
		return core.WorkOrderCmdReviewTerminal, true
	case current.State == core.WorkOrderSubmitted && next.State == core.WorkOrderClaimed:
		return core.WorkOrderCmdReviewRevise, true
	default:
		return "", false
	}
}

func (m *memory) CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[spec.TaskID]; !ok {
		return core.SpecVersion{}, fmt.Errorf("task %s not found", spec.TaskID)
	}
	spec.Version = len(m.specs[spec.TaskID]) + 1
	// Approval is a separate exact-version gate; callers cannot smuggle an
	// approved artifact through creation. This matches the Postgres contract.
	spec.Approved = false
	spec.ApprovedAt = time.Time{}
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	m.specs[spec.TaskID] = append(m.specs[spec.TaskID], spec)
	m.appendEventLocked(ctx, core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount})})
	return spec, nil
}

func (m *memory) GetLatestSpecVersion(_ context.Context, taskID string) (core.SpecVersion, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.specs[taskID]
	if len(versions) == 0 {
		return core.SpecVersion{}, false, nil
	}
	return versions[len(versions)-1], true, nil
}

func (m *memory) GetSpecVersion(ctx context.Context, taskID string, version int) (core.SpecVersion, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if selected, scoped := WorkspaceFromContext(ctx); scoped {
		task, ok := m.tasks[taskID]
		if !ok || task.Workspace != selected {
			return core.SpecVersion{}, false, nil
		}
	}
	for _, spec := range m.specs[taskID] {
		if spec.Version == version {
			return spec, true, nil
		}
	}
	return core.SpecVersion{}, false, nil
}

// GetApprovedSpecVersion returns the newest approved spec version, which is
// the one that governs: approval only ever lands on the newest
// version, so a later unapproved draft is a proposal that has materialized
// nothing and must not displace the blueprint currently in delivery.
func (m *memory) GetApprovedSpecVersion(_ context.Context, taskID string) (core.SpecVersion, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.specs[taskID]
	for index := len(versions) - 1; index >= 0; index-- {
		if versions[index].Approved {
			return versions[index], true, nil
		}
	}
	return core.SpecVersion{}, false, nil
}

func (m *memory) ApproveSpecVersion(ctx context.Context, taskID string, version int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.specs[taskID]
	if len(versions) == 0 || versions[len(versions)-1].Version != version {
		return fmt.Errorf("spec version %d for task %s not found or superseded", version, taskID)
	}
	versions[len(versions)-1].Approved = true
	versions[len(versions)-1].ApprovedAt = time.Now().UTC()
	m.specs[taskID] = versions
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})})
	return nil
}

func (m *memory) firstOrderDirectionLocked(taskID, current string) string {
	for _, order := range m.workOrders {
		if order.TaskID == taskID {
			return current
		}
	}
	if direction := m.tasks[taskID].IntakeOperatorDirection; direction != "" {
		return direction
	}
	return current
}
