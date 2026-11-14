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
	if risk.Mode == riskpolicy.ModeEnforce && risk.AuthorWaiverNeeded {
		counts += "; a non-author must waive the trusted author's approval"
	}
	if risk.Tier == "" {
		return fmt.Sprintf("Risk tier: not rated yet (mode %s); %s", risk.Mode, counts)
	}
	return fmt.Sprintf("Risk tier: %s from %s (mode %s): %s; %s", risk.Tier, riskSetterLabel(risk), risk.Mode, risk.Reason, counts)
}

// riskSetterLabel names the source of the rating that sets the task's tier.
// A risk agent rating also names its risk order and the model its session
// reported (req-risk-based-review AC-5.6, AC-11.2). The setter is the rating
// core.RiskTierOf selects from the ratings the server states.
func riskSetterLabel(risk *core.TaskRisk) string {
	label := riskSourceLabel(risk.Source)
	if risk.Source != core.RiskSourceRiskAgent {
		return label
	}
	setter, ok := core.RiskTierOf(risk.Ratings)
	if !ok || setter.Source != core.RiskSourceRiskAgent || setter.RiskWorkOrderID == "" {
		return label
	}
	if setter.RiskModel == "" {
		return fmt.Sprintf("%s order %s", label, setter.RiskWorkOrderID)
	}
	return fmt.Sprintf("%s order %s with model %s", label, setter.RiskWorkOrderID, setter.RiskModel)
}

// riskOrderLines states each risk order of the task with what it rates and
// its state (req-risk-based-review AC-11.2; feature-risk-based-review RR-16).
func riskOrderLines(risk *core.TaskRisk) []string {
	if risk == nil {
		return nil
	}
	lines := make([]string, 0, len(risk.Orders))
	for _, order := range risk.Orders {
		subject := "an unknown subject"
		switch {
		case order.Subject.PlanVersion > 0:
			subject = fmt.Sprintf("plan version %d", order.Subject.PlanVersion)
		case order.Subject.HeadSHA != "":
			subject = fmt.Sprintf("head %s against base %s", order.Subject.HeadSHA, order.Subject.BaseSHA)
		}
		line := fmt.Sprintf("Risk order %s rates %s: %s", order.WorkOrderID, subject, order.State)
		if order.Model != "" {
			line += fmt.Sprintf(" (model %s)", order.Model)
		}
		lines = append(lines, line)
	}
	return lines
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
	case core.RiskSourceRiskAgent:
		return "risk agent"
	}
	return string(source)
}
