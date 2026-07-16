// Package worker implements the enrolled Phase 5.1 dispatch supervisor
// control surface. It selects only Auto-mode orders and reuses the existing
// MCP work-order lifecycle rather than creating a parallel task protocol.
package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	HarnessSelection string         `json:"harness_selection"`
	Dispatch         string         `json:"dispatch"`
	Confinement      string         `json:"confinement"`
	Auth             string         `json:"auth"`
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
	registered := map[string]bool{}
	for _, harness := range cfg.Harnesses {
		registered[harness.Name] = true
	}
	now := s.now()
	for i := range probes {
		if !registered[probes[i].Harness] {
			return core.Worker{}, fmt.Errorf("unknown harness probe %q", probes[i].Harness)
		}
		if probes[i].CheckedAt.IsZero() {
			probes[i].CheckedAt = now
		}
	}
	return s.Store.HeartbeatWorker(ctx, worker.ID, now.Add(DefaultLivenessLease), probes)
}

func (s *Service) AutoAvailable(ctx context.Context, cfg *config.Config) (bool, string) {
	workers, err := s.Store.ListWorkers(ctx)
	if err != nil {
		return false, err.Error()
	}
	required, err := requiredHarnesses(cfg)
	if err != nil {
		return false, err.Error()
	}
	if len(required) == 0 {
		return false, "no routed worker harness is configured"
	}
	now := s.now()
	for _, worker := range workers {
		if !worker.Live(now) {
			continue
		}
		health := map[string]bool{}
		for _, probe := range worker.Probes {
			health[probe.Harness] = probe.Healthy
		}
		all := true
		for name := range required {
			if !health[name] {
				all = false
				break
			}
		}
		if all {
			return true, ""
		}
	}
	return false, "no live worker reports every routed harness healthy"
}

func requiredHarnesses(cfg *config.Config) (map[string]bool, error) {
	result := map[string]bool{}
	for _, stage := range []string{"implement", "review"} {
		route := cfg.Routing.Stages[stage]
		if stage == "review" && route.Execution == config.ExecutionInProcess {
			continue
		}
		if route.Harness == "" {
			return nil, fmt.Errorf("%s route has no harness", stage)
		}
		result[route.Harness] = true
	}
	return result, nil
}

func (s *Service) ListAuto(ctx context.Context) ([]DispatchOrder, error) {
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
		harness, ok := harnessFor(cfg, order.Stage)
		if !ok {
			continue
		}
		result = append(result, DispatchOrder{Order: order, Task: task, Harness: harness, Model: cfg.Routing.Stages[string(order.Stage)].Model, HarnessSelection: "enforced", Dispatch: "worker", Confinement: "none", Auth: "byoa"})
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
	if !worker.Live(s.now()) {
		return core.WorkOrder{}, fmt.Errorf("worker liveness lease expired")
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if task.Mode != core.TaskModeAuto {
		return core.WorkOrder{}, fmt.Errorf("worker may claim Auto tasks only")
	}
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	harness, ok := harnessFor(cfg, order.Stage)
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("no harness configured for %s", order.Stage)
	}
	probeHealthy := false
	for _, probe := range worker.Probes {
		if probe.Harness == harness.Name && probe.Healthy {
			probeHealthy = true
		}
	}
	if !probeHealthy {
		return core.WorkOrder{}, fmt.Errorf("routed harness %s is unhealthy", harness.Name)
	}
	claim.WorkerID = worker.ID
	claim.ClaimantID = worker.ID
	claim.Agent = harness.Name
	claim.Model = cfg.Routing.Stages[string(order.Stage)].Model
	if claim.Lease <= 0 {
		claim.Lease = DefaultClaimLease
	}
	claimed, err := s.WorkOrders.Claim(ctx, id, claim)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if job, exists, getErr := s.Store.GetLatestJob(ctx, task.ID); getErr == nil && exists && job.ID == claimed.JobID {
		job.Harness = harness.Name
		job.ModelTier = claim.Model
		job.AuthMode = "byoa"
		job.Runner = "worker"
		job.Confinement = "none"
		_ = s.Store.UpdateJob(ctx, job)
	}
	return claimed, nil
}

func harnessFor(cfg *config.Config, stage core.Stage) (config.Harness, bool) {
	route, ok := cfg.Routing.Stages[string(stage)]
	if !ok || route.Harness == "" {
		return config.Harness{}, false
	}
	for _, harness := range cfg.Harnesses {
		if harness.Name == route.Harness {
			return harness, true
		}
	}
	return config.Harness{}, false
}

func (s *Service) Renew(ctx context.Context, worker core.Worker, id string) (core.WorkOrder, error) {
	return s.Store.RenewWorkerClaim(ctx, id, worker.ID, DefaultClaimLease)
}
func (s *Service) Release(ctx context.Context, worker core.Worker, id, reason string) (core.WorkOrder, error) {
	return s.Store.ReleaseWorkerClaim(ctx, id, worker.ID, strings.TrimSpace(reason))
}
