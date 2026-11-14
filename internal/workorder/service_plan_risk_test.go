package workorder

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// These tests prove RR-13 of feature-risk-based-review at the submit_plan
// service boundary and the plan approval gate: every plan is accepted with or
// without a tier line; a submitted version gets one risk order that the
// result names; under enforce the approval waits for that order; a risk agent
// rating counts once its version is approved, in either order; and no plan
// text records a rating (req-risk-based-review AC-4.3, AC-4.7 to AC-4.9,
// AC-12.6, AC-1.6).

func riskPlanMarkdown(line string) string {
	if line == "" {
		return planSeatsMarkdown
	}
	return strings.Replace(planSeatsMarkdown, "## Risks\n- Drift.", "## Risks\n- Drift.\n\n"+line, 1)
}

func frozenRisk(mode riskpolicy.Mode) config.PolicyContract {
	return config.PolicyContract{Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
}

func (e *planSeatsEnv) submitMarkdown(order core.WorkOrder, markdown string) (map[string]any, error) {
	return e.service.SubmitPlan(e.ctx, order.ID, order.SessionID, pipeline.StructuredPlan{Markdown: markdown, Decomposition: []pipeline.DecompositionItem{}})
}

func (e *planSeatsEnv) riskRatings(t *testing.T) []core.RiskRating {
	t.Helper()
	ratings, err := e.st.ListTaskRiskRatings(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	return ratings
}

// planRiskOrder returns the risk order of plan version, failing when there
// is none.
func (e *planSeatsEnv) planRiskOrder(t *testing.T, version int) core.RiskOrder {
	t.Helper()
	orders, err := e.st.ListTaskRiskOrders(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range orders {
		if order.Subject.PlanVersion == version {
			return order
		}
	}
	t.Fatalf("plan version %d has no risk order among %+v", version, orders)
	return core.RiskOrder{}
}

// rate claims a plan risk order under a fresh session and completes it with
// tier, as an independent risk agent does.
func (e *planSeatsEnv) rate(t *testing.T, orderID string, tier riskpolicy.Tier) core.RiskRating {
	t.Helper()
	e.claims++
	session := fmt.Sprintf("risk-session-%d", e.claims)
	if _, err := storetest.For(e.st).ClaimWorkOrder(e.ctx, orderID, core.WorkOrderClaim{ClaimantID: "run:risk-agent", SessionID: session, ClientToken: session + "-token",
		Agent: "claude-code", Model: "risk-model", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	rating, err := taskops.ExecuteWorkOrder(e.ctx, e.st, e.taskID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
		return e.st.CompleteRiskOrderCommand(e.ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: orderID,
			Claim: core.WorkOrderClaimIdentity{ClaimantID: "run:risk-agent", SessionID: session}, Tier: tier, Reason: "Rated from the plan."})
	})
	if err != nil {
		t.Fatal(err)
	}
	return rating
}

// tier returns the task's counted tier, or empty when it has none.
func (e *planSeatsEnv) tier(t *testing.T) riskpolicy.Tier {
	t.Helper()
	if setter, ok := core.RiskTierOf(e.riskRatings(t)); ok {
		return setter.Tier
	}
	return ""
}

// tryApprove applies the plan approval through the real gate path and
// returns its error, recording the intervention only when it succeeds, as
// the review route does.
func (e *planSeatsEnv) tryApprove(t *testing.T) error {
	t.Helper()
	current, err := e.st.GetTask(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	latest, _, err := e.st.GetLatestJob(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	decision := core.Intervention{TaskID: e.taskID, JobID: latest.ID, Action: core.InterventionApprove, ReasonCode: "plan-approved"}
	if err = e.d.HandleIntervention(e.ctx, current, latest, decision); err != nil {
		return err
	}
	if err = e.st.CreateIntervention(e.ctx, decision); err != nil {
		t.Fatal(err)
	}
	return e.d.DispatchNow(e.ctx, e.taskID)
}

// TestSubmitPlanRiskOrderUnderEnforce shows a plan submitted under enforce,
// with a malformed tier line kept as text, gets a risk order that the result
// names; approval waits for it and changes nothing; the completed rating
// counts only once the version is approved; and a later version rated lower
// never lowers the tier.
func TestSubmitPlanRiskOrderUnderEnforce(t *testing.T) {
	env := newPlanSeatsEnv(t, "plan-risk-enforce", true, frozenRisk(riskpolicy.ModeEnforce), 1)
	result, err := env.submitMarkdown(env.claimPlan(t), riskPlanMarkdown("Risk tier: High — One.\n\n- Risk tier: low — Two."))
	if err != nil {
		t.Fatalf("plan with tier text refused: %v", err)
	}
	env.requireStored(t, result, 1, nil, false)
	first := env.planRiskOrder(t, 1)
	if result["risk_work_order_id"] != first.Order.ID || first.Order.State != core.WorkOrderQueued {
		t.Fatalf("submit_plan result=%v, risk order=%+v", result, first)
	}
	if ratings := env.riskRatings(t); len(ratings) != 0 {
		t.Fatalf("plan text recorded ratings %+v", ratings)
	}

	var refusal *store.PlanRiskRatingOpenError
	if err = env.tryApprove(t); !errors.As(err, &refusal) || refusal.WorkOrderID != first.Order.ID || !errors.Is(err, store.ErrPlanRiskRatingOpen) {
		t.Fatalf("approval with an open risk order: %v", err)
	}
	task, err := env.st.GetTask(env.ctx, env.taskID)
	if err != nil || task.PlanState != core.PlanAwaitingApproval || task.GateKind != core.GateKindPlanApproval {
		t.Fatalf("refused approval moved the task: %+v err=%v", task, err)
	}
	if _, approved, _ := env.st.GetApprovedSpecVersion(env.ctx, env.taskID); approved {
		t.Fatal("refused approval approved the plan")
	}

	// Rating before approval: the rating is recorded but counts only once
	// the version is approved (AC-4.3).
	rating := env.rate(t, first.Order.ID, riskpolicy.TierHigh)
	if rating.Source != core.RiskSourceRiskAgent || rating.PlanVersion != 1 || !rating.PlanPending {
		t.Fatalf("rating=%+v, want a pending risk agent rating of plan version 1", rating)
	}
	if tier := env.tier(t); tier != "" {
		t.Fatalf("an unapproved plan set the tier %s", tier)
	}
	if err = env.tryApprove(t); err != nil {
		t.Fatalf("approval after the rating: %v", err)
	}
	if tier := env.tier(t); tier != riskpolicy.TierHigh {
		t.Fatalf("tier after approval=%q, want high", tier)
	}

	// A later version at high tier gets no risk order (AC-12.3), and the
	// earlier approved rating stays the minimum.
	revision := env.reenterPlan(t)
	result, err = env.submitMarkdown(revision, riskPlanMarkdown(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, named := result["risk_work_order_id"]; named {
		t.Fatalf("a task at high named a risk order: %v", result)
	}
	if err = env.tryApprove(t); err != nil {
		t.Fatalf("approval with no risk order: %v", err)
	}
	if tier := env.tier(t); tier != riskpolicy.TierHigh {
		t.Fatalf("tier after a later plan=%q, want high", tier)
	}
}

// TestSubmitPlanRiskOrderLowerLaterVersion shows that a later version rated
// lower never lowers the minimum an earlier approved version set, and that
// each submission names its own risk order.
func TestSubmitPlanRiskOrderLowerLaterVersion(t *testing.T) {
	env := newPlanSeatsEnv(t, "plan-risk-lower", true, frozenRisk(riskpolicy.ModeEnforce), 1)
	if _, err := env.submitMarkdown(env.claimPlan(t), riskPlanMarkdown("")); err != nil {
		t.Fatal(err)
	}
	env.rate(t, env.planRiskOrder(t, 1).Order.ID, riskpolicy.TierMedium)
	if err := env.tryApprove(t); err != nil {
		t.Fatal(err)
	}
	result, err := env.submitMarkdown(env.reenterPlan(t), riskPlanMarkdown(""))
	if err != nil {
		t.Fatal(err)
	}
	second := env.planRiskOrder(t, 2)
	if result["risk_work_order_id"] != second.Order.ID {
		t.Fatalf("second submission result=%v, want %s", result, second.Order.ID)
	}
	env.rate(t, second.Order.ID, riskpolicy.TierLow)
	if err = env.tryApprove(t); err != nil {
		t.Fatal(err)
	}
	if tier := env.tier(t); tier != riskpolicy.TierMedium {
		t.Fatalf("tier=%q, want medium from the earlier approved version", tier)
	}
}

// TestSubmitPlanRiskOrderUnderRecordAndOff shows record approval never waits
// and leaves its risk order open, a rating completed after approval counts,
// a later version cancels an earlier version's open order, and a task whose
// risk mode is off gets no order and no rating.
func TestSubmitPlanRiskOrderUnderRecordAndOff(t *testing.T) {
	record := newPlanSeatsEnv(t, "plan-risk-record", true, frozenRisk(riskpolicy.ModeRecord), 1)
	result, err := record.submitMarkdown(record.claimPlan(t), riskPlanMarkdown("Risk tier: high — Kept as plan text."))
	if err != nil {
		t.Fatalf("record refused a plan: %v", err)
	}
	first := record.planRiskOrder(t, 1)
	if result["risk_work_order_id"] != first.Order.ID {
		t.Fatalf("submit_plan result=%v", result)
	}
	if err = record.tryApprove(t); err != nil {
		t.Fatalf("record approval waited: %v", err)
	}
	if order := record.planRiskOrder(t, 1); order.Order.State != core.WorkOrderQueued {
		t.Fatalf("record approval ended the risk order: %+v", order)
	}
	if ratings := record.riskRatings(t); len(ratings) != 0 {
		t.Fatalf("plan text recorded ratings %+v", ratings)
	}
	// Approval before rating: the rating counts once both exist (AC-4.3).
	record.rate(t, first.Order.ID, riskpolicy.TierMedium)
	if tier := record.tier(t); tier != riskpolicy.TierMedium {
		t.Fatalf("tier after a rating of an approved version=%q, want medium", tier)
	}

	// Version 2 is approved with its order still open; version 3 replaces
	// it and cancels that order (AC-4.9).
	if _, err = record.submitMarkdown(record.reenterPlan(t), riskPlanMarkdown("")); err != nil {
		t.Fatal(err)
	}
	if err = record.tryApprove(t); err != nil {
		t.Fatal(err)
	}
	if _, err = record.submitMarkdown(record.reenterPlan(t), riskPlanMarkdown("")); err != nil {
		t.Fatal(err)
	}
	second, err := record.st.GetWorkOrder(record.ctx, record.planRiskOrder(t, 2).Order.ID)
	if err != nil || second.State != core.WorkOrderCancelled || second.RetrySuppressionReason != store.PlanRiskReplacedReason {
		t.Fatalf("replaced version's risk order=%+v err=%v", second, err)
	}
	if third := record.planRiskOrder(t, 3); third.Order.State != core.WorkOrderQueued {
		t.Fatalf("version 3 risk order=%+v", third)
	}

	off := newPlanSeatsEnv(t, "plan-risk-off", true, config.PolicyContract{}, 1)
	result, err = off.submitMarkdown(off.claimPlan(t), riskPlanMarkdown("Risk tier: critical — Not a tier."))
	if err != nil {
		t.Fatal(err)
	}
	if _, named := result["risk_work_order_id"]; named {
		t.Fatalf("off task named a risk order: %v", result)
	}
	if orders, err := off.st.ListTaskRiskOrders(off.ctx, off.taskID); err != nil || len(orders) != 0 {
		t.Fatalf("off task risk orders=%+v err=%v", orders, err)
	}
	if err = off.tryApprove(t); err != nil {
		t.Fatal(err)
	}
	if ratings := off.riskRatings(t); len(ratings) != 0 {
		t.Fatalf("off task ratings=%+v", ratings)
	}
}
