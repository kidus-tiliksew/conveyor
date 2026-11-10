package main

import (
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// riskSummaryLine states the task's risk tier, its source and reason, and the
// requirements in effect, from the risk field the server states. It returns
// "" for a task whose frozen risk mode is off (req-risk-based-review REQ-11,
// AC-11.2).
func riskSummaryLine(risk *core.TaskRisk) string {
	if risk == nil {
		return ""
	}
	counts := fmt.Sprintf("needs %d agent reviews and %d human approvals; %d approvals recorded", risk.RequiredAgentReviews, risk.RequiredHumanApprovals, risk.RecordedApprovals)
	// An enforcing task short of its approvals states how many remain and
	// each author condition, as the server counts them (req-risk-based-review
	// AC-11.1, AC-11.2).
	if risk.Mode == riskpolicy.ModeEnforce && risk.RequiredHumanApprovals > 0 && !risk.ApprovalsSatisfied && risk.RemainingApprovals > 0 {
		counts += fmt.Sprintf("; %d more needed", risk.RemainingApprovals)
		switch {
		case risk.AuthorApprovalNeeded && risk.NonAuthorApprovalNeeded:
			counts += ", including a trusted author's and one from a person who is not an author"
		case risk.AuthorApprovalNeeded:
			counts += ", including a trusted author's"
		case risk.NonAuthorApprovalNeeded:
			counts += ", including one from a person who is not an author"
		}
	}
	if risk.Tier == "" {
		return fmt.Sprintf("Risk tier: not rated yet (mode %s); %s", risk.Mode, counts)
	}
	return fmt.Sprintf("Risk tier: %s from %s (mode %s): %s; %s", risk.Tier, riskSourceLabel(risk.Source), risk.Mode, risk.Reason, counts)
}

func riskSourceLabel(source core.RiskSource) string {
	switch source {
	case core.RiskSourcePathRule:
		return "path rule"
	case core.RiskSourceReviewer:
		return "reviewer"
	case core.RiskSourcePerson:
		return "person"
	case core.RiskSourcePlan:
		return "plan"
	}
	return string(source)
}
