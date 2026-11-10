package workorder

import (
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// These tests prove RR-4 of feature-risk-based-review at the submit_plan
// service boundary and the plan approval gate: under a frozen enforce mode a
// plan with no tier is refused with the plan order left claimed and nothing
// written, a malformed tier line is refused in every mode, and each approved
// plan version's tier becomes a rating that a later lower plan cannot lower
// (req-risk-based-review AC-4.1, AC-4.3, AC-4.5, AC-4.6, AC-2.2).

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

// TestSubmitPlanRiskTierUnderEnforce shows submit_plan refused under enforce
// with the order still claimed, then accepted on the same claim once the plan
// states a tier, and the tier recorded only when the operator approves it.
func TestSubmitPlanRiskTierUnderEnforce(t *testing.T) {
	env := newPlanSeatsEnv(t, "plan-risk-enforce", true, frozenRisk(riskpolicy.ModeEnforce), 1)
	order := env.claimPlan(t)
	before := env.snapshot(t, order.ID)

	_, err := env.submitMarkdown(order, riskPlanMarkdown(""))
	if !errors.Is(err, pipeline.ErrPlanRiskTierMissing) || !strings.Contains(err.Error(), "plan_risk_tier_missing") {
		t.Fatalf("missing tier error=%v", err)
	}
	env.requireRefusedUnchanged(t, before, order)

	for _, line := range []string{"Risk tier: High — Uppercase.", "Risk tier: high", "- Risk tier: high — Listed.", "Risk tier: high — One.\n\nRisk tier: low — Two."} {
		if _, err = env.submitMarkdown(order, riskPlanMarkdown(line)); err == nil || !strings.Contains(err.Error(), "plan risk tier") {
			t.Fatalf("malformed line %q error=%v", line, err)
		}
		env.requireRefusedUnchanged(t, before, order)
	}

	result, err := env.submitMarkdown(order, riskPlanMarkdown("Risk tier: high — The plan changes the approval path."))
	if err != nil {
		t.Fatalf("plan with a tier refused: %v", err)
	}
	env.requireStored(t, result, 1, nil, false)
	if ratings := env.riskRatings(t); len(ratings) != 0 {
		t.Fatalf("an unapproved plan was rated: %+v", ratings)
	}
	task, err := env.st.GetTask(env.ctx, env.taskID)
	if err != nil || task.PlanState != core.PlanAwaitingApproval {
		t.Fatalf("plan state=%s err=%v", task.PlanState, err)
	}

	env.approve(t)
	ratings := env.riskRatings(t)
	if len(ratings) != 1 || ratings[0].Source != core.RiskSourcePlan || ratings[0].Tier != riskpolicy.TierHigh || ratings[0].PlanVersion != 1 {
		t.Fatalf("approved plan ratings=%+v", ratings)
	}
	task, err = env.st.GetTask(env.ctx, env.taskID)
	if err != nil || task.PlanState != core.PlanCompleted {
		t.Fatalf("plan state after approval=%s err=%v", task.PlanState, err)
	}

	// A later approved plan at a lower tier records its own rating and never
	// lowers the task's tier (AC-4.3; component-risk-review invariant 4).
	revision := env.reenterPlan(t)
	if _, err = env.submitMarkdown(revision, riskPlanMarkdown("Risk tier: low — The revision only renames a field.")); err != nil {
		t.Fatal(err)
	}
	env.approve(t)
	ratings = env.riskRatings(t)
	if len(ratings) != 2 || ratings[1].Tier != riskpolicy.TierLow || ratings[1].PlanVersion != 2 {
		t.Fatalf("ratings after the lower plan=%+v", ratings)
	}
	setter, ok := core.RiskTierOf(ratings)
	if !ok || setter.Tier != riskpolicy.TierHigh || setter.PlanVersion != 1 {
		t.Fatalf("task tier set by %+v, want plan version 1 at high", setter)
	}
	task, err = env.st.GetTask(env.ctx, env.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if projection := store.ProjectTaskRisk(task, ratings, 1); projection == nil || projection.Tier != riskpolicy.TierHigh || projection.Source != core.RiskSourcePlan {
		t.Fatalf("task risk=%+v", projection)
	}
}

// TestSubmitPlanRiskTierUnderRecordAndOff shows record accepts a plan with
// no tier and records a stated one at approval, and that a task whose risk
// mode is off records no rating and still refuses a malformed line.
func TestSubmitPlanRiskTierUnderRecordAndOff(t *testing.T) {
	record := newPlanSeatsEnv(t, "plan-risk-record", true, frozenRisk(riskpolicy.ModeRecord), 1)
	result, err := record.submitMarkdown(record.claimPlan(t), riskPlanMarkdown(""))
	if err != nil {
		t.Fatalf("record refused a plan with no tier: %v", err)
	}
	record.requireStored(t, result, 1, nil, false)
	record.approve(t)
	if ratings := record.riskRatings(t); len(ratings) != 0 {
		t.Fatalf("a plan with no tier was rated: %+v", ratings)
	}
	revision := record.reenterPlan(t)
	if _, err = record.submitMarkdown(revision, riskPlanMarkdown("Risk tier: medium — Stated under record.")); err != nil {
		t.Fatal(err)
	}
	record.approve(t)
	if ratings := record.riskRatings(t); len(ratings) != 1 || ratings[0].Tier != riskpolicy.TierMedium || ratings[0].PlanVersion != 2 {
		t.Fatalf("record ratings=%+v", ratings)
	}

	off := newPlanSeatsEnv(t, "plan-risk-off", true, config.PolicyContract{}, 1)
	order := off.claimPlan(t)
	before := off.snapshot(t, order.ID)
	if _, err = off.submitMarkdown(order, riskPlanMarkdown("Risk tier: critical — Not a tier.")); err == nil {
		t.Fatal("off task accepted a malformed tier line")
	}
	off.requireRefusedUnchanged(t, before, order)
	if _, err = off.submitMarkdown(order, riskPlanMarkdown("Risk tier: high — Stated with risk off.")); err != nil {
		t.Fatal(err)
	}
	off.approve(t)
	if ratings := off.riskRatings(t); len(ratings) != 0 {
		t.Fatalf("off task ratings=%+v", ratings)
	}
}
