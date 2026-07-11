// Package routing selects harness capacity from the credential pool while
// respecting owner isolation, vendor policy, cooldowns, and leases (spec §5.2,
// §5.3).
package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

var ErrNoCapacity = errors.New("no eligible harness credential capacity")

const RateLimitCooldown = 5 * time.Minute

type ClaimRequest struct {
	TaskID          string
	JobID           string
	OwnerID         string
	Harnesses       []string
	LeaseSeconds    int64
	AllowRestricted bool
}

type Credential struct {
	ID        string
	OwnerID   string
	OwnerKind string
	Kind      string
	Vendor    string
	Harness   string
	Ref       string
}

type Pool interface {
	RescueTaskCredentialLeases(context.Context, string, string) error
	ClaimCredential(context.Context, ClaimRequest) (Credential, error)
	ReleaseCredential(context.Context, string, string, string) error
	ThrottleCredential(context.Context, string, string, string, int64) error
}

type Selection struct {
	Credential
	JobID     string
	ModelTier string
	BudgetUSD float64
}

type Outcome struct {
	RateLimited bool
	Error       string
}

type Selector interface {
	Select(context.Context, string, string, core.Stage) (Selection, error)
	Complete(context.Context, Selection, Outcome) error
}

type Router struct {
	pool Pool
	cfg  config.Routing
}

func New(pool Pool, cfg config.Routing) *Router { return &Router{pool: pool, cfg: cfg} }

type StaticRouter struct {
	credential Credential
	cfg        config.Routing
}

// NewStatic keeps the explicit memory compatibility backend on the same
// dispatch contract as Postgres. It does not lease capacity, but it still
// records harness, credential, auth mode, model tier, and budget on every job.
func NewStatic(credential Credential, cfg config.Routing) *StaticRouter {
	return &StaticRouter{credential: credential, cfg: cfg}
}

func (r *StaticRouter) Select(_ context.Context, _, jobID string, stage core.Stage) (Selection, error) {
	policy := stageRoute(r.cfg, stage)
	for _, harness := range policy.Harnesses {
		if harness == r.credential.Harness {
			return Selection{
				Credential: r.credential, JobID: jobID,
				ModelTier: policy.ModelTier, BudgetUSD: policy.BudgetUSD,
			}, nil
		}
	}
	return Selection{}, fmt.Errorf("route %s: static credential %s is not eligible", stage, r.credential.Harness)
}

func (*StaticRouter) Complete(context.Context, Selection, Outcome) error { return nil }

func (r *Router) Select(ctx context.Context, taskID, jobID string, stage core.Stage) (Selection, error) {
	if err := r.pool.RescueTaskCredentialLeases(ctx, taskID, jobID); err != nil {
		return Selection{}, fmt.Errorf("rescue task credential lease: %w", err)
	}
	policy := stageRoute(r.cfg, stage)
	credential, err := r.pool.ClaimCredential(ctx, ClaimRequest{
		TaskID: taskID, JobID: jobID, OwnerID: r.cfg.OwnerID, Harnesses: policy.Harnesses,
		LeaseSeconds: int64(r.cfg.LeaseSeconds), AllowRestricted: r.cfg.AllowRestricted,
	})
	if err != nil {
		if errors.Is(err, ErrNoCapacity) {
			return Selection{}, fmt.Errorf("route %s: %w", stage, err)
		}
		return Selection{}, err
	}
	return Selection{Credential: credential, JobID: jobID, ModelTier: policy.ModelTier, BudgetUSD: policy.BudgetUSD}, nil
}

func stageRoute(cfg config.Routing, stage core.Stage) config.StageRoute {
	policy, ok := cfg.Stages[string(stage)]
	if !ok {
		policy = config.StageRoute{Harnesses: []string{"codex", "claude-code"}}
		if stage == core.StageImplement {
			policy.BudgetUSD = 3
		}
	}
	return policy
}

func (r *Router) Complete(ctx context.Context, selection Selection, outcome Outcome) error {
	if outcome.RateLimited {
		return r.pool.ThrottleCredential(ctx, selection.ID, selection.JobID, outcome.Error, int64(RateLimitCooldown/time.Second))
	}
	return r.pool.ReleaseCredential(ctx, selection.ID, selection.JobID, outcome.Error)
}
