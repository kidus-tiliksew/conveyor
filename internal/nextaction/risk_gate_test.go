package nextaction

import (
	"strings"
	"testing"
)

// An enforcing task whose required human approvals are one or more is behind
// the merge gate whatever its frozen merge_approval says (DEC-124(2)): an
// approved task names the operator's merge at row 7, ahead of a check hold
// and of merging. Approvals are counted, so the merge choice stays offered:
// while more than one approval remains it is labelled Approve and records the
// person's approval without merging (component-next-action rows 7 to 9;
// req-risk-based-review AC-8.2, AC-8.7, AC-11.1).
func TestRiskHumanApprovalsPutAnApprovedTaskBehindTheMergeGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		approvals int
		shortfall *RiskApprovals
		hold      *CheckHold
		kind      string
		actor     string
		merge     bool
		label     string
	}{
		{"NoneRequiredMerges", 0, nil, nil, KindMerging, ActorFactory, false, ""},
		{"OneRequired", 1, &RiskApprovals{Required: 1, Remaining: 1}, nil, KindMergeExecution, ActorOperator, true, "Merge"},
		{"OneRequiredAheadOfACheckHold", 1, &RiskApprovals{Required: 1, Remaining: 1}, &CheckHold{Reason: CheckHoldFailed}, KindMergeExecution, ActorOperator, true, "Merge"},
		{"TwoRequiredOffersApprove", 2, &RiskApprovals{Required: 2, Remaining: 2}, nil, KindMergeExecution, ActorOperator, true, "Approve"},
		{"TwoRequiredOneRecordedOffersMerge", 2, &RiskApprovals{Required: 2, Recorded: 1, Remaining: 1}, nil, KindMergeExecution, ActorOperator, true, "Merge"},
		{"SatisfiedOffersMerge", 2, &RiskApprovals{Required: 2, Recorded: 2, Satisfied: true}, nil, KindMergeExecution, ActorOperator, true, "Merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := checkHoldInput(tc.hold, false)
			in.RiskHumanApprovals = tc.approvals
			in.RiskApprovals = tc.shortfall
			na := Project(in)
			if na.Kind != tc.kind || na.Actor != tc.actor {
				t.Fatalf("next action %s/%s, want %s/%s", na.Actor, na.Kind, tc.actor, tc.kind)
			}
			label := ""
			for _, choice := range na.Choices {
				if choice.ID == "merge" {
					label = choice.Label
				}
			}
			if (label != "") != tc.merge || label != tc.label {
				t.Fatalf("merge choice label=%q, want offered=%t label %q (choices %+v)", label, tc.merge, tc.label, na.Choices)
			}
			for _, refusal := range na.Details.Refusals {
				if refusal.Choice == "merge" {
					t.Fatalf("merge refused: %+v", refusal)
				}
			}
			if tc.shortfall != nil && !tc.shortfall.Satisfied && !strings.Contains(na.Summary, "required human approvals") {
				t.Fatalf("summary %q does not name the approvals", na.Summary)
			}
		})
	}
}

// The shortfall sentence names how many approvals remain and whether a
// trusted author's approval is among them (req-risk-based-review AC-11.1).
func TestRiskApprovalSummary(t *testing.T) {
	for _, tc := range []struct {
		in   RiskApprovals
		want string
	}{
		{RiskApprovals{Required: 2, Recorded: 1, Remaining: 1}, "The reviewed head has 1 of 2 required human approvals. 1 more approval is needed. A trusted author's approval is not among them."},
		{RiskApprovals{Required: 2, Remaining: 2, TrustedAuthorNeeded: true}, "The reviewed head has 0 of 2 required human approvals. 2 more approvals are needed, including a trusted author's approval."},
		{RiskApprovals{Required: 1, Remaining: 1, NonAuthorNeeded: true}, "The reviewed head has 0 of 1 required human approvals. 1 more approval is needed, including an approval from a person who is not an author."},
		{RiskApprovals{Required: 1, Recorded: 1, Remaining: 2, TrustedAuthorNeeded: true, NonAuthorNeeded: true}, "The reviewed head has 1 of 1 required human approvals. 2 more approvals are needed, including a trusted author's approval and an approval from a person who is not an author."},
	} {
		if got := RiskApprovalSummary(tc.in); got != tc.want {
			t.Fatalf("RiskApprovalSummary(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
