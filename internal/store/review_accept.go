package store

import "github.com/kidus-tiliksew/conveyor/internal/core"

// ReviewAcceptAdmitted reports whether an accepted review round leaves
// running through core.TaskReviewAccept (running -> approved) instead of the
// merge_approval gate (DEC-64(8); component-task-lifecycle v13,
// component-submission-merge v6). verdict is the round's aggregate verdict;
// the policy fields are the task row's, frozen at intake (DEC-74(1)).
//
// Admitted: any refresh round, gate on or off; a policy >0 task with the merge
// gate off; a legacy policy-0 task at L0. Every backend's
// AcceptReviewDecisionCommand uses this one rule.
func ReviewAcceptAdmitted(decision core.ReviewDecision, verdict string) bool {
	if verdict == "changes_requested" {
		return false
	}
	return decision.ReviewKind == "refresh" ||
		(decision.PolicyVersion > 0 && !decision.MergeApproval) ||
		(decision.PolicyVersion == 0 && decision.Level == core.L0)
}
