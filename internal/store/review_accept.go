package store

import "github.com/kidus-tiliksew/conveyor/internal/core"

// ReviewAcceptAdmitted reports whether an accepted review round leaves
// running through core.TaskReviewAccept (running -> approved) instead of the
// merge_approval gate (DEC-64(8); component-task-lifecycle v13,
// component-submission-merge v6). verdict is the round's aggregate verdict;
// the merge approval gate is the task row's, frozen at intake with the rest
// of the pipeline policy (DEC-125(1)).
//
// Admitted: any refresh round, gate on or off; a policy >0 task with the merge
// gate off; a legacy policy-0 task at L0. Every backend's
// AcceptReviewDecisionCommand uses this one rule.
//
// riskHold is RiskMergeHold's answer for the task, read in the acceptance
// transaction. An enforcing task that requires one or more human approvals is
// never admitted, refresh rounds included, so its approval takes the existing
// gate.merge edge to the merge gate (req-risk-based-review AC-8.2, AC-8.6;
// feature-risk-based-review RR-6). riskHold is false for every task whose
// frozen mode is not enforce, which keeps DEC-64(8) unchanged for them.
func ReviewAcceptAdmitted(decision core.ReviewDecision, verdict string, riskHold bool) bool {
	if verdict == "changes_requested" || riskHold {
		return false
	}
	return decision.ReviewKind == "refresh" ||
		(decision.PolicyVersion > 0 && !decision.MergeApproval) ||
		(decision.PolicyVersion == 0 && decision.Level == core.L0)
}
