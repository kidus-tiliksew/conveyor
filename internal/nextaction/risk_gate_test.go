package nextaction

import (
	"strings"
	"testing"
)

// An enforcing task whose required human approvals are one or more is behind
// the merge gate whatever its frozen merge_approval says (DEC-124(2)): an
// approved task names the operator's merge at row 7, ahead of a check hold
// and of merging. At two or more required approvals the merge choice is
// withheld with the route's reason (component-next-action rows 7 to 9 and
// invariant 7; req-risk-based-review AC-8.2, AC-8.7).
func TestRiskHumanApprovalsPutAnApprovedTaskBehindTheMergeGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		approvals int
		hold      *CheckHold
		kind      string
		actor     string
		merge     bool
	}{
		{"NoneRequiredMerges", 0, nil, KindMerging, ActorFactory, false},
		{"OneRequired", 1, nil, KindMergeExecution, ActorOperator, true},
		{"OneRequiredAheadOfACheckHold", 1, &CheckHold{Reason: CheckHoldFailed}, KindMergeExecution, ActorOperator, true},
		{"TwoRequiredWithholdsTheMerge", 2, nil, KindMergeExecution, ActorOperator, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := checkHoldInput(tc.hold, false)
			in.RiskHumanApprovals = tc.approvals
			na := Project(in)
			if na.Kind != tc.kind || na.Actor != tc.actor {
				t.Fatalf("next action %s/%s, want %s/%s", na.Actor, na.Kind, tc.actor, tc.kind)
			}
			merge := false
			for _, choice := range na.Choices {
				merge = merge || choice.ID == "merge"
			}
			if merge != tc.merge {
				t.Fatalf("merge offered=%t, want %t (choices %+v)", merge, tc.merge, na.Choices)
			}
			if tc.approvals >= 2 {
				refused := false
				for _, refusal := range na.Details.Refusals {
					refused = refused || (refusal.Choice == "merge" && strings.Contains(refusal.Text, "requires 2 human approvals"))
				}
				if !refused {
					t.Fatalf("refusals=%+v", na.Details.Refusals)
				}
			}
		})
	}
}
