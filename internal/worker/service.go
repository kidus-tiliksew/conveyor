// Package worker implements the enrolled Phase 5.1 dispatch supervisor
// control surface. It selects only Auto-mode orders and reuses the existing
// MCP work-order lifecycle rather than creating a parallel task protocol.
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
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	DefaultPairingTTL    = 10 * time.Minute
	DefaultLivenessLease = 15 * time.Second
	DefaultClaimLease    = 30 * time.Second
)

type Service struct {
	Store          store.Store
	WorkOrders     *workorder.Service
	ConfigProvider func(context.Context) (*config.Config, error)
	Now            func() time.Time
}

type Enrollment struct {
	Worker     core.Worker `json:"worker"`
	Credential string      `json:"credential"`
}

type DispatchOrder struct {
	Order            core.WorkOrder `json:"work_order"`
	Task             core.Task      `json:"task"`
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
		Command               []string            `json:"command"`
		ModelArgs             []string            `json:"model_args"`
		DefaultModelSentinels []string            `json:"default_model_sentinels"`
		EffortArgs            map[string][]string `json:"effort_args"`
		ProbeCommand          []string            `json:"probe_command"`
		ProbeTimeout          string              `json:"probe_timeout"`
	}{
		Name: harness.Name, MCPTransport: harness.MCPTransport, Command: canonicalArgs(harness.Command),
		ModelArgs: canonicalArgs(harness.ModelArgs), DefaultModelSentinels: canonicalArgs(harness.DefaultModelSentinels),
		EffortArgs: harness.EffortArgs, ProbeCommand: canonicalArgs(harness.ProbeCommand), ProbeTimeout: harness.ProbeTimeoutText,
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
		workerDispatched := order.Stage == core.StageImplement || order.Stage == core.StageReview
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

func (s *Service) AutoAvailable(ctx context.Context, cfg *config.Config) (bool, string) {
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
	for _, stage := range []string{"implement"} {
		route := cfg.Routing.Stages[stage]
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

func (s *Service) ListAuto(ctx context.Context, worker core.Worker) ([]DispatchOrder, error) {
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
		if getErr != nil || task.Mode != core.TaskModeAuto {
			continue
		}
		if healthy, _ := s.workerHealthyForOrder(worker, cfg, order); !healthy {
			continue
		}
		harness, ok := harnessForOrder(cfg, order)
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
		result = append(result, DispatchOrder{Order: order, Task: task, Harness: harness, Model: model, Effort: order.RequiredEffort, EffortArgv: effortArgv, HarnessSelection: "enforced", Dispatch: "worker", Confinement: "none", Auth: "byoa"})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Order.Stage != result[j].Order.Stage {
			return result[i].Order.Stage == core.StageReview
		}
		return result[i].Order.CreatedAt.Before(result[j].Order.CreatedAt)
	})
	return result, nil
}

func (s *Service) ClaimAuto(ctx context.Context, worker core.Worker, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if healthy, reason := s.workerHealthyForOrder(worker, cfg, order); !healthy {
		return core.WorkOrder{}, fmt.Errorf("auto unavailable: %s", reason)
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if task.Mode != core.TaskModeAuto {
		return core.WorkOrder{}, fmt.Errorf("worker may claim Auto tasks only")
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
	transport := snapshot.MCPTransport
	if transport == "" {
		transport = config.MCPTransportJSONFile
	}
	return config.Harness{
		Name: snapshot.Name, MCPTransport: transport, Command: append([]string(nil), snapshot.Command...),
		ModelArgs:             append([]string(nil), snapshot.ModelArgs...),
		DefaultModelSentinels: append([]string(nil), snapshot.DefaultModelSentinels...),
		EffortArgs:            cloneEffortArgs(snapshot.EffortArgs),
		ProbeCommand:          append([]string(nil), snapshot.ProbeCommand...),
		ProbeTimeoutText:      snapshot.ProbeTimeoutText, ProbeTimeout: probeTimeout,
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
		Name: harness.Name, MCPTransport: harness.MCPTransport, Command: harness.Command, ModelArgs: harness.ModelArgs,
		DefaultModelSentinels: harness.DefaultModelSentinels,
		EffortArgs:            harness.EffortArgs, Effort: route.Effort,
		ProbeCommand: harness.ProbeCommand, ProbeTimeoutText: harness.ProbeTimeoutText,
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
		Name: harness.Name, MCPTransport: harness.MCPTransport, Command: harness.Command, ModelArgs: harness.ModelArgs,
		DefaultModelSentinels: harness.DefaultModelSentinels,
		EffortArgs:            harness.EffortArgs, Effort: seat.Effort,
		ProbeCommand: harness.ProbeCommand, ProbeTimeoutText: harness.ProbeTimeoutText,
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

func (s *Service) Renew(ctx context.Context, worker core.Worker, id string) (core.WorkOrder, error) {
	return s.Store.RenewWorkerClaim(ctx, id, worker.ID, DefaultClaimLease)
}
func (s *Service) Release(ctx context.Context, worker core.Worker, id, reason string) (core.WorkOrder, error) {
	return s.Store.ReleaseWorkerClaim(ctx, id, worker.ID, strings.TrimSpace(reason))
}
