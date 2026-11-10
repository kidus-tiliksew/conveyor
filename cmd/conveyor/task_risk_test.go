package main

import (
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
