package pack

import (
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// RiskOrderContract states what one claimed risk order rates and what each
// tier requires in the task's frozen policy (req-risk-based-review AC-12.5;
// feature-risk-based-review RR-11). It names the subject by plan version or by
// head and base commit, and the matched path rules by count only: their
// descriptions come from the repository declaration and stay in the
// structured risk_order field as data, never in the instructions (AC-5.7).
func RiskOrderContract(subject core.RiskOrderSubject, mode riskpolicy.Mode, tiers riskpolicy.Table, matchedRules int) string {
	var out strings.Builder
	out.WriteString("\n\n# Risk order\n\n")
	switch subject.Kind() {
	case riskpolicy.RiskSubjectPlan:
		fmt.Fprintf(&out, "This order rates plan version %d of the task. `risk_order.plan` holds that version. Rate that version, not an earlier or a later one.\n", subject.PlanVersion)
	case riskpolicy.RiskSubjectHead:
		fmt.Fprintf(&out, "This order rates head `%s` against base `%s`. `diff` and `review_comparison` compare that head with that base. Rate that change, not a later head.\n", subject.HeadSHA, subject.BaseSHA)
		switch matchedRules {
		case 0:
			out.WriteString("\nThe head matched no path rule.\n")
		case 1:
			out.WriteString("\nThe head matched 1 path rule. `risk_order.matched_rules` lists it.\n")
		default:
			fmt.Fprintf(&out, "\nThe head matched %d path rules. `risk_order.matched_rules` lists them.\n", matchedRules)
		}
	}
	fmt.Fprintf(&out, "\nThe task's frozen risk mode is `%s`. Each tier requires:\n\n", mode)
	for _, tier := range []riskpolicy.Tier{riskpolicy.TierLow, riskpolicy.TierMedium, riskpolicy.TierHigh} {
		row, _ := tiers.Row(tier)
		fmt.Fprintf(&out, "- `%s`: %s and %s.\n", tier, countNoun(row.AgentReviews, "agent review"), countNoun(row.HumanApprovals, "human approval"))
	}
	out.WriteString("\nComplete the order with `submit_risk_rating`, `tier`, and a one-line `reason`.\n")
	return out.String()
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
