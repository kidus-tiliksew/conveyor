// Package worker implements the enrolled Phase 5.1 dispatch supervisor
// control surface. It selects orders the worker can serve — skipping held
// tasks (spec §21.31) — and reuses the existing MCP work-order lifecycle
// rather than creating a parallel task protocol.
package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
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
)

type Service struct {
	Store          store.Store
	WorkOrders     *workorder.Service
	ConfigProvider func(context.Context) (*config.Config, error)
	Now            func() time.Time
	RetryDelay     time.Duration
	RetryMaximum   time.Duration
	RetryLimit     int
}

type Enrollment struct {
	Worker     core.Worker `json:"worker"`
	Credential string      `json:"credential"`
}

// ClaimReconciliation is a read-only server-authoritative view used after a
// scheduler gap or ambiguous release. Authorized is never inferred locally.
type ClaimReconciliation struct {
	WorkOrder  core.WorkOrder `json:"work_order"`
	Authorized bool           `json:"authorized"`
	Reason     string         `json:"reason"`
}

type TaskWorkerStatus struct {
	Available         bool      `json:"available"`
	RequiredHarnesses []string  `json:"required_harnesses"`
	Reason            string    `json:"reason"`
	LastHeartbeatAt   time.Time `json:"last_heartbeat_at,omitempty"`
	LastHeartbeatAge  string    `json:"last_heartbeat_age,omitempty"`
	QueueContext      string    `json:"queue_context"`
}

type DispatchOrder struct {
	Order            core.WorkOrder `json:"work_order"`
	Task             core.Task      `json:"task"`
	Repository       config.Repo    `json:"repository"`
	Harness          config.Harness `json:"harness"`
	Model            string         `json:"model"`
	Effort           string         `json:"effort,omitempty"`
	EffortArgv       []string       `json:"effort_argv,omitempty"`
	HarnessSelection string         `json:"harness_selection"`
	Dispatch         string         `json:"dispatch"`
	Confinement      string         `json:"confinement"`
	Auth             string         `json:"auth"`
}

// HarnessProbeTarget is one exact harness definition the worker must probe.
// Fingerprint distinguishes an active round's immutable snapshot from a newer
// same-name workspace definition (spec §21.12 change 4).
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
	worker := core.Worker{ID: "worker-" + idSecret, Workspace: pairing.Workspace, Name: name, CredentialHash: hash(credential), CreatedAt: s.now()}
	workerCtx := store.WithWorkspace(store.WithActor(ctx, store.Actor{ID: worker.ID, Role: core.ActorRunner}), pairing.Workspace)
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
	return store.WithActor(store.WithWorkspace(ctx, worker.Workspace), store.Actor{ID: worker.ID, Role: core.ActorRunner}), worker, nil
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
	// queue (spec §21.18 change 5).
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
// dispatch, claim, retry, or model-selection path consults it (spec §14).
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

func (s *Service) AutoAvailable(ctx context.Context, cfg *config.Config) (bool, string) {
	setup, ok := cfg.Setup("")
	if ok {
		return s.AutoAvailableForSetup(ctx, cfg, setup)
	}
	return s.autoAvailableForConfig(ctx, cfg)
}

// AutoAvailableForSetup evaluates only the harnesses required by one setup.
// A broken harness in an unrelated setup must not disable Auto (spec §21.27).
func (s *Service) AutoAvailableForSetup(ctx context.Context, cfg *config.Config, setup config.ExecutionSetup) (bool, string) {
	if setup.Name == "" {
		var ok bool
		setup, ok = cfg.Setup("")
		if !ok {
			return false, "workspace has no valid default setup"
		}
	}
	failures, err := s.ModelFailuresForSetup(ctx, setup)
	if err != nil {
		return false, err.Error()
	}
	if len(failures) > 0 {
		failure := failures[0]
		return false, fmt.Sprintf("known provider rejection for %s / %s (observed on work order %s)", failure.Harness, failure.Model, failure.WorkOrderID)
	}
	return s.autoAvailableForConfig(ctx, cfg.WithSetup(setup))
}

// ModelFailuresForSetup projects retained provider evidence onto one setup.
// It is advisory health only; dispatch still evaluates the frozen order.
func (s *Service) ModelFailuresForSetup(ctx context.Context, setup config.ExecutionSetup) ([]core.HarnessModelFailure, error) {
	known, err := s.Store.ListHarnessModelFailures(ctx)
	if err != nil {
		return nil, err
	}
	pairs := setupHarnessModelPairs(setup)
	result := make([]core.HarnessModelFailure, 0)
	for _, failure := range known {
		if pairs[failure.Harness+"\x00"+failure.Model] {
			result = append(result, failure)
		}
	}
	return result, nil
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

func (s *Service) autoAvailableForConfig(ctx context.Context, cfg *config.Config) (bool, string) {
	workers, err := s.Store.ListWorkers(ctx)
	if err != nil {
		return false, err.Error()
	}
	now := s.now()
	for _, worker := range workers {
		if healthy, _ := workerHealthyForRoutes(worker, cfg, now); healthy {
			return true, ""
		}
	}
	return false, "no live worker reports every routed harness healthy"
}

func (s *Service) TaskAvailability(ctx context.Context, cfg *config.Config, task core.Task, orders []core.WorkOrder) *TaskWorkerStatus {
	if task.SetupContract.Name != "" {
		cfg = cfg.WithSetup(task.SetupContract)
	}
	status := TaskWorkerStatus{RequiredHarnesses: []string{}, Reason: "no healthy worker can serve the task's required harnesses", QueueContext: "never_started"}
	required := map[string]bool{}
	var activeOrders []core.WorkOrder
	for _, order := range orders {
		if order.TaskID != task.ID || (order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed) {
			continue
		}
		activeOrders = append(activeOrders, order)
		if order.RequiredHarness != "" {
			required[order.RequiredHarness] = true
		}
		if order.LastAttemptOutcome != "" || order.RetrySuppressed || !order.LastFailureAt.IsZero() {
			status.QueueContext = "interrupted"
		}
	}
	// Worker serviceability belongs to current task-owned work. Without a queued
	// or claimed order, setup-wide worker health is not actionable for this task.
	if len(activeOrders) == 0 {
		return nil
	}
	if len(required) == 0 {
		if setupHarnesses, setupErr := requiredHarnesses(cfg); setupErr == nil {
			for name := range setupHarnesses {
				required[name] = true
			}
		}
	}
	for harness := range required {
		status.RequiredHarnesses = append(status.RequiredHarnesses, harness)
	}
	sort.Strings(status.RequiredHarnesses)
	workers, err := s.Store.ListWorkers(ctx)
	if err != nil {
		status.Reason = err.Error()
		return &status
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
		if !worker.Live(now) {
			continue
		}
		healthy := true
		for _, order := range activeOrders {
			if ok, _ := s.workerHealthyForOrder(worker, cfg, order); !ok {
				healthy = false
				break
			}
		}
		if healthy {
			status.Available = true
			status.Reason = "healthy worker available"
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

func workerHealthyForRoutes(worker core.Worker, cfg *config.Config, now time.Time) (bool, string) {
	if !worker.Live(now) {
		return false, "worker liveness lease expired"
	}
	required, err := requiredHarnesses(cfg)
	if err != nil {
		return false, err.Error()
	}
	if len(required) == 0 {
		return false, "no routed worker harness is configured"
	}
	for name, harness := range required {
		if !probeHealthy(worker.Probes, name, HarnessFingerprint(harness), true) {
			return false, fmt.Sprintf("routed harness %s is unhealthy", name)
		}
	}
	return true, ""
}

func requiredHarnesses(cfg *config.Config) (map[string]config.Harness, error) {
	result := map[string]config.Harness{}
	byName := map[string]config.Harness{}
	for _, harness := range cfg.Harnesses {
		byName[harness.Name] = harness
	}
	for _, stage := range []string{"spec", "implement"} {
		route, configured := cfg.Routing.Stages[stage]
		if !configured || (stage == "spec" && route.Execution != config.ExecutionMCP) {
			continue
		}
		if route.Harness == "" {
			return nil, fmt.Errorf("%s route has no harness", stage)
		}
		harness, ok := byName[route.Harness]
		if !ok {
			return nil, fmt.Errorf("%s route harness %s is unavailable", stage, route.Harness)
		}
		result[route.Harness] = harness
	}
	reviewRoute := cfg.Routing.Stages["review"]
	if reviewRoute.Execution != config.ExecutionInProcess {
		seats := cfg.Review.Seats
		if len(seats) == 0 {
			seats = []config.ReviewSeat{{Model: reviewRoute.Model, Harness: reviewRoute.Harness}}
		}
		for i, seat := range seats {
			harness := seat.Harness
			if harness == "" {
				harness = reviewRoute.Harness
			}
			if harness == "" {
				return nil, fmt.Errorf("review seat %d has no harness", i+1)
			}
			definition, ok := byName[harness]
			if !ok {
				return nil, fmt.Errorf("review seat %d harness %s is unavailable", i+1, harness)
			}
			result[harness] = definition
		}
	}
	return result, nil
}

// ListClaimable returns the queued orders this worker may claim (§21.31):
// every order whose task is not held and whose frozen setup the worker's
// healthy harnesses can serve.
func (s *Service) ListClaimable(ctx context.Context, worker core.Worker) ([]DispatchOrder, error) {
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return nil, err
	}
	orders, err := s.WorkOrders.List(ctx)
	if err != nil {
		return nil, err
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
		orderCfg := cfg
		if task.SetupContract.Name != "" {
			orderCfg = cfg.WithSetup(task.SetupContract)
		}
		if healthy, _ := s.workerHealthyForOrder(worker, orderCfg, order); !healthy {
			continue
		}
		harness, ok := harnessForOrder(orderCfg, order)
		if !ok {
			continue
		}
		model := cfg.EffectiveModel(string(order.Stage))
		if order.RequiredModel != "" {
			model = order.RequiredModel
		}
		var effortArgv []string
		if order.RequiredEffort != "" && order.RequiredHarnessConfig != nil {
			effortArgv = append([]string(nil), order.RequiredHarnessConfig.EffortArgv...)
		}
		repository, _ := cfg.Repo(task.Repo)
		result = append(result, DispatchOrder{Order: order, Task: task, Repository: repository, Harness: harness, Model: model, Effort: order.RequiredEffort, EffortArgv: effortArgv, HarnessSelection: "enforced", Dispatch: "worker", Confinement: "none", Auth: "byoa"})
	}
	// The reserved review slot precedes workspace FIFO; ID breaks equal
	// queue-entry clocks deterministically (spec §6.3).
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
// not expose another worker's orders or any terminal state (spec §21.38).
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
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return nil, err
	}
	for _, order := range orders {
		if order.State == core.WorkOrderQueued && order.Stage == core.StageImplement &&
			!order.Claimable && len(order.BlockingTaskIDs) > 0 {
			task, getErr := s.Store.GetTask(ctx, order.TaskID)
			if getErr != nil || task.Hold {
				continue
			}
			orderCfg := cfg
			if task.SetupContract.Name != "" {
				orderCfg = cfg.WithSetup(task.SetupContract)
			}
			if healthy, _ := s.workerHealthyForOrder(worker, orderCfg, order); !healthy {
				continue
			}
			if _, ok := harnessForOrder(orderCfg, order); ok {
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

// ClaimForWorker is the server-side worker claim: held tasks are rejected at
// claim time — the same enforcement layer as the self-review guard — and the
// claiming worker must probe healthy for every harness the order requires.
func (s *Service) ClaimForWorker(ctx context.Context, worker core.Worker, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return core.WorkOrder{}, err
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
	if task.SetupContract.Name != "" {
		cfg = cfg.WithSetup(task.SetupContract)
	}
	if healthy, reason := s.workerHealthyForOrder(worker, cfg, order); !healthy {
		return core.WorkOrder{}, fmt.Errorf("worker cannot serve this order: %s", reason)
	}
	harness, ok := harnessForOrder(cfg, order)
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("no harness configured for %s", order.Stage)
	}
	claim.WorkerID = worker.ID
	claim.ClaimantID = worker.ID
	claim.Agent = harness.Name
	claim.Model = cfg.EffectiveModel(string(order.Stage))
	if order.RequiredModel != "" {
		claim.Model = order.RequiredModel
	}
	if claim.Lease <= 0 {
		claim.Lease = DefaultClaimLease
	}
	claimed, err := s.WorkOrders.Claim(ctx, id, claim)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if jobs, getErr := s.Store.ListJobs(ctx, task.ID); getErr == nil {
		for _, job := range jobs {
			if job.ID != claimed.JobID {
				continue
			}
			job.Harness = harness.Name
			job.ModelTier = claim.Model
			job.AuthMode = "byoa"
			job.Runner = "worker"
			job.Confinement = "none"
			_ = s.Store.UpdateJob(ctx, job)
			break
		}
	}
	return claimed, nil
}

func harnessForOrder(cfg *config.Config, order core.WorkOrder) (config.Harness, bool) {
	if snapshot := order.RequiredHarnessConfig; snapshot != nil && snapshot.Name != "" {
		return harnessFromSnapshot(snapshot), true
	}
	route, ok := cfg.Routing.Stages[string(order.Stage)]
	name := order.RequiredHarness
	if name == "" {
		name = route.Harness
	}
	if !ok || name == "" {
		return config.Harness{}, false
	}
	for _, harness := range cfg.Harnesses {
		if harness.Name == name {
			return harness, true
		}
	}
	return config.Harness{}, false
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

func (s *Service) workerHealthyForOrder(worker core.Worker, cfg *config.Config, order core.WorkOrder) (bool, string) {
	if order.RequiredHarnessConfig == nil || orderMatchesCurrentConfig(cfg, order) {
		return workerHealthyForRoutes(worker, cfg, s.now())
	}
	if order.RequiredHarnessConfig.Name != order.RequiredHarness {
		return false, "snapshotted harness identity does not match the work order"
	}
	if !worker.Live(s.now()) {
		return false, "worker liveness lease expired"
	}
	fingerprint := HarnessFingerprint(harnessFromSnapshot(order.RequiredHarnessConfig))
	if probeHealthy(worker.Probes, order.RequiredHarness, fingerprint, false) {
		return true, ""
	}
	return false, fmt.Sprintf("snapshotted harness %s is unhealthy", order.RequiredHarness)
}

func orderMatchesCurrentConfig(cfg *config.Config, order core.WorkOrder) bool {
	if order.Stage == core.StageReview {
		return reviewOrderMatchesCurrentConfig(cfg, order)
	}
	route, ok := cfg.Routing.Stages[string(order.Stage)]
	if !ok || route.Harness != order.RequiredHarness || cfg.EffectiveModel(string(order.Stage)) != order.RequiredModel {
		return false
	}
	harness, found := harnessForOrder(cfg, core.WorkOrder{Stage: order.Stage, RequiredHarness: route.Harness})
	if !found {
		return false
	}
	snapshot := &core.HarnessSnapshot{
		Name: harness.Name, MCPTransport: harness.MCPTransport, MCPAttachment: harness.MCPAttachment, Command: harness.Command, ModelArgs: harness.ModelArgs,
		DefaultModelSentinels: harness.DefaultModelSentinels,
		EffortArgs:            harness.EffortArgs, Effort: route.Effort,
		ProbeCommand: harness.ProbeCommand, ProbeTimeoutText: harness.ProbeTimeoutText,
		StallTimeoutText: harness.StallTimeoutText,
	}
	if route.Effort != "" {
		snapshot.EffortArgv = append([]string(nil), harness.EffortArgs[route.Effort]...)
	}
	return reflect.DeepEqual(snapshot, order.RequiredHarnessConfig)
}

func probeHealthy(probes []core.HarnessProbe, name, fingerprint string, allowLegacy bool) bool {
	legacyHealthy := false
	for _, probe := range probes {
		if probe.Harness != name {
			continue
		}
		if probe.Fingerprint == fingerprint {
			return probe.Healthy
		}
		if probe.Fingerprint == "" {
			legacyHealthy = legacyHealthy || probe.Healthy
		}
	}
	return allowLegacy && legacyHealthy
}

func reviewOrderMatchesCurrentConfig(cfg *config.Config, order core.WorkOrder) bool {
	route, ok := cfg.Routing.Stages["review"]
	if !ok {
		return false
	}
	seats := cfg.Review.Seats
	if len(seats) == 0 {
		seats = []config.ReviewSeat{{Model: route.Model, Harness: route.Harness}}
	}
	if order.ReviewSeat < 1 || order.ReviewSeat > len(seats) {
		return false
	}
	seat := seats[order.ReviewSeat-1]
	harnessName := seat.Harness
	if harnessName == "" {
		harnessName = route.Harness
	}
	if seat.Model != order.RequiredModel || harnessName != order.RequiredHarness || seat.Effort != order.RequiredEffort {
		return false
	}
	harness, found := harnessForOrder(cfg, core.WorkOrder{Stage: core.StageReview, RequiredHarness: harnessName})
	if !found {
		return false
	}
	snapshot := &core.HarnessSnapshot{
		Name: harness.Name, MCPTransport: harness.MCPTransport, MCPAttachment: harness.MCPAttachment, Command: harness.Command, ModelArgs: harness.ModelArgs,
		DefaultModelSentinels: harness.DefaultModelSentinels,
		EffortArgs:            harness.EffortArgs, Effort: seat.Effort,
		ProbeCommand: harness.ProbeCommand, ProbeTimeoutText: harness.ProbeTimeoutText,
		StallTimeoutText: harness.StallTimeoutText,
	}
	return reflect.DeepEqual(snapshot, order.RequiredHarnessConfig)
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

func (s *Service) Renew(ctx context.Context, worker core.Worker, id, sessionID string) (core.WorkOrder, error) {
	if strings.TrimSpace(sessionID) == "" {
		return core.WorkOrder{}, fmt.Errorf("session_id is required")
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, s.Store, order.TaskID, core.WorkOrderCmdRenew, func(taskLease taskops.TaskLease) (core.WorkOrder, error) {
		return s.Store.RenewWorkerClaimCommand(ctx, taskLease, id, worker.ID, sessionID, DefaultClaimLease)
	})
}

func (s *Service) Reconcile(ctx context.Context, worker core.Worker, id, sessionID string) (ClaimReconciliation, error) {
	if strings.TrimSpace(sessionID) == "" {
		return ClaimReconciliation{}, fmt.Errorf("session_id is required")
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return ClaimReconciliation{}, err
	}
	authorized := order.WorkerID == worker.ID && order.SessionID == sessionID &&
		order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(s.now())
	reason := "session is no longer the active lease owner"
	if authorized {
		reason = "session owns the active claim"
	} else if order.State == core.WorkOrderSubmitted || order.State == core.WorkOrderCompleted {
		reason = "work order already reached a durable terminal handoff"
	}
	return ClaimReconciliation{WorkOrder: order, Authorized: authorized, Reason: reason}, nil
}

func (s *Service) Release(ctx context.Context, worker core.Worker, id string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	if strings.TrimSpace(release.SessionID) == "" {
		return core.WorkOrder{}, fmt.Errorf("session_id is required")
	}
	release.Reason = strings.TrimSpace(release.Reason)
	release.FailureDetail = boundedFailureDetail(release.FailureDetail)
	release.ModelRejection = providerModelRejection(release.FailureDetail)
	if release.Outcome == "" {
		release.Outcome = core.WorkOrderOutcomeReleased
	}
	if release.Outcome != core.WorkOrderOutcomeChildFailure && release.Outcome != core.WorkOrderOutcomeStalled && release.Outcome != core.WorkOrderOutcomeReleased && release.Outcome != core.WorkOrderOutcomeCancelled {
		return core.WorkOrder{}, fmt.Errorf("invalid worker release outcome %q", release.Outcome)
	}
	if release.Outcome == core.WorkOrderOutcomeChildFailure && release.FailureCategory == "" && providerUsageLimit(release.FailureDetail) {
		release.FailureCategory = core.WorkOrderFailureProviderUsageLimit
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
		return s.Store.ReleaseWorkerClaimCommand(ctx, taskLease, id, worker.ID, release)
	})
	if err != nil {
		return core.WorkOrder{}, err
	}
	return s.refreshReleasedHarnessSnapshot(ctx, order), nil
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
	return s.Store.RecordWorkOrderAttemptCheckpoint(ctx, id, worker.ID, checkpoint)
}

const FailureDetailLimit = 2 * 1024

func boundedFailureDetail(detail string) string {
	detail = strings.ToValidUTF8(detail, "�")
	if len(detail) > FailureDetailLimit {
		detail = detail[len(detail)-FailureDetailLimit:]
		detail = strings.ToValidUTF8(detail, "�")
	}
	return strings.TrimSpace(detail)
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
// operator's current definition (spec §21.32). Best-effort: the release above
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
