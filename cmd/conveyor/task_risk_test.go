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
}
