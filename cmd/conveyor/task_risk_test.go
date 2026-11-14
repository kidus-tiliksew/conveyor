package main

import (
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// TestRiskSummaryLine: conveyor task show states the tier, source, reason,
// and counts the server returns, and says so when the task is not rated yet
// (req-risk-based-review REQ-11, AC-11.2).
func TestRiskSummaryLine(t *testing.T) {
	if got := riskSummaryLine(nil); got != "" {
		t.Fatalf("off task line=%q", got)
	}
	rated := &core.TaskRisk{Mode: riskpolicy.ModeRecord, Tier: riskpolicy.TierMedium, Source: core.RiskSourcePathRule, Reason: "workflows", RequiredAgentReviews: 1, RequiredHumanApprovals: 0}
	if got, want := riskSummaryLine(rated), "Risk tier: medium from path rule (mode record): workflows; needs 1 agent reviews and 0 human approvals; 0 approvals recorded"; got != want {
		t.Fatalf("line=%q\nwant %q", got, want)
	}
	unrated := &core.TaskRisk{Mode: riskpolicy.ModeEnforce, RequiredAgentReviews: 2, RequiredHumanApprovals: 1}
	if got, want := riskSummaryLine(unrated), "Risk tier: not rated yet (mode enforce); needs 2 agent reviews and 1 human approvals; 0 approvals recorded"; got != want {
		t.Fatalf("line=%q\nwant %q", got, want)
	}
	// An enforcing task short of its approvals names how many remain and
	// whether a trusted author's approval is among them (AC-11.1).
	short := &core.TaskRisk{Mode: riskpolicy.ModeEnforce, Tier: riskpolicy.TierHigh, Source: core.RiskSourceReviewer, Reason: "merge gate",
		RequiredAgentReviews: 2, RequiredHumanApprovals: 2, RecordedApprovals: 1, RemainingApprovals: 1, AuthorApprovalNeeded: true}
	if got, want := riskSummaryLine(short), "Risk tier: high from reviewer (mode enforce): merge gate; needs 2 agent reviews and 2 human approvals; 1 approvals recorded; 1 more needed, including a trusted author's"; got != want {
		t.Fatalf("line=%q\nwant %q", got, want)
	}
}

// TestRiskAgentSourceAndOrders: conveyor task show labels a risk agent rating
// with its order and reported model, keeps the historical plan and reviewer
// labels, and lists each risk order with what it rates and its state
// (req-risk-based-review AC-5.6, AC-11.2; feature-risk-based-review RR-16).
func TestRiskAgentSourceAndOrders(t *testing.T) {
	for source, want := range map[core.RiskSource]string{
		core.RiskSourcePlan: "plan", core.RiskSourceReviewer: "reviewer", core.RiskSourcePathRule: "path rule",
		core.RiskSourcePerson: "person", core.RiskSourceRiskAgent: "risk agent",
	} {
		if got := riskSourceLabel(source); got != want {
			t.Errorf("riskSourceLabel(%s) = %q, want %q", source, got, want)
		}
	}
	agent := core.RiskRating{Source: core.RiskSourceRiskAgent, Tier: riskpolicy.TierHigh, Reason: "schema change", RiskWorkOrderID: "t-risk-2", RiskModel: "rater-model", HeadSHA: "abc", BaseSHA: "def"}
	historical := core.RiskRating{Source: core.RiskSourcePlan, Tier: riskpolicy.TierMedium, Reason: "plan line", PlanVersion: 1}
	rated := &core.TaskRisk{Mode: riskpolicy.ModeRecord, Tier: riskpolicy.TierHigh, Source: core.RiskSourceRiskAgent, Reason: "schema change",
		RequiredAgentReviews: 1, Ratings: []core.RiskRating{historical, agent},
		Orders: []core.TaskRiskOrder{
			{WorkOrderID: "t-risk-1", State: core.WorkOrderCancelled, Subject: core.RiskOrderSubject{PlanVersion: 1}},
			{WorkOrderID: "t-risk-2", State: core.WorkOrderCompleted, Subject: core.RiskOrderSubject{HeadSHA: "abc", BaseSHA: "def"}, Model: "rater-model"},
		}}
	if got, want := riskSummaryLine(rated), "Risk tier: high from risk agent order t-risk-2 with model rater-model (mode record): schema change; needs 1 agent reviews and 0 human approvals; 0 approvals recorded"; got != want {
		t.Fatalf("line=%q\nwant %q", got, want)
	}
	lines := riskOrderLines(rated)
	want := []string{"Risk order t-risk-1 rates plan version 1: cancelled", "Risk order t-risk-2 rates head abc against base def: completed (model rater-model)"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines=%q\nwant %q", lines, want)
	}
	if riskOrderLines(nil) != nil {
		t.Fatal("an off task listed risk orders")
	}
	planSetter := &core.TaskRisk{Mode: riskpolicy.ModeRecord, Tier: riskpolicy.TierMedium, Source: core.RiskSourcePlan, Reason: "plan line", RequiredAgentReviews: 1, Ratings: []core.RiskRating{historical}}
	if got := riskSummaryLine(planSetter); !strings.HasPrefix(got, "Risk tier: medium from plan (mode record): plan line;") {
		t.Fatalf("historical plan line=%q", got)
	}
}
