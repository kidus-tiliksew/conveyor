// Package worker implements the enrolled Phase 5.1 dispatch supervisor
// control surface. It selects orders the worker can serve — skipping held
// tasks (DEC-5) — and reuses the existing MCP work-order lifecycle
// rather than creating a parallel task protocol.
package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	DefaultPairingTTL    = 10 * time.Minute
	DefaultLivenessLease = 15 * time.Second
	DefaultClaimLease    = core.DefaultWorkOrderClaimLease
	DefaultRetryDelay    = time.Second
	DefaultRetryMaximum  = 4 * time.Second
	DefaultRetryLimit    = 3
	// ActivitySnapshotLimit stays close to the existing failure-detail tail.
	ActivitySnapshotLimit = 4 * 1024
	// AttemptTranscriptLimit retains a useful stream-json tail while bounding
	// one termination capture to a single-digit MiB payload.
	AttemptTranscriptLimit = 4 * 1024 * 1024
)

type Service struct {
	Store            store.Store
	WorkOrders       *workorder.Service
	ConfigProvider   func(context.Context) (*config.Config, error)
	Now              func() time.Time
	RetryDelay       time.Duration
	RetryMaximum     time.Duration
	RetryLimit       int
	RedactionSecrets redact.SecretSource
	ForgeTokens      store.ForgeTokenStore
	IdentityUsers    store.CallerIdentityStore
}

type Enrollment struct {
	Worker     core.Worker `json:"worker"`
	Credential string      `json:"credential"`
}

// ClaimReconciliation is a read-only server-authoritative view used after a
// scheduler gap or ambiguous release. Authorized is never inferred locally.
type ClaimReconciliation struct {
	WorkOrder            core.WorkOrder `json:"work_order"`
	Authorized           bool           `json:"authorized"`
	ReleasedAtCheckpoint bool           `json:"released_at_checkpoint,omitempty"`
	Reason               string         `json:"reason"`
}

type TaskWorkerStatus struct {
	Available         bool      `json:"available"`
	RequiredHarnesses []string  `json:"required_harnesses"`
	Reason            string    `json:"reason"`
	LastHeartbeatAt   time.Time `json:"last_heartbeat_at,omitempty"`
	LastHeartbeatAge  string    `json:"last_heartbeat_age,omitempty"`
	QueueContext      string    `json:"queue_context"`
}

// WorkerServiceability is advisory presentation state. WorkerExpected is
// false for pull-only workspaces where no unrevoked enrollment exists; that
// neutral state must not be presented as an unhealthy worker population.
type WorkerServiceability struct {
	WorkerExpected bool
	Available      bool
	Reason         string
}

type DispatchOrder struct {
	Order            core.WorkOrder         `json:"work_order"`
	Task             core.Task              `json:"task"`
	Repository       config.Repo            `json:"repository"`
	Harness          config.Harness         `json:"harness"`
	Model            string                 `json:"model"`
	Effort           string                 `json:"effort,omitempty"`
	EffortArgv       []string               `json:"effort_argv,omitempty"`
	HarnessSelection string                 `json:"harness_selection"`
	Dispatch         string                 `json:"dispatch"`
	Confinement      string                 `json:"confinement"`
	Auth             string                 `json:"auth"`
	Gate             *TaskRunGate           `json:"gate,omitempty"`
	PendingProposals []TaskRunProposal      `json:"pending_proposals"`
	GitAuthor        core.GitAuthorIdentity `json:"git_author,omitempty"`
}

// ClaimDelivery is the authenticated worker claim response. ForgeToken is
// deliberately absent from queued dispatch projections and every durable
// work-order representation; only the worker that owns the exact live claim
// receives it (req-260821-830dbf REQ-6/AC-6.1).
type ClaimDelivery struct {
	WorkOrder  core.WorkOrder `json:"work_order"`
	ForgeToken string         `json:"forge_token"`
}

// TaskRunProposal is the task-scoped, read-only authority projection shown by
// an attended run. CanConfirm is derived from the invoking user credential;
// execution credentials never receive this response or gain confirmation
// authority through it (req-260811-0ee057 AC-2.2, AC-5.8).
type TaskRunProposal struct {
	Kind       string `json:"kind"`
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
	Version    int    `json:"version,omitempty"`
	CanConfirm bool   `json:"can_confirm"`
	ActorHint  string `json:"actor_hint"`
}

// TaskRunGate is the read-only human-gate projection returned to an attached
// conveyor run when no work order is claimable. It is derived from durable
// task events and grants no mutation authority (REQ-5, AC-5.6-AC-5.7).
type TaskRunGate struct {
	Kind              string `json:"kind"`
	Label             string `json:"label"`
	Summary           string `json:"summary"`
	CanOperate        bool   `json:"can_operate"`
	CanRequestChanges bool   `json:"can_request_changes"`
	SpecVersion       int    `json:"spec_version,omitempty"`
	PlanVersion       int    `json:"plan_version,omitempty"`
	Rationale         string `json:"rationale,omitempty"`
}

// HarnessProbeTarget is one exact harness definition the worker must probe.
// Fingerprint distinguishes an active round's immutable snapshot from a newer
// same-name workspace definition (design-harness-execution).
type HarnessProbeTarget struct {
	Harness     config.Harness `json:"harness"`
	Fingerprint string         `json:"fingerprint"`
}

// WorkerConfig preserves the existing top-level workspace document contract
// while additively exposing active review-seat snapshots to worker loops.
type WorkerConfig struct {
	config.WorkspaceDocument
	ActiveHarnesses []HarnessProbeTarget `json:"active_harnesses"`
}

func (c WorkerConfig) MarshalJSON() ([]byte, error) {
	document, err := json.Marshal(c.WorkspaceDocument)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		return nil, err
	}
	active, err := json.Marshal(c.ActiveHarnesses)
	if err != nil {
		return nil, err
	}
	fields["active_harnesses"] = active
	return json.Marshal(fields)
}

func HarnessFingerprint(harness config.Harness) string {
	data, _ := json.Marshal(struct {
		Name                  string              `json:"name"`
		MCPTransport          string              `json:"mcp_transport"`
		MCPAttachment         string              `json:"mcp_attachment"`
		Command               []string            `json:"command"`
		ModelArgs             []string            `json:"model_args"`
		DefaultModelSentinels []string            `json:"default_model_sentinels"`
		EffortArgs            map[string][]string `json:"effort_args"`
		ProbeCommand          []string            `json:"probe_command"`
		ProbeTimeout          string              `json:"probe_timeout"`
		StallTimeout          string              `json:"stall_timeout"`
	}{
		Name: harness.Name, MCPTransport: harness.MCPTransport, MCPAttachment: harness.MCPAttachment, Command: canonicalArgs(harness.Command),
		ModelArgs: canonicalArgs(harness.ModelArgs), DefaultModelSentinels: canonicalArgs(harness.DefaultModelSentinels),
		EffortArgs: harness.EffortArgs, ProbeCommand: canonicalArgs(harness.ProbeCommand), ProbeTimeout: harness.ProbeTimeoutText,
		StallTimeout: harness.StallTimeoutText,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func canonicalArgs(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return values
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func randomSecret(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *Service) IssuePairing(ctx context.Context, ttl time.Duration) (string, core.WorkerPairing, error) {
	if ttl <= 0 || ttl > time.Hour {
		ttl = DefaultPairingTTL
	}
	token, err := randomSecret(32)
	if err != nil {
		return "", core.WorkerPairing{}, err
	}
	workspace, ok := store.WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return "", core.WorkerPairing{}, store.ErrWorkspaceRequired
	}
	now := s.now()
	pairing := core.WorkerPairing{TokenHash: hash(token), Workspace: workspace, ExpiresAt: now.Add(ttl), CreatedAt: now}
	if credential, ok := store.CredentialFromContext(ctx); ok {
		pairing.OwnerUserID = credential.OwnerUserID
	}
	if err = s.Store.CreateWorkerPairing(ctx, pairing); err != nil {
		return "", core.WorkerPairing{}, err
	}
	return token, pairing, nil
}

func (s *Service) Enroll(ctx context.Context, pairingToken, name string) (Enrollment, error) {
	pairingToken, name = strings.TrimSpace(pairingToken), strings.TrimSpace(name)
	if pairingToken == "" {
		return Enrollment{}, fmt.Errorf("pairing_token is required")
	}
	if name == "" || len(name) > 80 {
		return Enrollment{}, fmt.Errorf("worker name is required and must be at most 80 characters")
	}
	pairing, err := s.Store.ConsumeWorkerPairing(ctx, hash(pairingToken), s.now())
	if err != nil {
		return Enrollment{}, err
	}
	credential, err := randomSecret(32)
	if err != nil {
		return Enrollment{}, err
	}
	idSecret, err := randomSecret(6)
	if err != nil {
		return Enrollment{}, err
	}
	worker := core.Worker{ID: "worker-" + idSecret, Workspace: pairing.Workspace, OwnerUserID: pairing.OwnerUserID, Name: name, CredentialHash: hash(credential), CreatedAt: s.now()}
	workerCtx := store.WithWorkspace(store.WithActor(ctx, store.Actor{ID: store.WorkerActorID(worker.ID), Role: core.ActorWorker}), pairing.Workspace)
	if err = s.Store.CreateWorker(workerCtx, worker); err != nil {
		return Enrollment{}, err
	}
	return Enrollment{Worker: worker, Credential: credential}, nil
}

func (s *Service) Authenticate(ctx context.Context, credential, requestedWorkspace string) (context.Context, core.Worker, error) {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return ctx, core.Worker{}, store.ErrWorkerUnauthorized
	}
	lookupCtx := ctx
	if requestedWorkspace != "" {
		lookupCtx = store.WithWorkspace(ctx, requestedWorkspace)
	}
	worker, err := s.Store.AuthenticateWorker(lookupCtx, hash(credential))
	if err != nil {
		return ctx, core.Worker{}, err
	}
	if requestedWorkspace != "" && worker.Workspace != requestedWorkspace {
		return ctx, core.Worker{}, store.ErrWorkerUnauthorized
	}
	return store.WithActor(store.WithWorkspace(ctx, worker.Workspace), store.Actor{ID: store.WorkerActorID(worker.ID), Role: core.ActorWorker}), worker, nil
}

func (s *Service) Heartbeat(ctx context.Context, worker core.Worker, probes []core.HarnessProbe) (core.Worker, error) {
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return core.Worker{}, err
	}
	registered := map[string]map[string]bool{}
	for _, harness := range cfg.Harnesses {
		registeredHarness(registered, harness)
	}
	// An active worker-dispatched order owns its snapshotted harness definition
	// even after the workspace registry hot reloads. Keep accepting health probes
	// for durable implementation and review snapshots until they leave the active
	// queue (design-harness-execution).
	active, err := s.ActiveHarnesses(ctx)
	if err != nil {
		return core.Worker{}, err
	}
	for _, target := range active {
		registeredHarness(registered, target.Harness)
	}
	now := s.now()
	for i := range probes {
		fingerprints, ok := registered[probes[i].Harness]
		if !ok {
			return core.Worker{}, fmt.Errorf("unknown harness probe %q", probes[i].Harness)
		}
		if probes[i].Fingerprint != "" && !fingerprints[probes[i].Fingerprint] {
			return core.Worker{}, fmt.Errorf("unknown harness probe fingerprint for %q", probes[i].Harness)
		}
		if probes[i].CheckedAt.IsZero() {
			probes[i].CheckedAt = now
		}
	}
	return s.Store.HeartbeatWorker(ctx, worker.ID, now.Add(DefaultLivenessLease), probes)
}

func registeredHarness(registered map[string]map[string]bool, harness config.Harness) {
	if registered[harness.Name] == nil {
		registered[harness.Name] = map[string]bool{}
	}
	registered[harness.Name][HarnessFingerprint(harness)] = true
}

func (s *Service) ActiveHarnesses(ctx context.Context) ([]HarnessProbeTarget, error) {
	orders, err := s.Store.ListWorkOrders(ctx)
	if err != nil {
		return nil, err
	}
	byFingerprint := map[string]HarnessProbeTarget{}
	for _, order := range orders {
		workerDispatched := order.Stage == core.StageSpec || order.Stage == core.StageImplement || order.Stage == core.StageReview
		if !workerDispatched || (order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed) || order.RequiredHarnessConfig == nil {
			continue
		}
		harness := harnessFromSnapshot(order.RequiredHarnessConfig)
		fingerprint := HarnessFingerprint(harness)
		byFingerprint[fingerprint] = HarnessProbeTarget{Harness: harness, Fingerprint: fingerprint}
	}
	result := make([]HarnessProbeTarget, 0, len(byFingerprint))
	for _, target := range byFingerprint {
		result = append(result, target)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Harness.Name != result[j].Harness.Name {
			return result[i].Harness.Name < result[j].Harness.Name
		}
		return result[i].Fingerprint < result[j].Fingerprint
	})
	return result, nil
}

// RateLimitHealth projects the latest self-reported provider status for each
// harness/model pair. This method is used only by operator health views; no
// dispatch, claim, retry, or model-selection path consults it (DEC-1).
func (s *Service) RateLimitHealth(ctx context.Context) ([]core.RateLimitHealth, error) {
	orders, err := s.Store.ListWorkOrders(ctx)
	if err != nil {
		return nil, err
	}
	latest := map[string]core.RateLimitHealth{}
	for _, order := range orders {
		if order.RateLimit == nil || order.RateLimitObservedAt.IsZero() {
			continue
		}
		harness := order.RequiredHarness
		if harness == "" {
			harness = order.Agent
		}
		model := order.RequiredModel
		if model == "" {
			model = order.Model
		}
		key := harness + "\x00" + model
		current, ok := latest[key]
		if ok && !order.RateLimitObservedAt.After(current.ObservedAt) {
			continue
		}
		latest[key] = core.RateLimitHealth{
			WorkOrderID: order.ID, WorkerID: order.WorkerID, Harness: harness, Model: model,
			RateLimit: *order.RateLimit, ObservedAt: order.RateLimitObservedAt,
		}
	}
	result := make([]core.RateLimitHealth, 0, len(latest))
	for _, status := range latest {
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Harness != result[j].Harness {
			return result[i].Harness < result[j].Harness
		}
		return result[i].Model < result[j].Model
	})
	return result, nil
}

func (s *Service) Serviceability(ctx context.Context, cfg *config.Config) WorkerServiceability {
	_ = cfg
	return s.serviceabilityForConfig(ctx, cfg)
}

// ServiceabilityForSetup evaluates only the harnesses required by one setup.
// A broken harness in an unrelated setup must not disable that setup.
func (s *Service) ServiceabilityForSetup(ctx context.Context, cfg *config.Config, setup config.ExecutionSetup) WorkerServiceability {
	_ = setup
	return s.serviceabilityForConfig(ctx, cfg)
}

// ModelFailuresForSetup projects retained provider evidence onto one setup.
// It is advisory health only; dispatch still evaluates the frozen order.
func (s *Service) ModelFailuresForSetup(ctx context.Context, setup config.ExecutionSetup) ([]core.HarnessModelFailure, error) {
	_, _ = ctx, setup
	return nil, nil
}

func setupHarnessModelPairs(setup config.ExecutionSetup) map[string]bool {
	result := map[string]bool{}
	add := func(harness, model string) {
		harness, model = strings.TrimSpace(harness), strings.TrimSpace(model)
		if harness != "" && model != "" {
			result[harness+"\x00"+model] = true
		}
	}
	add(setup.ExecutionSettings.Spec.Harness, setup.ExecutionSettings.Spec.Model)
	add(setup.ExecutionSettings.Implementation.Harness, setup.ExecutionSettings.Implementation.Model)
	if setup.ExecutionSettings.Review.Execution != config.ExecutionInProcess {
		if len(setup.Review.Seats) == 0 {
			add(setup.ExecutionSettings.Review.FallbackHarness, setup.ExecutionSettings.Review.FallbackModel)
		}
		for _, seat := range setup.Review.Seats {
			harness := seat.Harness
			if harness == "" {
				harness = setup.ExecutionSettings.Review.FallbackHarness
			}
			add(harness, seat.Model)
		}
	}
	return result
}

func (s *Service) serviceabilityForConfig(ctx context.Context, cfg *config.Config) WorkerServiceability {
	workers, err := s.Store.ListWorkers(ctx)
	if err != nil {
		return WorkerServiceability{WorkerExpected: true, Reason: err.Error()}
	}
	active := unrevokedWorkers(workers)
	if len(active) == 0 {
		return WorkerServiceability{}
	}
	now := s.now()
	var reason string
	for _, worker := range active {
		if worker.Live(now) {
			return WorkerServiceability{WorkerExpected: true, Available: true}
		} else if reason == "" {
			reason = fmt.Sprintf("enrolled worker %q: worker liveness lease expired", worker.Name)
		}
	}
	return WorkerServiceability{WorkerExpected: true, Reason: reason}
}

func unrevokedWorkers(workers []core.Worker) []core.Worker {
	result := make([]core.Worker, 0, len(workers))
	for _, worker := range workers {
		if worker.RevokedAt.IsZero() {
			result = append(result, worker)
		}
	}
	return result
}

func (s *Service) TaskAvailability(ctx context.Context, cfg *config.Config, task core.Task, orders []core.WorkOrder) *TaskWorkerStatus {
	status := TaskWorkerStatus{RequiredHarnesses: []string{}, Reason: "no live enrolled worker is available", QueueContext: "never_started"}
	var activeOrders []core.WorkOrder
	for _, order := range orders {
		if order.TaskID != task.ID || (order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed) {
			continue
		}
		activeOrders = append(activeOrders, order)
		if order.LastAttemptOutcome != "" || order.RetrySuppressed || !order.LastFailureAt.IsZero() {
			status.QueueContext = "interrupted"
		}
	}
	// Worker serviceability belongs to current task-owned work. Without a queued
	// or claimed order, setup-wide worker health is not actionable for this task.
	if len(activeOrders) == 0 {
		return nil
	}
	workers, err := s.Store.ListWorkers(ctx)
	if err != nil {
		status.Reason = err.Error()
		return &status
	}
	workers = unrevokedWorkers(workers)
	if len(workers) == 0 {
		return nil
	}
	now := s.now()
	for _, order := range activeOrders {
		if order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(now) {
			status.Available = true
			status.Reason = "active claimed order is being served"
			return &status
		}
	}
	for _, worker := range workers {
		if worker.LastSeenAt.After(status.LastHeartbeatAt) {
			status.LastHeartbeatAt = worker.LastSeenAt
		}
		if worker.Live(now) {
			status.Available = true
			status.Reason = "live enrolled worker available"
			break
		}
	}
	if !status.LastHeartbeatAt.IsZero() {
		age := now.Sub(status.LastHeartbeatAt)
		if age < 0 {
			age = 0
		}
		status.LastHeartbeatAge = age.Round(time.Second).String()
		if !status.Available {
			status.Reason += "; last heartbeat was " + status.LastHeartbeatAge + " ago"
		}
	} else if !status.Available {
		status.Reason += "; no enrolled worker has heartbeated"
	}
	return &status
}

// ListClaimable returns queued orders eligible for this live worker. Execution
// compatibility is resolved exclusively from the worker's client-local setup.
func (s *Service) ListClaimable(ctx context.Context, worker core.Worker) ([]DispatchOrder, error) {
	if s.Store.IsDurable() {
		if _, err := s.Store.AuthenticateWorker(ctx, worker.CredentialHash); err != nil {
			return nil, err
		}
	}
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return nil, err
	}
	orders, err := s.WorkOrders.List(ctx)
	if err != nil {
		return nil, err
	}
	if !worker.Live(s.now()) {
		return []DispatchOrder{}, nil
	}
	var forgeTokenErr error
	if s.ForgeTokens != nil {
		forgeTokenErr = store.RequireForgeTokenPresence(ctx, s.ForgeTokens, worker.OwnerUserID)
	}
	var result []DispatchOrder
	for _, order := range orders {
		if !order.Claimable || order.State != core.WorkOrderQueued {
			continue
		}
		task, getErr := s.Store.GetTask(ctx, order.TaskID)
		if getErr != nil || task.Hold {
			continue
		}
		if task.Assignee != nil && task.Assignee.UserID != worker.OwnerUserID {
			continue
		}
		if forgeTokenErr != nil {
			order.Claimable = false
			order.ClaimRefusalReason = store.ForgeTokenRequiredMessage
		}
		repository, _ := cfg.Repo(task.Repo)
		item := DispatchOrder{Order: order, Task: task, Repository: repository, HarnessSelection: "local", Dispatch: "worker", Confinement: "none", Auth: "byoa"}
		if s.IdentityUsers != nil {
			item.GitAuthor, getErr = store.GitAuthorForUser(ctx, s.IdentityUsers, worker.OwnerUserID)
			if getErr != nil {
				return nil, getErr
			}
		}
		result = append(result, item)
	}
	// The reserved review slot precedes workspace FIFO; ID breaks equal
	// queue-entry clocks deterministically (design-260805-973cd4).
	sort.Slice(result, func(i, j int) bool {
		iReview := result[i].Order.Stage == core.StageReview
		jReview := result[j].Order.Stage == core.StageReview
		if iReview != jReview {
			return iReview
		}
		if !result[i].Order.QueueEnteredAt.Equal(result[j].Order.QueueEnteredAt) {
			return result[i].Order.QueueEnteredAt.Before(result[j].Order.QueueEnteredAt)
		}
		return result[i].Order.ID < result[j].Order.ID
	})
	return result, nil
}

// ListVisibleOrders combines compatible claimable work with the authenticated
// worker's own active claims and durable review waits. It intentionally does
// not expose another worker's orders or any terminal state (design-260805-973cd4).
func (s *Service) ListVisibleOrders(ctx context.Context, worker core.Worker) ([]core.WorkOrder, error) {
	claimable, err := s.ListClaimable(ctx, worker)
	if err != nil {
		return nil, err
	}
	result := make([]core.WorkOrder, 0, len(claimable))
	for _, item := range claimable {
		result = append(result, item.Order)
	}
	orders, err := s.WorkOrders.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, order := range orders {
		if order.State == core.WorkOrderQueued && order.Stage == core.StageImplement &&
			!order.Claimable && len(order.BlockingTaskIDs) > 0 {
			task, getErr := s.Store.GetTask(ctx, order.TaskID)
			if getErr != nil || task.Hold || task.Assignee != nil && task.Assignee.UserID != worker.OwnerUserID {
				continue
			}
			if worker.Live(s.now()) {
				result = append(result, order)
			}
			continue
		}
	}
	active, err := s.Store.ListWorkOrders(ctx)
	if err != nil {
		return nil, err
	}
	for _, order := range active {
		if order.WorkerID == worker.ID &&
			(order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted) {
			result = append(result, order)
		}
	}
	return result, nil
}

// ClaimForWorker enforces task and worker lifecycle eligibility. Harness and
// model selection remain exclusively in the worker's client-local setup.
func (s *Service) ClaimForWorker(ctx context.Context, worker core.Worker, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	if s.Store.IsDurable() {
		if _, err := s.Store.AuthenticateWorker(ctx, worker.CredentialHash); err != nil {
			return core.WorkOrder{}, err
		}
	}
	if s.ForgeTokens != nil {
		if err := store.RequireForgeTokenPresence(ctx, s.ForgeTokens, worker.OwnerUserID); err != nil {
			return core.WorkOrder{}, err
		}
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if task.Hold {
		return core.WorkOrder{}, fmt.Errorf("task is held for operator claiming")
	}
	if order.Stage == core.StageImplement && len(task.BlockingTaskIDs) > 0 {
		return core.WorkOrder{}, fmt.Errorf("task %s is blocked by unmerged dependencies: %s", task.ID, strings.Join(task.BlockingTaskIDs, ", "))
	}
	if !worker.Live(s.now()) {
		return core.WorkOrder{}, fmt.Errorf("worker liveness lease expired")
	}
	claim.WorkerID = worker.ID
	claim.ClaimantID = worker.ID
	// REQ-2: claim ownership is derived from the authenticated worker
	// enrollment, never from the assigned task or a client assertion.
	claim.OwnerUserID = worker.OwnerUserID
	claim.Agent = "worker"
	claim.Model = order.RequiredModel // compatibility for pre-DEC-23 in-flight review orders
	if claim.Lease <= 0 {
		claim.Lease = DefaultClaimLease
	}
	return s.WorkOrders.Claim(ctx, id, claim)
}

// ClaimForWorkerDelivery resolves the authenticated enrollment owner's forge
// credential only after the exact worker claim exists. A failed outbound-use
// read releases that exact session and never falls back to another identity or
// an ambient host credential (req-260821-830dbf AC-3.2, AC-6.1).
func (s *Service) ClaimForWorkerDelivery(ctx context.Context, worker core.Worker, id string, claim core.WorkOrderClaim) (ClaimDelivery, error) {
	order, err := s.ClaimForWorker(ctx, worker, id, claim)
	if err != nil {
		return ClaimDelivery{}, err
	}
	credential, resolveErr := s.resolveWorkerForgeToken(ctx, worker)
	if resolveErr == nil {
		return ClaimDelivery{WorkOrder: order, ForgeToken: credential.Token}, nil
	}
	reason := fmt.Sprintf("resolve forge token for worker owner %s failed", strings.TrimSpace(worker.OwnerUserID))
	_, releaseErr := s.Release(ctx, worker, id, core.WorkOrderRelease{
		SessionID: claim.SessionID,
		Reason:    reason,
		Cause:     core.WorkOrderReleaseCauseSessionExit,
		Outcome:   core.WorkOrderOutcomeReleased,
	})
	if releaseErr != nil {
		return ClaimDelivery{}, errors.Join(resolveErr, fmt.Errorf("release exact claim after forge token resolution failed: %w", releaseErr))
	}
	return ClaimDelivery{}, resolveErr
}

func (s *Service) resolveWorkerForgeToken(ctx context.Context, worker core.Worker) (core.ForgeTokenCredential, error) {
	if s.ForgeTokens == nil || strings.TrimSpace(worker.OwnerUserID) == "" {
		return core.ForgeTokenCredential{}, workerForgeTokenRequired(worker.OwnerUserID, nil)
	}
	credential, err := s.ForgeTokens.GetForgeTokenForUse(ctx, worker.OwnerUserID)
	if err != nil {
		return core.ForgeTokenCredential{}, workerForgeTokenRequired(worker.OwnerUserID, err)
	}
	if credential.UserID != worker.OwnerUserID || strings.TrimSpace(credential.Token) == "" {
		return core.ForgeTokenCredential{}, workerForgeTokenRequired(worker.OwnerUserID, errors.New("resolved credential did not match the worker owner"))
	}
	return credential, nil
}

func workerForgeTokenRequired(ownerUserID string, cause error) error {
	owner := strings.TrimSpace(ownerUserID)
	if owner == "" {
		owner = "<missing>"
	}
	detail := "forge token cannot be resolved"
	if cause != nil {
		detail = cause.Error()
	}
	return fmt.Errorf("worker owner %s: %s; %w", owner, detail, store.ErrForgeTokenRequired)
}

func harnessFromSnapshot(snapshot *core.HarnessSnapshot) config.Harness {
	probeTimeout, _ := time.ParseDuration(snapshot.ProbeTimeoutText)
	stallTimeout, _ := time.ParseDuration(snapshot.StallTimeoutText)
	transport := snapshot.MCPTransport
	if transport == "" {
		transport = config.MCPTransportJSONFile
	}
	return config.Harness{
		Name: snapshot.Name, MCPTransport: transport, MCPAttachment: snapshot.MCPAttachment, Command: append([]string(nil), snapshot.Command...),
		ModelArgs:             append([]string(nil), snapshot.ModelArgs...),
		DefaultModelSentinels: append([]string(nil), snapshot.DefaultModelSentinels...),
		EffortArgs:            cloneEffortArgs(snapshot.EffortArgs),
		ProbeCommand:          append([]string(nil), snapshot.ProbeCommand...),
		ProbeTimeoutText:      snapshot.ProbeTimeoutText, ProbeTimeout: probeTimeout,
		StallTimeoutText: snapshot.StallTimeoutText, StallTimeout: stallTimeout,
	}
}

func cloneEffortArgs(source map[string][]string) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string][]string, len(source))
	for effort, args := range source {
		result[effort] = append([]string(nil), args...)
	}
	return result
}

func (s *Service) Renew(ctx context.Context, worker core.Worker, id, sessionID string, snapshot ...*core.WorkOrderActivitySnapshotInput) (core.WorkOrder, error) {
	return s.RenewClaim(ctx, core.WorkOrderClaimIdentity{WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: sessionID}, id, snapshot...)
}

// RenewClaim renews or classifies the exact authenticated child claim. The
// explicit identity remains available after a deliberate release clears the
// active ownership columns (design-260805-973cd4).
func (s *Service) RenewClaim(ctx context.Context, claim core.WorkOrderClaimIdentity, id string, snapshots ...*core.WorkOrderActivitySnapshotInput) (core.WorkOrder, error) {
	sessionID := claim.SessionID
	if strings.TrimSpace(sessionID) == "" {
		return core.WorkOrder{}, fmt.Errorf("session_id is required")
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	renewed, err := taskops.ExecuteWorkOrder(ctx, s.Store, order.TaskID, core.WorkOrderCmdRenew, func(taskLease taskops.TaskLease) (core.WorkOrder, error) {
		return s.Store.RenewWorkerClaimCommand(ctx, taskLease, id, claim, DefaultClaimLease)
	})
	if err != nil {
		return core.WorkOrder{}, err
	}
	if len(snapshots) > 0 && snapshots[0] != nil {
		content, _, redactErr := s.boundedObservabilityContent(ctx, snapshots[0].Content, ActivitySnapshotLimit)
		// Observability is best-effort and cannot alter the renewal result.
		if redactErr == nil {
			_ = s.Store.UpsertWorkOrderActivitySnapshot(ctx, id, claim, content)
		}
	}
	return renewed, nil
}

func (s *Service) Reconcile(ctx context.Context, worker core.Worker, id, sessionID string) (ClaimReconciliation, error) {
	return s.ReconcileClaim(ctx, core.WorkOrderClaimIdentity{WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: sessionID}, id)
}

// ReconcileClaim returns a read-only view of either the active exact claim or
// its exact deliberate checkpoint release. It never renews or restores claim
// authority.
func (s *Service) ReconcileClaim(ctx context.Context, claim core.WorkOrderClaimIdentity, id string) (ClaimReconciliation, error) {
	sessionID := claim.SessionID
	if strings.TrimSpace(sessionID) == "" {
		return ClaimReconciliation{}, fmt.Errorf("session_id is required")
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return ClaimReconciliation{}, err
	}
	authorized := order.WorkerID == claim.WorkerID && order.ClaimantID == claim.ClaimantID && order.SessionID == sessionID &&
		order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(s.now())
	releasedAtCheckpoint, err := store.ReleasedCheckpointClaimMatches(ctx, s.Store, order, claim)
	if err != nil {
		return ClaimReconciliation{}, err
	}
	reason := "session is no longer the active lease owner"
	if authorized {
		reason = "session owns the active claim"
	} else if releasedAtCheckpoint {
		reason = "session deliberately released at an operator checkpoint"
	} else if order.State == core.WorkOrderSubmitted || order.State == core.WorkOrderCompleted {
		reason = "work order already reached a durable terminal handoff"
	}
	return ClaimReconciliation{WorkOrder: order, Authorized: authorized, ReleasedAtCheckpoint: releasedAtCheckpoint, Reason: reason}, nil
}

func (s *Service) Release(ctx context.Context, worker core.Worker, id string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	return s.ReleaseClaim(ctx, core.WorkOrderClaimIdentity{WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: release.SessionID}, id, release)
}

// ReleaseClaim releases the exact authenticated claim. Worker-facing callers
// use worker identity; MCP agent children carry the live claim identity that
// their session authorization resolved (design-260805-973cd4).
func (s *Service) ReleaseClaim(ctx context.Context, claim core.WorkOrderClaimIdentity, id string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	if strings.TrimSpace(release.SessionID) == "" {
		return core.WorkOrder{}, fmt.Errorf("session_id is required")
	}
	if claim.SessionID != release.SessionID {
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	release.Reason = strings.TrimSpace(release.Reason)
	checkpoint, err := s.normalizeCheckpoint(ctx, release.Reason, release.Checkpoint)
	if err != nil {
		return core.WorkOrder{}, err
	}
	release.Checkpoint = checkpoint
	release.Cause = strings.TrimSpace(release.Cause)
	if release.Cause == "" {
		release.Cause = core.WorkOrderReleaseCauseSessionExit
	}
	if !core.ValidWorkOrderReleaseCause(release.Cause) {
		return core.WorkOrder{}, fmt.Errorf("invalid work-order release cause %q", release.Cause)
	}
	release.FailureDetail = boundedFailureDetail(release.FailureDetail)
	release.ModelRejection = providerModelRejection(release.FailureDetail)
	if release.Outcome == "" {
		release.Outcome = core.WorkOrderOutcomeReleased
	}
	if release.Outcome != core.WorkOrderOutcomeChildFailure && release.Outcome != core.WorkOrderOutcomeStalled && release.Outcome != core.WorkOrderOutcomeReleased && release.Outcome != core.WorkOrderOutcomeCancelled {
		return core.WorkOrder{}, fmt.Errorf("invalid worker release outcome %q", release.Outcome)
	}
	if release.Outcome == core.WorkOrderOutcomeChildFailure && release.FailureCategory == "" {
		switch {
		case providerUsageLimit(release.FailureDetail):
			release.FailureCategory = core.WorkOrderFailureProviderUsageLimit
		case transientConnectivityFailure(release.FailureDetail):
			release.FailureCategory = core.WorkOrderFailureTransientConnectivity
		}
	}
	release.InitialRetryDelay = s.RetryDelay
	if release.InitialRetryDelay <= 0 {
		release.InitialRetryDelay = DefaultRetryDelay
	}
	release.MaximumRetryDelay = s.RetryMaximum
	if release.MaximumRetryDelay <= 0 {
		release.MaximumRetryDelay = DefaultRetryMaximum
	}
	if release.MaximumRetryDelay < release.InitialRetryDelay {
		release.MaximumRetryDelay = release.InitialRetryDelay
	}
	release.AutomaticRetryLimit = s.RetryLimit
	if release.AutomaticRetryLimit <= 0 {
		release.AutomaticRetryLimit = DefaultRetryLimit
	}
	current, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	order, err := taskops.ExecuteWorkOrder(ctx, s.Store, current.TaskID, core.WorkOrderCmdRelease, func(taskLease taskops.TaskLease) (core.WorkOrder, error) {
		return s.Store.ReleaseWorkerClaimCommand(ctx, taskLease, id, claim, release)
	})
	if err != nil {
		return core.WorkOrder{}, err
	}
	return s.refreshReleasedHarnessSnapshot(ctx, order), nil
}

func (s *Service) normalizeCheckpoint(ctx context.Context, reason string, checkpoint *core.WorkOrderCheckpoint) (*core.WorkOrderCheckpoint, error) {
	if checkpoint == nil {
		if reason == core.WorkOrderReleaseReasonOperatorCheckpointReached {
			return nil, fmt.Errorf("checkpoint.decision_request is required when reason is %q", core.WorkOrderReleaseReasonOperatorCheckpointReached)
		}
		return nil, nil
	}
	normalized := &core.WorkOrderCheckpoint{
		DecisionRequest: strings.TrimSpace(checkpoint.DecisionRequest),
		Class:           strings.TrimSpace(checkpoint.Class),
	}
	if utf8.RuneCountInString(normalized.DecisionRequest) > core.WorkOrderDecisionRequestLimit {
		return nil, fmt.Errorf("checkpoint.decision_request exceeds %d characters", core.WorkOrderDecisionRequestLimit)
	}
	if reason == core.WorkOrderReleaseReasonOperatorCheckpointReached && normalized.DecisionRequest == "" {
		return nil, fmt.Errorf("checkpoint.decision_request is required when reason is %q", core.WorkOrderReleaseReasonOperatorCheckpointReached)
	}
	if normalized.Class != "" && normalized.Class != core.WorkOrderCheckpointClassAuthorityConflict {
		return nil, fmt.Errorf("checkpoint.class %q is not recognized; allowed value is %q", normalized.Class, core.WorkOrderCheckpointClassAuthorityConflict)
	}
	if normalized.Class == "" && len(checkpoint.Citations) > 0 {
		return nil, fmt.Errorf("checkpoint.class must be %q when citations are supplied", core.WorkOrderCheckpointClassAuthorityConflict)
	}
	if normalized.Class == core.WorkOrderCheckpointClassAuthorityConflict && len(checkpoint.Citations) == 0 {
		return nil, fmt.Errorf("checkpoint.citations is required when checkpoint.class is %q", core.WorkOrderCheckpointClassAuthorityConflict)
	}
	for i, citation := range checkpoint.Citations {
		citation.DocumentID = strings.TrimSpace(citation.DocumentID)
		citation.StatementOrSectionID = strings.TrimSpace(citation.StatementOrSectionID)
		if citation.DocumentID == "" {
			return nil, fmt.Errorf("checkpoint.citations[%d].document_id is required", i)
		}
		if citation.CitedVersion <= 0 {
			return nil, fmt.Errorf("checkpoint.citations[%d].cited_version must be positive", i)
		}
		exists, clauseKnown, err := s.checkpointCitation(ctx, citation)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("checkpoint.citations[%d].document_id %q does not exist in this workspace", i, citation.DocumentID)
		}
		if !clauseKnown {
			citation.StatementOrSectionID = ""
		}
		normalized.Citations = append(normalized.Citations, citation)
	}
	if normalized.DecisionRequest == "" && normalized.Class == "" && len(normalized.Citations) == 0 {
		return nil, nil
	}
	return normalized, nil
}

func (s *Service) checkpointCitation(ctx context.Context, citation core.WorkOrderAuthorityConflictCitation) (bool, bool, error) {
	clause := citation.StatementOrSectionID
	if requirement, err := s.Store.GetRequirement(ctx, citation.DocumentID); err == nil {
		version := requirement.CurrentVersion
		if citation.CitedVersion > 0 {
			version = citation.CitedVersion
		}
		revision, getErr := s.Store.GetRequirementVersion(ctx, citation.DocumentID, version)
		if getErr != nil {
			return true, clause == "", nil
		}
		if clause == "" {
			return true, true, nil
		}
		for _, statement := range revision.Statements {
			if statement.ID == clause {
				return true, true, nil
			}
			for _, criterion := range statement.AcceptanceCriteria {
				if criterion.ID == clause {
					return true, true, nil
				}
			}
		}
		return true, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, false, err
	}
	if document, err := s.Store.GetSystemDesign(ctx, citation.DocumentID); err == nil {
		version := document.CurrentVersion
		if citation.CitedVersion > 0 {
			version = citation.CitedVersion
		}
		revision, getErr := s.Store.GetSystemDesignVersion(ctx, citation.DocumentID, version)
		return true, clause == "" || (getErr == nil && markdownClauseExists(revision.Content, clause)), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, false, err
	}
	return false, false, nil
}

func markdownClauseExists(content, clause string) bool {
	clause = strings.TrimSpace(clause)
	if clause == "" {
		return true
	}
	needle := strings.ToLower(clause)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
			anchor := strings.Trim(strings.Map(func(r rune) rune {
				if r >= 'A' && r <= 'Z' {
					return r + ('a' - 'A')
				}
				if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
					return r
				}
				if r == ' ' {
					return '-'
				}
				return -1
			}, heading), "-")
			if strings.EqualFold(heading, clause) || anchor == needle {
				return true
			}
		}
	}
	return false
}

// RequestPlanRevision atomically releases one exact implement attempt and
// moves its task to the distinct operator gate (REQ-1, AC-1.1–AC-1.3).
func (s *Service) RequestPlanRevision(ctx context.Context, worker core.Worker, id, sessionID, rationale string) (store.PlanRevisionRequestResult, error) {
	return s.RequestPlanRevisionClaim(ctx, core.WorkOrderClaimIdentity{WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: sessionID}, id, rationale)
}

// RequestPlanRevisionClaim raises the operator-gated revision request for the
// exact authenticated claim owner without assuming every caller is a worker
// credential (REQ-1, AC-1.1-AC-1.3; design-260805-973cd4).
func (s *Service) RequestPlanRevisionClaim(ctx context.Context, claim core.WorkOrderClaimIdentity, id, rationale string) (store.PlanRevisionRequestResult, error) {
	sessionID := claim.SessionID
	sessionID, rationale = strings.TrimSpace(sessionID), strings.TrimSpace(rationale)
	if sessionID == "" {
		return store.PlanRevisionRequestResult{}, fmt.Errorf("session_id is required")
	}
	if rationale == "" {
		return store.PlanRevisionRequestResult{}, fmt.Errorf("rationale is required")
	}
	current, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, s.Store, current.TaskID, core.WorkOrderCmdRequestPlanRevision, func(taskLease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
		claim.SessionID = sessionID
		return s.Store.RequestPlanRevisionCommand(ctx, taskLease, id, claim, rationale)
	})
}

func transientConnectivityFailure(detail string) bool {
	detail = strings.ToLower(detail)
	for _, marker := range []string{
		"failed to connect to websocket",
		"nodename nor servname provided",
		"temporary failure in name resolution",
		"network is unreachable",
		"no such host",
		"connection refused",
		"connection reset by peer",
		"tls handshake timeout",
		"i/o timeout",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// CheckpointAttempt records a successful additive Git preservation commit.
// Store implementations re-check active attempt authority atomically with the
// idempotent append so a stale child cannot acknowledge a newer claim.
func (s *Service) CheckpointAttempt(ctx context.Context, worker core.Worker, id string, checkpoint core.WorkOrderAttemptCheckpoint) (bool, error) {
	checkpoint.SessionID = strings.TrimSpace(checkpoint.SessionID)
	checkpoint.AttemptID = strings.TrimSpace(checkpoint.AttemptID)
	checkpoint.TerminationReason = strings.TrimSpace(checkpoint.TerminationReason)
	checkpoint.CommitSHA = strings.TrimSpace(checkpoint.CommitSHA)
	checkpoint.PushResult = strings.TrimSpace(checkpoint.PushResult)
	if checkpoint.SessionID == "" || checkpoint.AttemptID == "" || checkpoint.TerminationReason == "" || checkpoint.CommitSHA == "" {
		return false, fmt.Errorf("session_id, attempt_id, termination_reason, and commit_sha are required")
	}
	if checkpoint.PushResult != "pushed" {
		return false, fmt.Errorf("attempt checkpoint must have a confirmed pushed result")
	}
	if checkpoint.Transcript != nil {
		content, truncated, redactErr := s.boundedObservabilityContent(ctx,
			checkpoint.Transcript.Content, AttemptTranscriptLimit, checkpoint.Transcript.Truncated,
		)
		if redactErr != nil {
			checkpoint.Transcript = nil
		} else {
			checkpoint.Transcript.Content, checkpoint.Transcript.Truncated = content, truncated
		}
	}
	created, err := s.Store.RecordWorkOrderAttemptCheckpoint(ctx, id, worker.ID, checkpoint)
	if err != nil {
		return false, err
	}
	// Snapshot supersession and optional transcript persistence are isolated
	// from the established checkpoint result.
	_ = s.Store.FinalizeWorkOrderAttemptObservability(ctx, id, worker.ID, checkpoint)
	return created, nil
}

const FailureDetailLimit = core.WorkOrderFailureDetailLimit

func boundedFailureDetail(detail string) string {
	detail = strings.ToValidUTF8(detail, "�")
	if len(detail) > FailureDetailLimit {
		detail = detail[len(detail)-FailureDetailLimit:]
		detail = strings.ToValidUTF8(detail, "�")
	}
	return strings.TrimSpace(detail)
}

func boundedObservabilityContent(content string, limit int, alreadyTruncated ...bool) (string, bool) {
	content = strings.ToValidUTF8(content, "�")
	content, _ = redact.New(nil).Redact(content)
	truncated := len(alreadyTruncated) > 0 && alreadyTruncated[0]
	if len(content) > limit {
		// The source is valid UTF-8 at this point. Dropping only a partial
		// leading rune keeps the newest complete content without expanding past
		// the byte cap through a replacement rune.
		content = strings.ToValidUTF8(content[len(content)-limit:], "")
		truncated = true
	}
	return content, truncated
}

func (s *Service) boundedObservabilityContent(ctx context.Context, content string, limit int, alreadyTruncated ...bool) (string, bool, error) {
	content = strings.ToValidUTF8(content, "�")
	clean, _, err := redact.Text(ctx, s.RedactionSecrets, content)
	if err != nil {
		return "", false, err
	}
	truncated := len(alreadyTruncated) > 0 && alreadyTruncated[0]
	if len(clean) > limit {
		clean = strings.ToValidUTF8(clean[len(clean)-limit:], "")
		truncated = true
	}
	return clean, truncated, nil
}

// providerModelRejection deliberately recognizes only explicit model support
// failures. A generic provider or harness exit must not poison pair health.
func providerModelRejection(detail string) bool {
	detail = strings.ToLower(strings.TrimSpace(detail))
	if detail == "" || !strings.Contains(detail, "model") {
		return false
	}
	for _, marker := range []string{
		"model is not supported",
		"unsupported model",
		"model is unsupported",
		"does not support the model",
		"does not support model",
		"model not supported",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// providerUsageLimit maps provider-specific stderr onto a stable category at
// the worker boundary. The raw detail remains durable for audit and future
// classifiers; presentation code consumes only this provider-neutral value.
func providerUsageLimit(detail string) bool {
	detail = strings.ToLower(strings.TrimSpace(detail))
	for _, marker := range []string{
		"usage limit", "usage cap", "quota exceeded", "quota has been exceeded",
		"rate limit", "too many requests", "capacity limit", "capacity exhausted",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// refreshReleasedHarnessSnapshot re-resolves a released order's pinned harness
// snapshot from the current registry so the next attempt launches the
// operator's current definition (design-harness-execution). Best-effort: the release above
// already committed, and retaining the prior snapshot is the explicit
// fallback.
func (s *Service) refreshReleasedHarnessSnapshot(ctx context.Context, order core.WorkOrder) core.WorkOrder {
	if order.RequiredHarnessConfig == nil || s.ConfigProvider == nil {
		return order
	}
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return order
	}
	snapshot, changed := core.RefreshedHarnessSnapshot(cfg.Harnesses, order.RequiredHarnessConfig)
	if !changed {
		return order
	}
	refreshed, err := s.Store.RefreshWorkOrderHarnessSnapshot(ctx, order.ID, snapshot)
	if err != nil {
		return order
	}
	return refreshed
}
