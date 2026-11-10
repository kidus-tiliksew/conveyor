package nextaction

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// An enforcing task whose required human approvals are one or more is behind
// the merge gate whatever its frozen merge_approval says (DEC-124(2)): an
// approved task names the operator's merge at row 7, ahead of a check hold
// and of merging. Approvals are counted, so the merge choice stays offered:
// while more than one approval remains it is labelled Approve and records the
// person's approval without merging (component-next-action rows 7 to 9;
// req-risk-based-review AC-8.2, AC-8.7, AC-11.1).
func TestRiskHumanApprovalsPutAnApprovedTaskBehindTheMergeGate(t *testing.T) {
	// A non-author caller whose approval completes the requirements, one
	// whose approval leaves two short, and a task already satisfied.
	completing := riskpolicy.ApprovalStatus{Required: 1, Recorded: 1, Satisfied: true, ProspectiveEligible: true}
	short := riskpolicy.ApprovalStatus{Required: 2, Recorded: 1, Remaining: 1, ProspectiveEligible: true}
	for _, tc := range []struct {
		name      string
		approvals int
		shortfall *RiskApprovals
		hold      *CheckHold
		kind      string
		actor     string
		label     string
	}{
		{"NoneRequiredMerges", 0, nil, nil, KindMerging, ActorFactory, ""},
		{"OneRequiredCompletedByTheCaller", 1, ptr(RiskApprovalsFrom(riskpolicy.ApprovalStatus{Required: 1, Remaining: 1}, &completing)), nil, KindMergeExecution, ActorOperator, "Merge"},
		{"OneRequiredAheadOfACheckHold", 1, ptr(RiskApprovalsFrom(riskpolicy.ApprovalStatus{Required: 1, Remaining: 1}, &completing)), &CheckHold{Reason: CheckHoldFailed}, KindMergeExecution, ActorOperator, "Merge"},
		{"TwoRequiredCallerLeavesOneShort", 2, ptr(RiskApprovalsFrom(riskpolicy.ApprovalStatus{Required: 2, Remaining: 2}, &short)), nil, KindMergeExecution, ActorOperator, "Approve"},
		{"SatisfiedOffersMerge", 2, &RiskApprovals{Required: 2, Recorded: 2, Satisfied: true}, nil, KindMergeExecution, ActorOperator, "Merge"},
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
			if label != tc.label {
				t.Fatalf("merge choice label=%q, want %q (choices %+v)", label, tc.label, na.Choices)
			}
			if tc.shortfall != nil && !tc.shortfall.Satisfied && !strings.Contains(na.Summary, "required human approvals") {
				t.Fatalf("summary %q does not name the approvals", na.Summary)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// The merge and approve choices over every combination of the approvals still
// needed (0, 1, 2 or more), whether only a waiver can meet the author
// condition, whether the caller's approval is already counted, and the
// caller's role (an author who is not trusted, a person who is not an author,
// a trusted author). Each cell is built through riskpolicy.EvaluateApprovals,
// the evaluator the merge route uses, and mapped by RiskApprovalsFrom as the
// store does. The merge choice keeps the label Merge only when the act lands
// the head; otherwise it is Approve with what is still needed, or withheld
// with the reason (req-risk-based-review AC-8.7, AC-11.1, REQ-9;
// feature-risk-based-review RR-7).
func TestRiskApprovalChoiceGrid(t *testing.T) {
	type role string
	const (
		untrustedAuthor role = "untrusted author"
		nonAuthor       role = "non-author"
		trustedAuthor   role = "trusted author"
	)
	callerID := map[role]string{untrustedAuthor: "u", nonAuthor: "n", trustedAuthor: "t"}
	gates := map[string]bool{"u": true, "n": true, "t": true, "o1": true, "o2": true, "o3": true}
	type authorSet struct {
		authors []string
		trusted []string
	}
	// find searches small inputs for one that reaches the cell.
	find := func(remaining int, waiver, counted bool, r role) (riskpolicy.ApprovalInput, bool) {
		var sets []authorSet
		switch {
		case waiver:
			sets = []authorSet{{[]string{"t", "u"}, []string{"t"}}}
		case r == untrustedAuthor:
			sets = []authorSet{{[]string{"u"}, []string{"x"}}}
		case r == trustedAuthor:
			sets = []authorSet{{[]string{"t"}, nil}}
		default:
			sets = []authorSet{{[]string{"t"}, nil}, {[]string{"u"}, []string{"x"}}}
		}
		others := [][]string{{}, {"o1"}, {"o1", "o2"}, {"o1", "o2", "o3"}, {"t"}, {"o1", "t"}}
		for _, set := range sets {
			for _, waived := range []bool{false, true} {
				for required := 1; required <= 3; required++ {
					for _, approvals := range others {
						approvals := append([]string{}, approvals...)
						if counted {
							approvals = append(approvals, callerID[r])
						}
						in := riskpolicy.ApprovalInput{Required: required, Authors: set.authors, TrustedAuthors: set.trusted, Gatekeepers: gates, Approvals: approvals, Waived: waived}
						base := riskpolicy.EvaluateApprovals(in)
						bucket := min(base.Remaining, 2)
						if bucket != remaining || base.AuthorWaiverNeeded != waiver {
							continue
						}
						in.Prospective = callerID[r]
						if (riskpolicy.EvaluateApprovals(in).ProspectiveReason == riskpolicy.ApprovalReasonAlreadyCounted) != counted {
							continue
						}
						in.Prospective = ""
						return in, true
					}
				}
			}
		}
		return riskpolicy.ApprovalInput{}, false
	}
	reached := map[string]bool{}
	for _, remaining := range []int{0, 1, 2} {
		for _, waiver := range []bool{false, true} {
			for _, counted := range []bool{false, true} {
				for _, r := range []role{untrustedAuthor, nonAuthor, trustedAuthor} {
					name := fmt.Sprintf("remaining=%d/waiver=%t/counted=%t/%s", remaining, waiver, counted, r)
					t.Run(name, func(t *testing.T) {
						in, ok := find(remaining, waiver, counted, r)
						if !ok {
							// Only an approval that can never count is unreachable
							// as counted: an untrusted author's, or a trusted
							// author's beside an untrusted author (REQ-9).
							if !counted || (r != untrustedAuthor && !(r == trustedAuthor && waiver)) {
								t.Fatalf("cell is unexpectedly unreachable")
							}
							t.Logf("unreachable")
							return
						}
						reached[name] = true
						base := riskpolicy.EvaluateApprovals(in)
						in.Prospective = callerID[r]
						withCaller := riskpolicy.EvaluateApprovals(in)
						approvals := RiskApprovalsFrom(base, &withCaller)
						held := checkHoldInput(nil, false)
						held.RiskHumanApprovals, held.RiskApprovals = base.Required, &approvals
						na := Project(held)
						merge, offered := Choice{}, false
						for _, choice := range na.Choices {
							if choice.ID == "merge" {
								merge, offered = choice, true
							}
						}
						refusal := ""
						for _, item := range na.Details.Refusals {
							if item.Choice == "merge" {
								refusal = item.Text
							}
						}
						_, approveEffect, approveRefusal, _ := riskApprovalChoice(Input{RiskApprovals: &approvals}, "approve")
						// The rule, stated independently of the implementation.
						authorOfUntrusted := r == trustedAuthor && (waiver || withCaller.UntrustedAuthor)
						switch {
						case base.Satisfied:
							if !offered || merge.Label != "Merge" || refusal != "" {
								t.Fatalf("satisfied: merge=%+v refusal=%q", merge, refusal)
							}
						case remaining == 0 && waiver:
							if offered || !strings.HasPrefix(refusal, "No approval can complete the merge now.") || approveRefusal == "" {
								t.Fatalf("waiver only: merge=%+v refusal=%q approve refusal=%q", merge, refusal, approveRefusal)
							}
						case counted:
							if offered || !strings.HasPrefix(refusal, "Your approval of this head is already recorded.") || approveRefusal != refusal {
								t.Fatalf("counted: merge=%+v refusal=%q", merge, refusal)
							}
						case r == untrustedAuthor:
							if offered || !strings.Contains(refusal, "you are an author who is not trusted") {
								t.Fatalf("untrusted author: merge=%+v refusal=%q", merge, refusal)
							}
						case authorOfUntrusted:
							if offered || !strings.Contains(refusal, "another author is not trusted") {
								t.Fatalf("author beside an untrusted author: merge=%+v refusal=%q", merge, refusal)
							}
						case withCaller.Satisfied:
							if !offered || merge.Label != "Merge" || !strings.Contains(merge.Effect, "Your approval completes the required approvals") || !strings.Contains(approveEffect, "approves the merge") {
								t.Fatalf("completing: merge=%+v approve=%q", merge, approveEffect)
							}
						default:
							if !offered || merge.Label != "Approve" || !strings.Contains(merge.Effect, "It does not merge, because the merge still needs") || !strings.Contains(approveEffect, "It does not approve the merge") {
								t.Fatalf("short: merge=%+v approve=%q", merge, approveEffect)
							}
						}
						// Soundness: the label Merge is offered only when the
						// merge route lands the head.
						if offered && merge.Label == "Merge" && !base.Satisfied && !withCaller.Satisfied {
							t.Fatalf("Merge offered for an act that cannot land: base=%+v caller=%+v", base, withCaller)
						}
						text := merge.Label + ": " + merge.Effect
						if !offered {
							text = "withheld: " + refusal
						}
						t.Logf("%s", text)
					})
				}
			}
		}
	}
	// The states the review named are reachable and covered.
	for _, name := range []string{
		"remaining=0/waiver=true/counted=false/non-author",
		"remaining=1/waiver=false/counted=true/non-author",
		"remaining=1/waiver=false/counted=false/non-author",
		"remaining=1/waiver=true/counted=false/trusted author",
	} {
		if !reached[name] {
			t.Errorf("cell %s is not reached", name)
		}
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
		{RiskApprovals{Required: 1, Remaining: 1, NonAuthorNeeded: true, WaiverNeeded: true}, "The reviewed head has 0 of 1 required human approvals. 1 more approval is needed, including an approval from a person who is not an author. No author's approval counts while an author is not trusted, so a person who is not an author must also waive the trusted author's approval."},
		{RiskApprovals{Required: 1, Recorded: 1, WaiverNeeded: true}, "The reviewed head has 1 of 1 required human approvals. No author's approval counts while an author is not trusted, so a person who is not an author must waive the trusted author's approval."},
	} {
		if got := RiskApprovalSummary(tc.in); got != tc.want {
			t.Fatalf("RiskApprovalSummary(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
