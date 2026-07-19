// Package store holds event-sourced control-plane state behind an interface.
// The memory implementation is for unit tests and explicit local development;
// Postgres is the durable Phase 2 implementation (spec §16, §19).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

var (
	ErrWorkspaceRequired   = errors.New("workspace context is required")
	ErrWorkspaceConflict   = errors.New("workspace id or name already exists")
	ErrWorkOrderStale      = errors.New("work order is stale and requires redispatch")
	ErrWorkOrderTimedOut   = errors.New("work order execution deadline exceeded")
	ErrReviewRetryConflict = errors.New("review round retry conflicts with current state")
	ErrPairingInvalid      = errors.New("worker pairing token is invalid, expired, or already used")
	ErrWorkerUnauthorized  = errors.New("worker credential is invalid or revoked")
)

// WorkspaceControlStore owns durable workspace resources independently of a
// workspace-scoped Store operation (spec §21.10).
type WorkspaceControlStore interface {
	ListWorkspaces(context.Context) ([]core.Workspace, error)
	GetWorkspace(context.Context, string) (core.Workspace, error)
	CreateWorkspace(context.Context, string, string, *config.Config) (core.Workspace, error)
}
type Store interface {
	CreateTask(ctx context.Context, t core.Task) error
	GetTask(ctx context.Context, id string) (core.Task, error)
	GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error)
	ListTasks(ctx context.Context) ([]core.Task, error)
	UpdateTaskState(ctx context.Context, id string, s core.TaskState) error
	SetTaskTransition(ctx context.Context, id string, state core.TaskState, nextStage, recoveryStage core.Stage) error
	UpdateTaskClassification(ctx context.Context, id, class string) error
	EnsureTaskEnqueued(ctx context.Context, id string) error

	CreateJob(ctx context.Context, j core.Job) error
	UpdateJob(ctx context.Context, j core.Job) error
	ListJobs(ctx context.Context, taskID string) ([]core.Job, error)
	GetLatestJob(ctx context.Context, taskID string) (core.Job, bool, error)
	// WithTaskLock serializes one workspace-scoped external side effect across
	// concurrent control-plane instances while its callback rechecks durable
	// state. Implementations must release the lock when the callback returns.
	WithTaskLock(ctx context.Context, taskID string, fn func() error) error

	AppendEvent(ctx context.Context, event core.Event) error
	ListEvents(ctx context.Context, taskID string) ([]core.Event, error)
	ListEventsAfter(ctx context.Context, taskID string, afterID int64) ([]core.Event, error)
	CountEvents(ctx context.Context, taskID, kind string) (int, error)
	// CountEventsSinceHumanIntervention counts task events of the given kind
	// recorded after the latest human intervention on the task — the check-in
	// window of spec §21.17. With no human intervention it counts all events
	// of that kind.
	CountEventsSinceHumanIntervention(ctx context.Context, taskID, kind string) (int, error)
	ListActivityMarkers(ctx context.Context) ([]ActivityMarker, error)
	CreateIntervention(ctx context.Context, intervention core.Intervention) error
	ListInterventions(ctx context.Context, taskID string) ([]core.Intervention, error)
	UpsertTranscript(ctx context.Context, transcript core.Transcript) error
	GetTranscript(ctx context.Context, jobID string) (core.Transcript, error)
	CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error)
	GetLatestSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error)
	ApproveSpecVersion(ctx context.Context, taskID string, version int) error
	QueueGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error
	GetGitHubLifecycle(ctx context.Context, taskID string) (core.GitHubLifecycle, bool, error)
	UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error
	ReconcileGitHubLifecycles(ctx context.Context) (int, error)

	CreateWorkOrder(ctx context.Context, order core.WorkOrder) error
	CreateReviewRound(ctx context.Context, taskID string, jobs []core.Job, orders []core.WorkOrder) error
	RetryReviewRound(ctx context.Context, request ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (ReviewRoundRetryResult, error)
	RecoverInterruptedReviewRound(ctx context.Context, request InterruptedReviewRecoveryRequest, queueTimeout time.Duration) (InterruptedReviewRecoveryResult, error)
	GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error)
	ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error)
	ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error)
	ClaimWorkOrder(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error)
	RedispatchWorkOrder(ctx context.Context, id string, queueTimeout time.Duration) (core.WorkOrder, error)
	RecoverWorkOrder(ctx context.Context, id, requestID string, queueTimeout time.Duration) (core.WorkOrder, error)
	UpdateWorkOrder(ctx context.Context, order core.WorkOrder) error
	QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error
	GetReviewPublication(ctx context.Context, reviewWorkOrderID string) (core.ReviewPublication, error)
	UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error
	ReconcileReviewPublications(ctx context.Context) (int, error)
	AcceptReviewDecision(ctx context.Context, decision core.ReviewDecision) error
	CreateWorkerPairing(ctx context.Context, pairing core.WorkerPairing) error
	ConsumeWorkerPairing(ctx context.Context, tokenHash string, now time.Time) (core.WorkerPairing, error)
	CreateWorker(ctx context.Context, worker core.Worker) error
	ListWorkers(ctx context.Context) ([]core.Worker, error)
	AuthenticateWorker(ctx context.Context, credentialHash string) (core.Worker, error)
	HeartbeatWorker(ctx context.Context, id string, leaseExpires time.Time, probes []core.HarnessProbe) (core.Worker, error)
	RevokeWorker(ctx context.Context, id string) error
	RenewWorkerClaim(ctx context.Context, workOrderID, workerID, sessionID string, lease time.Duration) (core.WorkOrder, error)
	ReleaseWorkerClaim(ctx context.Context, workOrderID, workerID string, release core.WorkOrderRelease) (core.WorkOrder, error)

	CreateFeature(ctx context.Context, feature core.Feature) error
	ListFeatures(ctx context.Context) ([]core.Feature, error)
	AssignTaskFeature(ctx context.Context, taskID, featureID string) error
	CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error)
	GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error)
	GetArtifactForContext(ctx context.Context, id, taskID, featureID string) (core.Artifact, []byte, error)
	ListArtifacts(ctx context.Context) ([]core.Artifact, error)
}

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

// ActivityMarker contains only the changing fields needed by the activity
// index plus narrow review lifecycle diagnostics. Full job and event histories
// are still loaded only for one selected task.
type ActivityMarker struct {
	TaskID                    string
	LatestStage               core.Stage
	LastEventAt               time.Time
	ReviewDiagnostics         []ReviewVerdictDiagnostic
	ReviewRecovery            *ReviewRecoveryState
	InterruptedReviewRecovery *InterruptedReviewRecoveryState
}

// InterruptedReviewRecoveryState describes only the latest round's incomplete
// seats whose worker attempts expired or were retry-suppressed. Completed seats
// remain authoritative and are never recreated (spec §21.26).
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
}

type InterruptedReviewRecoveryResult struct {
	RequestID       string           `json:"request_id"`
	TaskID          string           `json:"task_id"`
	ReviewRound     int              `json:"review_round"`
	RecoveredOrders []core.WorkOrder `json:"recovered_orders"`
	RetainedOrders  []core.WorkOrder `json:"retained_orders"`
}

type memoryInterruptedReviewRecovery struct {
	Workspace string
	Request   InterruptedReviewRecoveryRequest
	Result    InterruptedReviewRecoveryResult
}

func InterruptedReviewRecoveryNeeded(orders []core.WorkOrder) *InterruptedReviewRecoveryState {
	latest := 0
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound > latest {
			latest = order.ReviewRound
		}
	}
	if latest == 0 {
		return nil
	}
	state := &InterruptedReviewRecoveryState{Needed: true, ReviewRound: latest, Reason: "latest review round has interrupted seats whose claims are no longer authorized"}
	for _, order := range orders {
		if order.Stage != core.StageReview || order.ReviewRound != latest {
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

// ReviewRecoveryState is the actionable projection of a latest review round
// that cannot finish because at least one seat reached terminal timed_out.
// Prior work orders remain immutable; recovery always creates a new round.
type ReviewRecoveryState struct {
	Needed         bool             `json:"needed"`
	PriorRound     int              `json:"prior_round"`
	Reason         string           `json:"reason"`
	TimedOutOrders []core.WorkOrder `json:"timed_out_orders"`
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
func ReviewRecoveryNeeded(orders []core.WorkOrder) *ReviewRecoveryState {
	latest := 0
	for _, order := range orders {
		if order.Stage != core.StageReview {
			continue
		}
		if order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted {
			return nil
		}
		if order.ReviewRound > latest {
			latest = order.ReviewRound
		}
	}
	if latest == 0 {
		return nil
	}
	state := &ReviewRecoveryState{Needed: true, PriorRound: latest, Reason: "latest review round is terminal after a reviewer timed out"}
	for _, order := range orders {
		if order.Stage != core.StageReview || order.ReviewRound != latest {
			continue
		}
		switch order.State {
		case core.WorkOrderTimedOut:
			state.TimedOutOrders = append(state.TimedOutOrders, order)
		}
	}
	if len(state.TimedOutOrders) == 0 {
		return nil
	}
	return state
}

func NewMemory() Store {
	return &memory{
		tasks:                       map[string]core.Task{},
		jobs:                        map[string][]core.Job{},
		events:                      map[string][]core.Event{},
		interventions:               map[string][]core.Intervention{},
		transcripts:                 map[string]core.Transcript{},
		specs:                       map[string][]core.SpecVersion{},
		workOrders:                  map[string]core.WorkOrder{},
		publications:                map[string]core.ReviewPublication{},
		github:                      map[string]core.GitHubLifecycle{},
		features:                    map[string]core.Feature{},
		artifacts:                   map[memoryArtifactKey]memoryArtifact{},
		pairings:                    map[string]core.WorkerPairing{},
		workers:                     map[string]core.Worker{},
		recoveries:                  map[string]struct{}{},
		reviewRetries:               map[string]memoryReviewRoundRetry{},
		interruptedReviewRecoveries: map[string]memoryInterruptedReviewRecovery{},
	}
}

type memoryArtifact struct {
	meta    core.Artifact
	content []byte
	links   []core.Artifact
}

type memoryArtifactKey struct {
	workspace string
	id        string
}

type memory struct {
	mu                          sync.RWMutex
	tasks                       map[string]core.Task
	jobs                        map[string][]core.Job
	events                      map[string][]core.Event
	interventions               map[string][]core.Intervention
	transcripts                 map[string]core.Transcript
	specs                       map[string][]core.SpecVersion
	workOrders                  map[string]core.WorkOrder
	publications                map[string]core.ReviewPublication
	github                      map[string]core.GitHubLifecycle
	features                    map[string]core.Feature
	artifacts                   map[memoryArtifactKey]memoryArtifact
	pairings                    map[string]core.WorkerPairing
	workers                     map[string]core.Worker
	recoveries                  map[string]struct{}
	reviewRetries               map[string]memoryReviewRoundRetry
	interruptedReviewRecoveries map[string]memoryInterruptedReviewRecovery
	nextEventID                 int64
	nextReviewID                int64
	taskLocks                   sync.Map
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

func workspaceOrDefault(ctx context.Context, fallback string) string {
	if workspace, ok := WorkspaceFromContext(ctx); ok && workspace != "" {
		return workspace
	}
	return fallback
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
	m.appendEventLocked(ctx, core.Event{Kind: "worker.enrolled", ActorRole: core.ActorRunner, ActorID: worker.ID, Payload: core.JSONPayload(map[string]string{"worker_id": worker.ID, "name": worker.Name})})
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
	m.appendEventLocked(ctx, core.Event{Kind: "worker.heartbeat", ActorRole: core.ActorRunner, ActorID: id, Payload: core.JSONPayload(map[string]any{"worker_id": id, "lease_expires_at": leaseExpires, "probes": probes})})
	return worker, nil
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

func (m *memory) RenewWorkerClaim(ctx context.Context, workOrderID, workerID, sessionID string, lease time.Duration) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	now := time.Now().UTC()
	if ok {
		order = m.refreshWorkOrderLocked(ctx, order, now)
	}
	if !ok || order.WorkerID != workerID || order.SessionID == "" || order.SessionID != sessionID {
		return core.WorkOrder{}, ErrWorkerUnauthorized
	}
	if order.State == core.WorkOrderSubmitted || order.State == core.WorkOrderCompleted {
		return order, nil
	}
	if order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, ErrWorkerUnauthorized
	}
	expires := now.Add(lease)
	if !order.ExecutionDeadline.IsZero() && expires.After(order.ExecutionDeadline) {
		expires = order.ExecutionDeadline
	}
	order.LeaseExpiresAt = expires
	order.UpdatedAt = now
	m.workOrders[workOrderID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.lease_renewed", ActorRole: core.ActorRunner, ActorID: workerID, Payload: core.JSONPayload(map[string]any{"lease_expires_at": expires})})
	return order, nil
}

func (m *memory) ReleaseWorkerClaim(ctx context.Context, workOrderID, workerID string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	now := time.Now().UTC()
	if ok {
		order = m.refreshWorkOrderLocked(ctx, order, now)
	}
	if !ok || order.WorkerID != workerID || order.SessionID == "" || order.SessionID != release.SessionID || order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, ErrWorkerUnauthorized
	}
	clearActiveAttempt(&order)
	order.State = core.WorkOrderQueued
	order.LastAttemptOutcome = release.Outcome
	order.NextRetryAt = time.Time{}
	if release.Outcome == core.WorkOrderOutcomeChildFailure {
		order.LastFailureMessage = strings.TrimSpace(release.Reason)
		order.LastFailureExitStatus = release.ExitStatus
		order.LastFailureAt = now
		limit := release.AutomaticRetryLimit
		if limit <= 0 {
			limit = 3
		}
		if order.AutomaticRetryCount < limit {
			order.AutomaticRetryCount++
			order.NextRetryAt = now.Add(workOrderRetryDelay(release, order.AutomaticRetryCount))
			order.RetrySuppressed = false
		} else {
			order.RetrySuppressed = true
		}
	} else {
		order.RetrySuppressed = true
	}
	order.UpdatedAt = now
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
	if release.Outcome == core.WorkOrderOutcomeChildFailure {
		kind = "work_order.child_failed"
	}
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: kind, ActorRole: core.ActorRunner, ActorID: workerID, Payload: core.JSONPayload(map[string]any{"session_id": release.SessionID, "reason": release.Reason, "outcome": release.Outcome, "exit_status": release.ExitStatus, "automatic_retry_count": order.AutomaticRetryCount, "next_retry_at": order.NextRetryAt, "retry_suppressed": order.RetrySuppressed}), At: now})
	return order, nil
}

func clearActiveAttempt(order *core.WorkOrder) {
	order.ClaimantID, order.SessionID, order.ClientTokenHash = "", "", ""
	order.Agent, order.Model, order.WorkerID, order.ModelEnforcement = "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
	order.ExecutionStartedAt, order.ExecutionDeadline = time.Time{}, time.Time{}
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

func (m *memory) WithTaskLock(ctx context.Context, taskID string, fn func() error) error {
	workspace, _ := WorkspaceFromContext(ctx)
	value, _ := m.taskLocks.LoadOrStore(workspace+"/"+taskID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func (m *memory) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.publications[publication.ReviewWorkOrderID]; ok {
		return nil
	}
	now := time.Now().UTC()
	publication.State, publication.CreatedAt, publication.UpdatedAt = core.ReviewPublicationQueued, now, now
	m.publications[publication.ReviewWorkOrderID] = publication
	m.appendEventLocked(ctx, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: "review.publication_queued", Payload: core.JSONPayload(publication)})
	return nil
}

func (m *memory) GetReviewPublication(_ context.Context, id string) (core.ReviewPublication, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	publication, ok := m.publications[id]
	if !ok {
		return core.ReviewPublication{}, fmt.Errorf("review publication %s not found", id)
	}
	return publication, nil
}

func (m *memory) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.publications[publication.ReviewWorkOrderID]; !ok {
		return fmt.Errorf("review publication %s not found", publication.ReviewWorkOrderID)
	}
	publication.UpdatedAt = time.Now().UTC()
	m.publications[publication.ReviewWorkOrderID] = publication
	kind := "review.publication_retry"
	if publication.State == core.ReviewPublicationPublished {
		kind = "review.publication_published"
	} else if publication.State == core.ReviewPublicationFailed {
		kind = "review.publication_failed"
	}
	m.appendEventLocked(ctx, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: kind, Payload: core.JSONPayload(publication)})
	return nil
}

func (m *memory) ReconcileReviewPublications(ctx context.Context) (int, error) {
	m.mu.RLock()
	var missing []core.ReviewPublication
	seen := map[string]bool{}
	for taskID, events := range m.events {
		for _, event := range events {
			if event.Kind != "review.completed" {
				continue
			}
			publication, ok := reviewPublicationFromEvent(taskID, event.JobID, event.Payload)
			if ok && !seen[publication.ReviewWorkOrderID] {
				if _, exists := m.publications[publication.ReviewWorkOrderID]; !exists {
					missing = append(missing, publication)
					seen[publication.ReviewWorkOrderID] = true
				}
			}
		}
	}
	m.mu.RUnlock()
	created := 0
	for _, publication := range missing {
		if err := m.QueueReviewPublication(ctx, publication); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

func (m *memory) AcceptReviewDecision(ctx context.Context, decision core.ReviewDecision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[decision.TaskID]
	if !ok {
		return fmt.Errorf("task %s not found", decision.TaskID)
	}
	job, _, ok := m.findJobLocked(decision.JobID)
	if !ok || job.TaskID != decision.TaskID {
		return fmt.Errorf("job %s does not belong to task %s", decision.JobID, decision.TaskID)
	}
	completed := false
	for _, event := range m.events[decision.TaskID] {
		var prior struct {
			ReviewWorkOrderID string `json:"review_work_order_id"`
		}
		if json.Unmarshal(event.Payload, &prior) != nil || prior.ReviewWorkOrderID != decision.ReviewWorkOrderID {
			continue
		}
		if event.Kind == "review.accepted" {
			return nil
		}
		completed = completed || event.Kind == "review.completed"
	}
	if !completed {
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

	state, next, recovery := core.TaskAwaiting, core.Stage(""), core.StageImplement
	if aggregate.Verdict == "changes_requested" {
		count := 0
		for _, event := range m.events[decision.TaskID] {
			if event.Kind == "pipeline.bounced" {
				count++
			}
		}
		// The check-in comparison uses bounces since the last human
		// intervention, not the lifetime count (spec §21.17); the recorded
		// count in the event payload stays lifetime for the timeline.
		window := m.countEventsSinceHumanInterventionLocked(decision.TaskID, "pipeline.bounced")
		m.nextReviewID++
		actorID := fmt.Sprintf("review:round:%d", decision.ReviewRound)
		intervention := core.Intervention{ID: m.nextReviewID, TaskID: decision.TaskID, JobID: decision.JobID, ActorID: actorID, ActorRole: core.ActorAgent, Action: core.InterventionRedirect, ReasonCode: aggregate.ReasonCode, Comment: aggregate.Feedback, At: time.Now().UTC()}
		m.interventions[decision.TaskID] = append(m.interventions[decision.TaskID], intervention)
		m.appendEventLocked(ctx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "intervention.redirect", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"reason_code": aggregate.ReasonCode, "comment": aggregate.Feedback, "review_round": decision.ReviewRound, "reviews": aggregate.Reviews}), At: intervention.At})
		count++
		window++
		m.appendEventLocked(ctx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "pipeline.bounced", Payload: core.JSONPayload(map[string]any{"from": "review", "to": "implement", "reason_code": aggregate.ReasonCode, "feedback": aggregate.Feedback, "reviews": aggregate.Reviews, "count": count, "source": "mcp-review-panel", "review_round": decision.ReviewRound})})
		if window < decision.MaxBounces {
			state, next, recovery = core.TaskQueued, core.StageImplement, ""
		} else {
			m.appendEventLocked(ctx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"count": count, "window": window, "max_bounces": decision.MaxBounces, "review_round": decision.ReviewRound})})
		}
	} else if (decision.PolicyVersion > 0 && !decision.MergeApproval) || (decision.PolicyVersion == 0 && decision.Level == core.L0) {
		state, recovery = core.TaskApproved, ""
	}
	fromState, fromStage := task.State, task.NextStage
	task.State, task.NextStage, task.RecoveryStage = state, next, recovery
	m.tasks[task.ID] = task
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state})})
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": fromStage, "next_stage": next, "recovery_stage": recovery, "state": state, "review_work_order_id": decision.ReviewWorkOrderID})})
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
}

type reviewRoundResult struct {
	ReviewRound int               `json:"review_round"`
	Verdict     string            `json:"verdict"`
	ReasonCode  string            `json:"reason_code"`
	Summary     string            `json:"summary"`
	Feedback    string            `json:"feedback,omitempty"`
	Reviews     []completedReview `json:"reviews"`
}

func (m *memory) completedReviewRoundLocked(taskID string, round int, workOrderID string) ([]completedReview, int) {
	required := 0
	for _, order := range m.workOrders {
		if order.TaskID == taskID && order.Stage == core.StageReview && order.ReviewRound == round && (round > 0 || order.ID == workOrderID) {
			required++
		}
	}
	var reviews []completedReview
	for _, event := range m.events[taskID] {
		if event.Kind != "review.completed" {
			continue
		}
		var review completedReview
		if json.Unmarshal(event.Payload, &review) == nil && review.ReviewRound == round && (round > 0 || review.ReviewWorkOrderID == workOrderID) {
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
		return reviewRoundResult{ReviewRound: round, Verdict: review.Verdict, ReasonCode: review.ReasonCode, Summary: review.Summary, Feedback: review.Feedback, Reviews: reviews}
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
	result.Feedback = strings.Join(feedback, "\n")
	return result
}

func reviewDecisionPayload(decision core.ReviewDecision) []byte {
	return core.JSONPayload(map[string]any{
		"review_work_order_id": decision.ReviewWorkOrderID, "verdict": decision.Verdict,
		"reason_code": decision.ReasonCode, "summary": decision.Summary, "feedback": decision.Feedback,
		"reviewed_commit_sha": decision.ReviewedCommitSHA, "reviewer": decision.Reviewer,
		"reviewer_model": decision.ReviewerModel, "reviewer_session": decision.ReviewerSession,
		"same_model_as_implementer": decision.SameModelAsImplementer,
		"review_round":              decision.ReviewRound, "review_seat": decision.ReviewSeat,
		"required_model": decision.RequiredModel, "required_harness": decision.RequiredHarness,
		"required_effort":      decision.RequiredEffort,
		"model_enforcement":    decision.ModelEnforcement,
		"publication_eligible": decision.PublicationEligible,
	})
}

func reviewPublicationFromDecision(decision core.ReviewDecision) core.ReviewPublication {
	return core.ReviewPublication{
		ReviewWorkOrderID: decision.ReviewWorkOrderID, TaskID: decision.TaskID, JobID: decision.JobID,
		Verdict: decision.Verdict, ReasonCode: decision.ReasonCode, Summary: decision.Summary,
		Feedback: decision.Feedback, ReviewedCommitSHA: decision.ReviewedCommitSHA,
		ReviewerModel: decision.ReviewerModel, ReviewerSession: decision.ReviewerSession,
		SameModelAsImplementer: decision.SameModelAsImplementer,
		ReviewRound:            decision.ReviewRound, ReviewSeat: decision.ReviewSeat,
		RequiredModel: decision.RequiredModel, RequiredHarness: decision.RequiredHarness, RequiredEffort: decision.RequiredEffort,
		ModelEnforcement: decision.ModelEnforcement,
	}
}

func reviewPublicationFromEvent(taskID, jobID string, payload []byte) (core.ReviewPublication, bool) {
	var eligibility struct {
		PublicationEligible bool `json:"publication_eligible"`
	}
	if json.Unmarshal(payload, &eligibility) != nil || !eligibility.PublicationEligible {
		return core.ReviewPublication{}, false
	}
	var publication core.ReviewPublication
	if json.Unmarshal(payload, &publication) != nil || publication.ReviewWorkOrderID == "" {
		return core.ReviewPublication{}, false
	}
	publication.TaskID = taskID
	publication.JobID = jobID
	publication.State = core.ReviewPublicationQueued
	return publication, true
}

func (m *memory) CreateWorkOrder(ctx context.Context, order core.WorkOrder) error {
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
	if order.State == "" {
		order.State = core.WorkOrderQueued
	}
	order.Claimable = order.ClaimableAt(now)
	m.workOrders[order.ID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	return nil
}

func (m *memory) CreateReviewRound(ctx context.Context, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	if selected, present := WorkspaceFromContext(ctx); present && selected != "" && task.Workspace != selected {
		return fmt.Errorf("task %s belongs to workspace %s, not %s", taskID, task.Workspace, selected)
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
			return fmt.Errorf("job %s already exists", job.ID)
		}
	}
	now := time.Now().UTC()
	from := task.State
	task.State = core.TaskRunning
	m.tasks[taskID] = task
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": from, "to": core.TaskRunning})})
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
		order.State, order.Claimable, order.UpdatedAt = core.WorkOrderQueued, true, now
		m.workOrders[order.ID] = order
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	}
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "review.round_created", Payload: core.JSONPayload(map[string]any{"review_round": orders[0].ReviewRound, "seat_count": len(orders)})})
	return nil
}

func (m *memory) RetryReviewRound(ctx context.Context, request ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (ReviewRoundRetryResult, error) {
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
	recovery := ReviewRecoveryNeeded(taskOrders)
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
			return ReviewRoundRetryResult{}, fmt.Errorf("%w: job %s already exists", ErrReviewRetryConflict, job.ID)
		}
		if _, exists := m.workOrders[order.ID]; exists {
			return ReviewRoundRetryResult{}, fmt.Errorf("%w: work order %s already exists", ErrReviewRetryConflict, order.ID)
		}
	}
	if task.State != core.TaskRunning {
		from := task.State
		task.State = core.TaskRunning
		m.tasks[request.TaskID] = task
		m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": from, "to": core.TaskRunning}), At: now})
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
		order.State, order.Claimable, order.UpdatedAt = core.WorkOrderQueued, true, now
		m.workOrders[order.ID] = order
		created = append(created, order)
		m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order), At: now})
	}
	actor := ActorFromContext(ctx)
	timedOutIDs := make([]string, 0, len(recovery.TimedOutOrders))
	for _, order := range recovery.TimedOutOrders {
		timedOutIDs = append(timedOutIDs, order.ID)
	}
	payload := map[string]any{"request_id": request.RequestID, "workspace_id": workspace, "task_id": request.TaskID, "actor": actor.ID, "reason": request.Reason, "prior_round": request.PriorRound, "new_round": newRound, "pr_head": request.PRHead, "timed_out_work_order_ids": timedOutIDs}
	m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, Kind: "review.round_retried", Payload: core.JSONPayload(payload), At: now})
	m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, Kind: "review.round_created", Payload: core.JSONPayload(map[string]any{"review_round": newRound, "seat_count": len(created), "retry_request_id": request.RequestID}), At: now})
	result := ReviewRoundRetryResult{RequestID: request.RequestID, TaskID: request.TaskID, PriorRound: request.PriorRound, NewRound: newRound, PRHead: request.PRHead, WorkOrders: created}
	m.reviewRetries[key] = memoryReviewRoundRetry{Workspace: workspace, Request: request, Result: result}
	return result, nil
}

func (m *memory) RecoverInterruptedReviewRound(ctx context.Context, request InterruptedReviewRecoveryRequest, queueTimeout time.Duration) (InterruptedReviewRecoveryResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.RequestID == "" || request.TaskID == "" || request.Round <= 0 || queueTimeout <= 0 {
		return InterruptedReviewRecoveryResult{}, fmt.Errorf("interrupted review recovery requires task, request_id, round, and queue timeout")
	}
	workspace, _ := WorkspaceFromContext(ctx)
	key := request.RequestID
	if prior, ok := m.interruptedReviewRecoveries[key]; ok {
		if prior.Workspace != workspace || prior.Request != request {
			return InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", ErrReviewRetryConflict, request.RequestID)
		}
		return prior.Result, nil
	}
	task, ok := m.tasks[request.TaskID]
	if !ok || task.Workspace != workspace {
		return InterruptedReviewRecoveryResult{}, fmt.Errorf("task %s not found", request.TaskID)
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
	recovery := InterruptedReviewRecoveryNeeded(taskOrders)
	if recovery == nil || recovery.ReviewRound != request.Round {
		return InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: task %s has no matching interrupted review round", ErrReviewRetryConflict, request.TaskID)
	}
	result := InterruptedReviewRecoveryResult{RequestID: request.RequestID, TaskID: request.TaskID, ReviewRound: request.Round, RetainedOrders: append([]core.WorkOrder(nil), recovery.RetainedOrders...)}
	actor := ActorFromContext(ctx)
	for _, eligible := range recovery.EligibleOrders {
		order := m.workOrders[eligible.ID]
		priorOutcome := order.LastAttemptOutcome
		order.LastAttemptOutcome = ""
		order.RetrySuppressed = false
		order.AutomaticRetryCount = 0
		order.NextRetryAt = time.Time{}
		order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
		order.RedispatchCount++
		order.UpdatedAt, order.Claimable = now, true
		m.workOrders[order.ID] = order
		result.RecoveredOrders = append(result.RecoveredOrders, order)
		m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, JobID: order.JobID, Kind: "review.seat_recovered", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "review_round": request.Round, "review_seat": order.ReviewSeat, "work_order_id": order.ID, "request_id": request.RequestID, "prior_state": core.WorkOrderQueued, "prior_outcome": priorOutcome, "resulting_state": order.State, "outcome": "recovered"}), At: now})
	}
	for _, retained := range recovery.RetainedOrders {
		m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, JobID: retained.JobID, Kind: "review.seat_recovery_skipped", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "review_round": request.Round, "review_seat": retained.ReviewSeat, "work_order_id": retained.ID, "request_id": request.RequestID, "prior_state": retained.State, "resulting_state": retained.State, "outcome": "retained_completed"}), At: now})
	}
	m.appendEventLocked(ctx, core.Event{TaskID: request.TaskID, Kind: "review.round_recovered", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "review_round": request.Round, "request_id": request.RequestID, "actor": actor.ID, "recovered_seats": len(result.RecoveredOrders), "retained_completed_seats": len(result.RetainedOrders)}), At: now})
	m.interruptedReviewRecoveries[key] = memoryInterruptedReviewRecovery{Workspace: workspace, Request: request, Result: result}
	return result, nil
}

func (m *memory) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	if selected, present := WorkspaceFromContext(ctx); present && selected != "" && m.tasks[order.TaskID].Workspace != selected {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	order = m.refreshWorkOrderLocked(ctx, order, time.Now().UTC())
	return order, nil
}

func (m *memory) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	orders := make([]core.WorkOrder, 0, len(m.workOrders))
	for _, order := range m.workOrders {
		order = m.refreshWorkOrderLocked(ctx, order, now)
		orders = append(orders, order)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.Before(orders[j].CreatedAt) })
	return orders, nil
}

func (m *memory) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, _ := m.ListWorkOrders(ctx)
	out := orders[:0]
	for _, order := range orders {
		if order.TaskID == taskID {
			out = append(out, order)
		}
	}
	return out, nil
}

func (m *memory) ClaimWorkOrder(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	now := time.Now().UTC()
	order = m.refreshWorkOrderLocked(ctx, order, now)
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
		lease = 15 * time.Minute
	}
	order.State, order.ClaimantID, order.SessionID = core.WorkOrderClaimed, claim.ClaimantID, claim.SessionID
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
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(order)})
	return order, nil
}

func (m *memory) RedispatchWorkOrder(ctx context.Context, id string, queueTimeout time.Duration) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	now := time.Now().UTC()
	order = m.refreshWorkOrderLocked(ctx, order, now)
	if order.State != core.WorkOrderStale {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not stale and cannot be redispatched", id)
	}
	if queueTimeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	order.State, order.Claimable = core.WorkOrderQueued, true
	order.ClaimantID, order.SessionID, order.ClientTokenHash = "", "", ""
	order.Agent, order.Model, order.WorkerID, order.Progress = "", "", "", ""
	order.ModelEnforcement = ""
	order.LeaseExpiresAt = time.Time{}
	order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
	order.ExecutionStartedAt, order.ExecutionDeadline = time.Time{}, time.Time{}
	order.RedispatchCount++
	order.UpdatedAt = now
	m.workOrders[id] = order
	if job, index, exists := m.findJobLocked(order.JobID); exists {
		job.State, job.StartedAt, job.EndedAt = core.JobPending, time.Time{}, time.Time{}
		m.jobs[job.TaskID][index] = job
	}
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.redispatched", Payload: core.JSONPayload(order), At: now})
	return order, nil
}

func (m *memory) RecoverWorkOrder(ctx context.Context, id, requestID string, queueTimeout time.Duration) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(requestID) == "" {
		return core.WorkOrder{}, fmt.Errorf("recovery request_id is required")
	}
	if queueTimeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	workspace, _ := WorkspaceFromContext(ctx)
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
	if workspace != "" && m.tasks[order.TaskID].Workspace != workspace {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	if order.State != core.WorkOrderQueued || order.SessionID != "" || order.WorkerID != "" || !order.LeaseExpiresAt.IsZero() {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not an unclaimed queued order", id)
	}
	if order.LastAttemptOutcome == "" && !order.RetrySuppressed && order.NextRetryAt.IsZero() {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not released, expired, or retry-suppressed", id)
	}
	now := time.Now().UTC()
	prior := order.LastAttemptOutcome
	order.LastAttemptOutcome = ""
	order.RetrySuppressed = false
	order.AutomaticRetryCount = 0
	order.NextRetryAt = time.Time{}
	order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
	order.RedispatchCount++
	order.UpdatedAt = now
	order.Claimable = true
	m.workOrders[id] = order
	m.recoveries[key] = struct{}{}
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.recovered", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "work_order_id": id, "request_id": requestID, "prior_state": core.WorkOrderQueued, "prior_outcome": prior, "new_state": order.State, "reason": "operator recovery"}), At: now})
	return order, nil
}

func (m *memory) refreshWorkOrderLocked(ctx context.Context, order core.WorkOrder, now time.Time) core.WorkOrder {
	if (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed) &&
		!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now) {
		order.State, order.Claimable = core.WorkOrderTimedOut, false
		order.LeaseExpiresAt = time.Time{}
		order.UpdatedAt = now
		m.workOrders[order.ID] = order
		if job, index, exists := m.findJobLocked(order.JobID); exists {
			job.State, job.EndedAt = core.JobFailed, order.ExecutionDeadline
			m.jobs[job.TaskID][index] = job
		}
		m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.timed_out", Payload: core.JSONPayload(order), At: now})
		return order
	}
	if order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.After(now) {
		clearActiveAttempt(&order)
		order.State, order.Claimable = core.WorkOrderQueued, false
		order.LastAttemptOutcome = core.WorkOrderOutcomeExpired
		order.NextRetryAt = time.Time{}
		order.RetrySuppressed = true
		order.UpdatedAt = now
		m.workOrders[order.ID] = order
		if job, index, exists := m.findJobLocked(order.JobID); exists {
			job.State, job.StartedAt, job.EndedAt = core.JobPending, time.Time{}, time.Time{}
			m.jobs[job.TaskID][index] = job
		}
		m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.expired", Payload: core.JSONPayload(map[string]any{"outcome": order.LastAttemptOutcome, "retry_suppressed": true}), At: now})
	}
	if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
		!order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
		order.State, order.Claimable, order.UpdatedAt = core.WorkOrderStale, false, now
		m.workOrders[order.ID] = order
		m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.stale", Payload: core.JSONPayload(order), At: now})
		return order
	}
	order.Claimable = order.ClaimableAt(now)
	return order
}

func tokenHash(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

func (m *memory) UpdateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if updateRequiresClaim(order.State, current.State) &&
		(current.State != core.WorkOrderClaimed || current.SessionID == "" || current.SessionID != order.SessionID) {
		return fmt.Errorf("work order %s is not claimed by this session", order.ID)
	}
	order.UpdatedAt = time.Now().UTC()
	m.workOrders[order.ID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order)})
	return nil
}

func updateRequiresClaim(next, current core.WorkOrderState) bool {
	if next == core.WorkOrderClaimed {
		return true
	}
	return current != next && (next == core.WorkOrderSubmitted || next == core.WorkOrderCompleted)
}

func (m *memory) CreateFeature(ctx context.Context, feature core.Feature) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.features[feature.ID]; ok {
		return fmt.Errorf("feature %s already exists", feature.ID)
	}
	if feature.ParentID != "" {
		if _, ok := m.features[feature.ParentID]; !ok {
			return fmt.Errorf("parent feature %s not found", feature.ParentID)
		}
	}
	if feature.CreatedAt.IsZero() {
		feature.CreatedAt = time.Now().UTC()
	}
	m.features[feature.ID] = feature
	return nil
}

func (m *memory) ListFeatures(_ context.Context) ([]core.Feature, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]core.Feature, 0, len(m.features))
	for _, feature := range m.features {
		out = append(out, feature)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memory) AssignTaskFeature(ctx context.Context, taskID, featureID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	if featureID != "" {
		if _, ok := m.features[featureID]; !ok {
			return fmt.Errorf("feature %s not found", featureID)
		}
	}
	task.FeatureID = featureID
	m.tasks[taskID] = task
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "task.feature_assigned", Payload: core.JSONPayload(map[string]string{"feature_id": featureID})})
	return nil
}

func (m *memory) CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, artifact.Workspace)
	if artifact.Workspace != "" && artifact.Workspace != workspace {
		return core.Artifact{}, fmt.Errorf("artifact workspace mismatch")
	}
	artifact.Workspace = workspace
	if artifact.Role == "" {
		artifact.Role = core.ArtifactRoleTaskContext
	}
	if !artifact.Role.Valid() {
		return core.Artifact{}, fmt.Errorf("invalid artifact role %q", artifact.Role)
	}
	artifact.ID = fmt.Sprintf("%x", sha256.Sum256(content))
	artifact.SizeBytes = int64(len(content))
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}
	key := memoryArtifactKey{workspace: artifact.Workspace, id: artifact.ID}
	if existing, ok := m.artifacts[key]; ok {
		for _, link := range existing.links {
			if link.Workspace == artifact.Workspace && link.TaskID == artifact.TaskID && link.FeatureID == artifact.FeatureID && link.Role == artifact.Role {
				return link, nil
			}
		}
		existing.links = append(existing.links, artifact)
		m.artifacts[key] = existing
		return artifact, nil
	}
	m.artifacts[key] = memoryArtifact{meta: artifact, content: append([]byte(nil), content...), links: []core.Artifact{artifact}}
	return artifact, nil
}

func (m *memory) artifactForRead(ctx context.Context, id string) (memoryArtifact, bool) {
	if workspace, scoped := WorkspaceFromContext(ctx); scoped {
		artifact, ok := m.artifacts[memoryArtifactKey{workspace: workspace, id: id}]
		return artifact, ok
	}
	var match memoryArtifact
	found := false
	for key, artifact := range m.artifacts {
		if key.id != id {
			continue
		}
		if found {
			return memoryArtifact{}, false
		}
		match = artifact
		found = true
	}
	return match, found
}

func (m *memory) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	artifact, ok := m.artifactForRead(ctx, id)
	if !ok {
		return core.Artifact{}, nil, fmt.Errorf("artifact %s not found", id)
	}
	return artifact.meta, append([]byte(nil), artifact.content...), nil
}

func (m *memory) GetArtifactForContext(ctx context.Context, id, taskID, featureID string) (core.Artifact, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	artifact, ok := m.artifactForRead(ctx, id)
	if !ok {
		return core.Artifact{}, nil, fmt.Errorf("artifact %s not found", id)
	}
	for _, link := range artifact.links {
		if (taskID != "" && link.TaskID == taskID) || (featureID != "" && link.FeatureID == featureID) {
			return link, append([]byte(nil), artifact.content...), nil
		}
	}
	return core.Artifact{}, nil, fmt.Errorf("artifact %s not found", id)
}

func (m *memory) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []core.Artifact
	workspace, scoped := WorkspaceFromContext(ctx)
	for key, artifact := range m.artifacts {
		if scoped && workspace != "" && key.workspace != workspace {
			continue
		}
		out = append(out, artifact.links...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
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

func (m *memory) QueueGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[lifecycle.TaskID]; !ok {
		return fmt.Errorf("task %s not found", lifecycle.TaskID)
	}
	if _, exists := m.github[lifecycle.TaskID]; exists {
		return nil
	}
	now := time.Now().UTC()
	if lifecycle.CreatedAt.IsZero() {
		lifecycle.CreatedAt = now
	}
	lifecycle.UpdatedAt = lifecycle.CreatedAt
	if lifecycle.State == "" {
		lifecycle.State = core.GitHubPublicationQueued
	}
	if lifecycle.CreateState == "" {
		lifecycle.CreateState = core.GitHubCreateNotStarted
	}
	m.github[lifecycle.TaskID] = lifecycle
	m.appendEventLocked(ctx, core.Event{TaskID: lifecycle.TaskID, Kind: "github_issue.publication_queued", Payload: core.JSONPayload(lifecycle)})
	return nil
}

func (m *memory) GetGitHubLifecycle(_ context.Context, taskID string) (core.GitHubLifecycle, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lifecycle, ok := m.github[taskID]
	return lifecycle, ok, nil
}

func (m *memory) UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.github[lifecycle.TaskID]; !ok {
		return fmt.Errorf("GitHub lifecycle for task %s not found", lifecycle.TaskID)
	}
	lifecycle.UpdatedAt = time.Now().UTC()
	m.github[lifecycle.TaskID] = lifecycle
	var kind string
	switch {
	case lifecycle.State == core.GitHubPublicationPublished:
		kind = "github_issue.publication_published"
	case lifecycle.State == core.GitHubPublicationFailed:
		kind = "github_issue.publication_failed"
	case lifecycle.State == core.GitHubPublicationRetrying && strings.TrimSpace(lifecycle.LastError) != "":
		kind = "github_issue.publication_retry"
	}
	if kind != "" {
		m.appendEventLocked(ctx, core.Event{TaskID: lifecycle.TaskID, Kind: kind, Payload: core.JSONPayload(lifecycle)})
	}
	return nil
}

func (m *memory) ReconcileGitHubLifecycles(context.Context) (int, error) { return 0, nil }

func (m *memory) CreateTask(ctx context.Context, t core.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tasks[t.ID]; exists {
		return fmt.Errorf("task %s already exists", t.ID)
	}
	for _, existing := range m.tasks {
		if existing.Branch != "" && existing.Branch == t.Branch {
			return fmt.Errorf("branch %s already belongs to task %s", t.Branch, existing.ID)
		}
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if !t.Mode.Valid() {
		t.Mode, t.SpecApproval, t.MergeApproval = core.LegacyPolicy(t.Level)
	}
	if t.Level == "" {
		t.Level = core.LegacyLevel(t.Mode, t.SpecApproval, t.MergeApproval)
	}
	if t.NextStage == "" && (t.State == core.TaskQueued || t.State == core.TaskClaiming) {
		t.NextStage = core.InitialStage(t.Level)
	}
	m.tasks[t.ID] = t
	m.appendEventLocked(ctx, core.Event{TaskID: t.ID, Kind: "task.created", Payload: core.JSONPayload(t), At: t.CreatedAt})
	return nil
}

func (m *memory) GetTask(_ context.Context, id string) (core.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return core.Task{}, fmt.Errorf("task %s not found", id)
	}
	if lifecycle, exists := m.github[id]; exists {
		copy := lifecycle
		t.GitHub = &copy
	}
	return t, nil
}

func (m *memory) GetTaskByIntakeKey(_ context.Context, key string) (core.Task, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, task := range m.tasks {
		if key != "" && task.IntakeKey == key {
			if lifecycle, exists := m.github[task.ID]; exists {
				copy := lifecycle
				task.GitHub = &copy
			}
			return task, true, nil
		}
	}
	return core.Task{}, false, nil
}

func (m *memory) ListTasks(_ context.Context) ([]core.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]core.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		if lifecycle, exists := m.github[t.ID]; exists {
			copy := lifecycle
			t.GitHub = &copy
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (m *memory) UpdateTaskState(ctx context.Context, id string, s core.TaskState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	from := t.State
	t.State = s
	m.tasks[id] = t
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": from, "to": s})})
	return nil
}

func (m *memory) SetTaskTransition(ctx context.Context, id string, state core.TaskState, nextStage, recoveryStage core.Stage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	fromState, fromStage := task.State, task.NextStage
	task.State = state
	task.NextStage = nextStage
	task.RecoveryStage = recoveryStage
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state})})
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": fromStage, "next_stage": nextStage, "recovery_stage": recoveryStage, "state": state})})
	return nil
}

func (m *memory) UpdateTaskClassification(ctx context.Context, id, class string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	task.Class = class
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.classified", Payload: core.JSONPayload(map[string]any{"class": class})})
	return nil
}

func (m *memory) EnsureTaskEnqueued(_ context.Context, id string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	if task.State != core.TaskQueued {
		return fmt.Errorf("task %s is not queued", id)
	}
	return nil
}

func (m *memory) CreateJob(ctx context.Context, j core.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[j.TaskID]; !ok {
		return fmt.Errorf("task %s not found", j.TaskID)
	}
	if _, _, ok := m.findJobLocked(j.ID); ok {
		return fmt.Errorf("job %s already exists", j.ID)
	}
	m.jobs[j.TaskID] = append(m.jobs[j.TaskID], j)
	m.appendEventLocked(ctx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.created", Payload: core.JSONPayload(j)})
	return nil
}

func (m *memory) UpdateJob(ctx context.Context, j core.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := m.jobs[j.TaskID]
	for i := range jobs {
		if jobs[i].ID == j.ID {
			jobs[i] = j
			m.jobs[j.TaskID] = jobs
			m.appendEventLocked(ctx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.updated", Payload: core.JSONPayload(j)})
			return nil
		}
	}
	return fmt.Errorf("job %s not found", j.ID)
}

func (m *memory) ListJobs(_ context.Context, taskID string) ([]core.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := append([]core.Job(nil), m.jobs[taskID]...)
	sortJobs(jobs)
	return jobs, nil
}

func (m *memory) GetLatestJob(_ context.Context, taskID string) (core.Job, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := append([]core.Job(nil), m.jobs[taskID]...)
	if len(jobs) == 0 {
		return core.Job{}, false, nil
	}
	sortJobs(jobs)
	return jobs[len(jobs)-1], true, nil
}

func (m *memory) AppendEvent(ctx context.Context, event core.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[event.TaskID]; !ok {
		return fmt.Errorf("task %s not found", event.TaskID)
	}
	if event.JobID != "" {
		job, _, ok := m.findJobLocked(event.JobID)
		if !ok || job.TaskID != event.TaskID {
			return fmt.Errorf("job %s does not belong to task %s", event.JobID, event.TaskID)
		}
	}
	m.appendEventLocked(ctx, event)
	return nil
}

func (m *memory) ListEvents(_ context.Context, taskID string) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	events := append([]core.Event(nil), m.events[taskID]...)
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID < events[j].ID
		}
		return events[i].At.Before(events[j].At)
	})
	return events, nil
}

func (m *memory) ListEventsAfter(_ context.Context, taskID string, afterID int64) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	events := m.events[taskID]
	first := sort.Search(len(events), func(i int) bool { return events[i].ID > afterID })
	return append([]core.Event(nil), events[first:]...), nil
}

func (m *memory) CountEvents(_ context.Context, taskID, kind string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, event := range m.events[taskID] {
		if event.Kind == kind {
			count++
		}
	}
	return count, nil
}

func (m *memory) CountEventsSinceHumanIntervention(_ context.Context, taskID, kind string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.countEventsSinceHumanInterventionLocked(taskID, kind), nil
}

func (m *memory) countEventsSinceHumanInterventionLocked(taskID, kind string) int {
	var since time.Time
	for _, intervention := range m.interventions[taskID] {
		if intervention.ActorRole == core.ActorHuman && intervention.At.After(since) {
			since = intervention.At
		}
	}
	count := 0
	for _, event := range m.events[taskID] {
		if event.Kind == kind && event.At.After(since) {
			count++
		}
	}
	return count
}

func (m *memory) ListActivityMarkers(ctx context.Context) ([]ActivityMarker, error) {
	orders, err := m.ListWorkOrders(ctx)
	if err != nil {
		return nil, err
	}
	ordersByTask := make(map[string][]core.WorkOrder)
	for _, order := range orders {
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	markers := make([]ActivityMarker, 0, len(m.tasks))
	for id, task := range m.tasks {
		marker := ActivityMarker{TaskID: id, LastEventAt: task.CreatedAt}
		if jobs := append([]core.Job(nil), m.jobs[id]...); len(jobs) != 0 {
			sortJobs(jobs)
			marker.LatestStage = jobs[len(jobs)-1].Stage
		}
		if events := m.events[id]; len(events) != 0 {
			marker.LastEventAt = events[len(events)-1].At
		}
		marker.ReviewDiagnostics = ReviewVerdictDiagnostics(ordersByTask[id], m.events[id], time.Now().UTC())
		marker.ReviewRecovery = ReviewRecoveryNeeded(ordersByTask[id])
		marker.InterruptedReviewRecovery = InterruptedReviewRecoveryNeeded(ordersByTask[id])
		markers = append(markers, marker)
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].TaskID < markers[j].TaskID })
	return markers, nil
}

func (m *memory) CreateIntervention(ctx context.Context, intervention core.Intervention) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.tasks[intervention.TaskID]
	if !ok {
		return fmt.Errorf("task %s not found", intervention.TaskID)
	}
	if !intervention.Action.Valid() {
		return fmt.Errorf("invalid intervention action %q", intervention.Action)
	}
	if intervention.JobID != "" {
		job, _, ok := m.findJobLocked(intervention.JobID)
		if !ok || job.TaskID != intervention.TaskID {
			return fmt.Errorf("job %s does not belong to task %s", intervention.JobID, intervention.TaskID)
		}
	}
	actor := ActorFromContext(ctx)
	if intervention.ActorID == "" {
		intervention.ActorID = actor.ID
	}
	if intervention.ActorRole == "" {
		intervention.ActorRole = actor.Role
	}
	if intervention.At.IsZero() {
		intervention.At = time.Now().UTC()
	}
	m.nextReviewID++
	intervention.ID = m.nextReviewID
	m.interventions[intervention.TaskID] = append(m.interventions[intervention.TaskID], intervention)
	m.appendEventLocked(ctx, core.Event{
		TaskID:    intervention.TaskID,
		JobID:     intervention.JobID,
		Kind:      "intervention." + string(intervention.Action),
		ActorID:   intervention.ActorID,
		ActorRole: intervention.ActorRole,
		Payload: core.JSONPayload(map[string]any{
			"reason_code": intervention.ReasonCode,
			"comment":     intervention.Comment,
		}),
		At: intervention.At,
	})
	return nil
}

func (m *memory) ListInterventions(_ context.Context, taskID string) ([]core.Intervention, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := append([]core.Intervention(nil), m.interventions[taskID]...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].At.Equal(items[j].At) {
			return items[i].ID < items[j].ID
		}
		return items[i].At.Before(items[j].At)
	})
	return items, nil
}

func (m *memory) UpsertTranscript(ctx context.Context, transcript core.Transcript) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, _, ok := m.findJobLocked(transcript.JobID)
	if !ok {
		return fmt.Errorf("job %s not found", transcript.JobID)
	}
	if transcript.CreatedAt.IsZero() {
		transcript.CreatedAt = time.Now().UTC()
	}
	if artifactID := strings.TrimPrefix(transcript.URI, "artifact://"); artifactID != transcript.URI {
		task := m.tasks[job.TaskID]
		key := memoryArtifactKey{workspace: task.Workspace, id: artifactID}
		if artifact, exists := m.artifacts[key]; exists {
			hasExplicitAudit := false
			for _, link := range artifact.links {
				hasExplicitAudit = hasExplicitAudit || link.TaskID == job.TaskID && link.Role == core.ArtifactRoleGeneratedAudit
			}
			for i := range artifact.links {
				if !hasExplicitAudit && artifact.links[i].TaskID == job.TaskID && artifact.links[i].Role.ModelInputEligible() {
					artifact.links[i].Role = core.ArtifactRoleGeneratedAudit
				}
			}
			m.artifacts[key] = artifact
		}
	}
	m.transcripts[transcript.JobID] = transcript
	m.appendEventLocked(ctx, core.Event{
		TaskID: job.TaskID, JobID: job.ID, Kind: "transcript.persisted",
		Payload: core.JSONPayload(map[string]any{"uri": transcript.URI, "redaction_stats": transcript.RedactionStats}),
	})
	return nil
}

func (m *memory) GetTranscript(_ context.Context, jobID string) (core.Transcript, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	transcript, ok := m.transcripts[jobID]
	if !ok {
		return core.Transcript{}, fmt.Errorf("transcript for job %s not found", jobID)
	}
	return transcript, nil
}

func (m *memory) appendEventLocked(ctx context.Context, event core.Event) {
	actor := ActorFromContext(ctx)
	if event.ActorID == "" {
		event.ActorID = actor.ID
	}
	if event.ActorRole == "" {
		event.ActorRole = actor.Role
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Payload == nil {
		event.Payload = core.JSONPayload(struct{}{})
	}
	m.nextEventID++
	event.ID = m.nextEventID
	m.events[event.TaskID] = append(m.events[event.TaskID], event)
}

func (m *memory) findJobLocked(id string) (core.Job, int, bool) {
	for _, jobs := range m.jobs {
		for i, job := range jobs {
			if job.ID == id {
				return job, i, true
			}
		}
	}
	return core.Job{}, 0, false
}

func sortJobs(jobs []core.Job) {
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].StartedAt.IsZero() != jobs[j].StartedAt.IsZero() {
			return !jobs[i].StartedAt.IsZero()
		}
		if jobs[i].StartedAt.IsZero() {
			return false
		}
		if jobs[i].StartedAt.Equal(jobs[j].StartedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].StartedAt.Before(jobs[j].StartedAt)
	})
}
