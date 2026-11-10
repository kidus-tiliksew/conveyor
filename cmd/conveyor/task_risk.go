package main

import (
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
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
