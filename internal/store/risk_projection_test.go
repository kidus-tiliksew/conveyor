package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// TestProjectTaskRisk covers the shared task read field risk
// (req-risk-based-review AC-1.1, AC-1.6, AC-5.5, REQ-7, REQ-8, REQ-11;
// component-risk-review invariants 2 and 4).
func TestProjectTaskRisk(t *testing.T) {
	enforce := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}
	task := core.Task{ID: "risk-projection", ReviewedHeadSHA: "reviewed", MergeApproval: true, SetupContract: config.PolicyContract{Risk: &enforce}}

	if got := ProjectTaskRisk(core.Task{ID: "off"}, nil, 1); got != nil {
		t.Fatalf("off task projection=%+v, want nil", got)
	}

	unrated := ProjectTaskRisk(task, nil, 1)
	if unrated == nil || unrated.Tier != "" || unrated.Source != "" || unrated.Reason != "" || unrated.Head != "reviewed" || len(unrated.Ratings) != 0 {
		t.Fatalf("unrated projection=%+v, want no invented tier", unrated)
	}
	// An unrated enforcing task is low: the frozen floors stand, and a merge
	// gate counts one approval (REQ-2, AC-2.1).
	if unrated.RequiredAgentReviews != 1 || unrated.RequiredHumanApprovals != 1 || unrated.CalculatedAgentReviews != 1 || unrated.CalculatedHumanApprovals != 1 {
		t.Fatalf("unrated requirements=%+v", unrated)
	}

	ratings := []core.RiskRating{
		{Seq: 3, Source: core.RiskSourceReviewer, Tier: riskpolicy.TierLow, HeadSHA: "head-2", Reason: "seat low"},
		{Seq: 1, Source: core.RiskSourcePathRule, Tier: riskpolicy.TierHigh, HeadSHA: "head-1", Reason: "changed paths could not be read in full", HumanApprovals: 0},
		{Seq: 2, Source: core.RiskSourcePathRule, Tier: riskpolicy.TierMedium, HeadSHA: "head-2", Reason: "workflows", HumanApprovals: 1},
	}
	projected := ProjectTaskRisk(task, ratings, 1)
	// A less sensitive later head and a low seat never lower the tier.
	if projected.Tier != riskpolicy.TierHigh || projected.Source != core.RiskSourcePathRule || projected.Reason != "changed paths could not be read in full" || projected.Head != "head-2" {
		t.Fatalf("projection=%+v", projected)
	}
	// Under enforce the tier's requirements are in effect (REQ-7, REQ-8).
	if projected.RequiredAgentReviews != 2 || projected.RequiredHumanApprovals != 2 || projected.CalculatedAgentReviews != 2 || projected.CalculatedHumanApprovals != 2 {
		t.Fatalf("requirements=%+v, want high's table row in effect", projected)
	}
	// Under record the frozen floors stay in effect and the tier's row is
	// only calculated (AC-1.6).
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	recorded := task
	recorded.SetupContract = config.PolicyContract{Risk: &record}
	if got := ProjectTaskRisk(recorded, ratings, 1); got.RequiredAgentReviews != 1 || got.RequiredHumanApprovals != 1 || got.CalculatedAgentReviews != 2 || got.CalculatedHumanApprovals != 2 {
		t.Fatalf("record requirements=%+v, want the floors in effect and high's table row calculated", got)
	}
	if len(projected.Ratings) != 3 || projected.Ratings[0].Seq != 1 || projected.Ratings[2].Seq != 3 {
		t.Fatalf("ratings not in recorded order: %+v", projected.Ratings)
	}
}

// TestProjectTaskRiskServesPathCounts: the projection serves each rating
// with counts in place of its path lists and leaves the stored ratings it
// read unchanged. The tier and the requirements still come from the stored
// ratings, so a path rule's stored human approvals stay in effect
// (component-risk-review, Public surfaces; req-risk-based-review AC-11.2).
func TestProjectTaskRiskServesPathCounts(t *testing.T) {
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	task := core.Task{ID: "risk-served", ReviewedHeadSHA: "reviewed", SetupContract: config.PolicyContract{Risk: &record}}
	rules := []riskpolicy.RuleMatch{{Source: riskpolicy.RuleDeclared, Index: 1, Description: "billing", MinTier: riskpolicy.TierLow, MinHumanApprovals: 3, Paths: []string{"billing/a.go", "billing/b.go"}}}
	ratings := []core.RiskRating{
		{Seq: 2, Key: core.RiskAgentKey("o"), Source: core.RiskSourceRiskAgent, Tier: riskpolicy.TierMedium, HeadSHA: "head-1", BaseSHA: "base", Reason: "agent",
			RiskWorkOrderID: "o", RiskModel: "m", MatchedRules: rules, CoveredPaths: []string{"billing/a.go", "billing/b.go", "web/x.ts"}},
		{Seq: 1, Key: core.PathRiskKey("head-1"), Source: core.RiskSourcePathRule, Tier: riskpolicy.TierLow, HeadSHA: "head-1", BaseSHA: "base", Reason: "rules",
			MatchedRules: rules, HumanApprovals: 3, DeclarationState: riskpolicy.DeclarationPresent},
	}
	before := make([]core.RiskRating, len(ratings))
	for i, rating := range ratings {
		before[i] = cloneRiskRating(rating)
	}
	projected := ProjectTaskRisk(task, ratings, 1)
	if len(projected.Ratings) != 2 || projected.Ratings[0].Seq != 1 || projected.Ratings[1].Seq != 2 {
		t.Fatalf("served ratings = %+v", projected.Ratings)
	}
	agent := projected.Ratings[1]
	if agent.CoveredPathCount == nil || *agent.CoveredPathCount != 3 || len(agent.MatchedRules) != 1 || agent.MatchedRules[0].PathCount != 2 ||
		agent.RiskWorkOrderID != "o" || agent.RiskModel != "m" || agent.BaseSHA != "base" {
		t.Fatalf("served risk agent rating = %+v", agent)
	}
	if path := projected.Ratings[0]; path.CoveredPathCount != nil || path.MatchedRules[0].PathCount != 2 || path.HumanApprovals != 3 || path.DeclarationState != riskpolicy.DeclarationPresent {
		t.Fatalf("served path rating = %+v", path)
	}
	// The projection sorts its input by sequence, as before; it changes no
	// rating and no nested list.
	byKey := map[string]core.RiskRating{}
	for _, rating := range ratings {
		byKey[rating.Key] = rating
	}
	for _, want := range before {
		if got := byKey[want.Key]; !reflect.DeepEqual(got, want) {
			t.Fatalf("stored rating changed:\ngot  %+v\nwant %+v", got, want)
		}
	}
	if projected.Tier != riskpolicy.TierMedium || projected.Source != core.RiskSourceRiskAgent || projected.Reason != "agent" || projected.Head != "head-1" {
		t.Fatalf("projection = %+v", projected)
	}
	// Medium's table row asks for one approval; the stored path rule asks
	// for three, and the calculated count takes the larger.
	if projected.CalculatedHumanApprovals != 3 || projected.RequiredAgentReviews != 1 || projected.RequiredHumanApprovals != 0 {
		t.Fatalf("requirements = %+v", projected)
	}
}

// TestPrepareRiskRaiseIsStrict covers the shared raise rule every backend
// applies (req-risk-based-review AC-6.1, AC-6.2).
func TestPrepareRiskRaiseIsStrict(t *testing.T) {
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	task := core.Task{ID: "raise", SetupContract: config.PolicyContract{Risk: &record}}
	existing := []core.RiskRating{{Seq: 1, Key: core.PathRiskKey("h"), Source: core.RiskSourcePathRule, Tier: riskpolicy.TierMedium, HeadSHA: "h", Reason: "workflows"}}
	for _, tier := range []riskpolicy.Tier{riskpolicy.TierLow, riskpolicy.TierMedium} {
		_, err := PrepareRiskRaise(task, existing, core.RiskRating{Tier: tier, Reason: "lower", ActorID: "user:a"}, time.Now())
		notRaised, ok := err.(*core.RiskTierNotRaisedError)
		if !ok || notRaised.Current != riskpolicy.TierMedium {
			t.Fatalf("raise to %s err=%v", tier, err)
		}
	}
	raised, err := PrepareRiskRaise(task, existing, core.RiskRating{Tier: riskpolicy.TierHigh, Reason: "payments", ActorID: "user:a"}, time.Now())
	if err != nil || raised.Seq != 2 || raised.PreviousTier != riskpolicy.TierMedium || raised.Key != core.PersonRiskKey(riskpolicy.TierHigh) || raised.Source != core.RiskSourcePerson {
		t.Fatalf("raise=%+v err=%v", raised, err)
	}
}
