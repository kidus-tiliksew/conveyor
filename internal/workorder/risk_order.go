package workorder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RiskContract is what a claimed risk order delivers beside the task: the
// subject it rates, the plan version for a plan subject, the changed paths
// and matched path rules stored for a head subject, and the task's frozen
// risk mode and tier table (req-risk-based-review AC-12.5;
// feature-risk-based-review RR-11). A head subject's comparison and diff
// arrive in the context's review_comparison and diff, read by the review
// order's reader.
type RiskContract struct {
	Subject      core.RiskOrderSubject  `json:"subject"`
	Plan         *core.SpecVersion      `json:"plan,omitempty"`
	ChangedPaths []string               `json:"changed_paths"`
	MatchedRules []riskpolicy.RuleMatch `json:"matched_rules"`
	Mode         riskpolicy.Mode        `json:"mode"`
	Tiers        riskpolicy.Table       `json:"tiers"`
}

// ErrRiskOrderSubjectUnavailable refuses a risk order contract whose subject
// cannot be read in full: the stored subject, the plan version, or the head's
// comparison. The contract never falls back to another subject.
var ErrRiskOrderSubjectUnavailable = errors.New("risk order subject unavailable")

// riskMaterialForOrder reads the exact subject that the risk order stored. A
// plan subject reads that immutable plan version, never the latest approved
// plan. A head subject reads the stored base and head through the review
// order's comparison and diff reader, never a comparison derived from the
// task's current events.
func (s *Service) riskMaterialForOrder(ctx context.Context, order core.WorkOrder, task core.Task) (*RiskContract, reviewMaterial, error) {
	policy := task.SetupContract.Risk
	if !policy.Enabled() {
		return nil, reviewMaterial{}, fmt.Errorf("%w: task %s has no frozen risk policy", ErrRiskOrderSubjectUnavailable, task.ID)
	}
	orders, err := s.Store.ListTaskRiskOrders(ctx, task.ID)
	if err != nil {
		return nil, reviewMaterial{}, err
	}
	var risk *core.RiskOrder
	for i := range orders {
		if orders[i].Order.ID == order.ID {
			risk = &orders[i]
			break
		}
	}
	if risk == nil {
		return nil, reviewMaterial{}, fmt.Errorf("%w: risk order %s has no stored subject", ErrRiskOrderSubjectUnavailable, order.ID)
	}
	contract := &RiskContract{Subject: risk.Subject, ChangedPaths: risk.ChangedPaths, MatchedRules: risk.MatchedRules, Mode: policy.Mode, Tiers: policy.Tiers}
	if contract.ChangedPaths == nil {
		contract.ChangedPaths = []string{}
	}
	if contract.MatchedRules == nil {
		contract.MatchedRules = []riskpolicy.RuleMatch{}
	}
	switch risk.Subject.Kind() {
	case riskpolicy.RiskSubjectPlan:
		plan, ok, getErr := s.Store.GetSpecVersion(ctx, task.ID, risk.Subject.PlanVersion)
		if getErr != nil {
			return nil, reviewMaterial{}, getErr
		}
		if !ok {
			return nil, reviewMaterial{}, fmt.Errorf("%w: plan version %d of task %s is not recorded", ErrRiskOrderSubjectUnavailable, risk.Subject.PlanVersion, task.ID)
		}
		contract.Plan = &plan
		return contract, reviewMaterial{}, nil
	case riskpolicy.RiskSubjectHead:
		cfg, cfgErr := s.config(ctx)
		if cfgErr != nil {
			return nil, reviewMaterial{}, cfgErr
		}
		repo, ok := cfg.Repo(task.Repo)
		if !ok || repo.GitHub == "" {
			return nil, reviewMaterial{}, fmt.Errorf("%w: repository %s has no GitHub repository to compare head %s with base %s", ErrRiskOrderSubjectUnavailable, task.Repo, risk.Subject.HeadSHA, risk.Subject.BaseSHA)
		}
		comparison := dispatch.ReviewComparison{Source: dispatch.ReviewComparisonSourceGitHub, BaselineSHA: risk.Subject.BaseSHA, HeadSHA: risk.Subject.HeadSHA, Scope: config.RefreshReviewFull}
		material, materialErr := s.reviewMaterialFor(ctx, repo.GitHub, task.Branch, comparison)
		if materialErr != nil {
			return nil, reviewMaterial{}, materialErr
		}
		return contract, material, nil
	}
	return nil, reviewMaterial{}, fmt.Errorf("%w: risk order %s names no valid subject", ErrRiskOrderSubjectUnavailable, order.ID)
}

// SubmitRiskRating completes a risk order with a tier and a one-line reason
// (req-risk-based-review AC-12.2; feature-risk-based-review RR-11). claim is
// the authenticated claimant's identity for the presented session, never a
// value the caller supplies. The store command reads the subject, the paths,
// the rules, and the model from the stored order and its claim, refuses a
// missing tier or reason and a lost or expired claim with the order still
// claimed, and returns the recorded rating to a replay by the completing
// session.
func (s *Service) SubmitRiskRating(ctx context.Context, claim core.WorkOrderClaimIdentity, id, tier, reason string) (map[string]any, error) {
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return nil, err
	}
	if order.Stage != core.StageRisk {
		return nil, fmt.Errorf("%w: %s", store.ErrNotRiskOrder, id)
	}
	// The refusals keep the wording every claim-bound submission uses
	// (component-work-orders). A completed order goes on to the store, which
	// returns the recorded rating only to the session that completed it.
	switch order.State {
	case core.WorkOrderTimedOut:
		return nil, store.ErrWorkOrderTimedOut
	case core.WorkOrderStale:
		return nil, store.ErrWorkOrderStale
	case core.WorkOrderCancelled:
		return nil, store.ErrWorkOrderCancelled
	case core.WorkOrderClaimed, core.WorkOrderCompleted:
	default:
		return nil, store.Refusef("work order %s is not claimed", id)
	}
	if strings.TrimSpace(claim.SessionID) == "" || order.SessionID != claim.SessionID {
		return nil, store.Refusef("work order %s is claimed by another session", id)
	}
	if order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.After(time.Now()) {
		return nil, store.Refusef("work order lease expired")
	}
	rating, err := taskops.ExecuteWorkOrder(ctx, s.Store, order.TaskID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
		return s.Store.CompleteRiskOrderCommand(ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: id, Claim: claim, Tier: riskpolicy.Tier(tier), Reason: reason})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"work_order_id": id, "task_id": order.TaskID, "state": core.WorkOrderCompleted, "rating": rating}, nil
}

// projectRiskSubjects names each risk order's subject on the work-order list
// from the task's stored risk orders, one read per task that has one
// (req-risk-based-review AC-11.2).
func (s *Service) projectRiskSubjects(ctx context.Context, orders []core.WorkOrder) error {
	subjects := map[string]core.RiskOrderSubject{}
	read := map[string]bool{}
	for _, order := range orders {
		if order.Stage != core.StageRisk || read[order.TaskID] {
			continue
		}
		read[order.TaskID] = true
		risks, err := s.Store.ListTaskRiskOrders(ctx, order.TaskID)
		if err != nil {
			return err
		}
		for _, risk := range risks {
			subjects[risk.Order.ID] = risk.Subject
		}
	}
	for i := range orders {
		if subject, ok := subjects[orders[i].ID]; ok && orders[i].Stage == core.StageRisk {
			orders[i].RiskSubject = &subject
		}
	}
	return nil
}
