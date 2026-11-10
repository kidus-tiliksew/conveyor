package riskpolicy

import "testing"

// EvaluateApprovals applies req-risk-based-review REQ-8 and REQ-9: distinct
// gatekeepers count once, an untrusted author's approval never counts and
// calls for a non-author, a trusted author's approval is required unless
// waived, and a waiver changes no number.
func TestEvaluateApprovals(t *testing.T) {
	gates := map[string]bool{"alice": true, "bob": true, "carol": true, "dave": true}
	for _, tc := range []struct {
		name string
		in   ApprovalInput
		want ApprovalStatus
	}{
		{
			name: "two people needed and one approving",
			in:   ApprovalInput{Required: 2, Gatekeepers: gates, Approvals: []string{"bob"}},
			want: ApprovalStatus{Required: 2, Recorded: 1, Remaining: 1},
		},
		{
			name: "the second approval completes the count",
			in:   ApprovalInput{Required: 2, Gatekeepers: gates, Approvals: []string{"bob"}, Prospective: "carol"},
			want: ApprovalStatus{Required: 2, Recorded: 2, Remaining: 0, Satisfied: true},
		},
		{
			name: "one person approving twice counts once",
			in:   ApprovalInput{Required: 2, Gatekeepers: gates, Approvals: []string{"bob", "bob"}, Prospective: "bob"},
			want: ApprovalStatus{Required: 2, Recorded: 1, Remaining: 1},
		},
		{
			name: "a person without operate_gates does not count",
			in:   ApprovalInput{Required: 1, Gatekeepers: gates, Approvals: []string{"mallory"}},
			want: ApprovalStatus{Required: 1, Recorded: 0, Remaining: 1},
		},
		{
			name: "a trusted author's approval is required and counts",
			in:   ApprovalInput{Required: 2, Authors: []string{"alice"}, Gatekeepers: gates, Approvals: []string{"bob", "carol"}},
			want: ApprovalStatus{Required: 2, Recorded: 2, Remaining: 1, TrustedAuthorApprovalNeeded: true},
		},
		{
			name: "the trusted author's approval completes the requirement",
			in:   ApprovalInput{Required: 2, Authors: []string{"alice"}, Gatekeepers: gates, Approvals: []string{"bob"}, Prospective: "alice"},
			want: ApprovalStatus{Required: 2, Recorded: 2, Remaining: 0, Satisfied: true},
		},
		{
			name: "a waiver satisfies the author condition and changes no number",
			in:   ApprovalInput{Required: 2, Authors: []string{"alice"}, Gatekeepers: gates, Approvals: []string{"bob"}, Waived: true},
			want: ApprovalStatus{Required: 2, Recorded: 1, Remaining: 1},
		},
		{
			name: "an untrusted author's approval does not count and a non-author is required",
			in: ApprovalInput{Required: 1, Authors: []string{"alice"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates,
				Approvals: []string{"alice"}},
			want: ApprovalStatus{Required: 1, Recorded: 0, Remaining: 1, UntrustedAuthor: true, NonAuthorApprovalNeeded: true},
		},
		{
			name: "an untrusted author raises a zero count to one",
			in:   ApprovalInput{Required: 0, Authors: []string{"alice"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates},
			want: ApprovalStatus{Required: 1, Remaining: 1, UntrustedAuthor: true, NonAuthorApprovalNeeded: true},
		},
		{
			name: "a non-author satisfies an untrusted author's task",
			in: ApprovalInput{Required: 1, Authors: []string{"alice"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates,
				Approvals: []string{"alice"}, Prospective: "carol"},
			want: ApprovalStatus{Required: 1, Recorded: 1, Remaining: 0, UntrustedAuthor: true, Satisfied: true},
		},
		{
			name: "a trusted coauthor's approval does not count beside an untrusted author",
			in: ApprovalInput{Required: 1, Authors: []string{"alice", "bob"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates,
				Approvals: []string{"bob"}},
			want: ApprovalStatus{Required: 1, Recorded: 0, Remaining: 1, UntrustedAuthor: true, NonAuthorApprovalNeeded: true, AuthorWaiverNeeded: true},
		},
		{
			name: "mixed authors with a non-author's approval still need the waiver",
			in: ApprovalInput{Required: 1, Authors: []string{"alice", "bob"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates,
				Approvals: []string{"bob", "carol"}},
			want: ApprovalStatus{Required: 1, Recorded: 1, Remaining: 0, UntrustedAuthor: true, AuthorWaiverNeeded: true},
		},
		{
			name: "mixed authors with a waiver still need a non-author",
			in: ApprovalInput{Required: 1, Authors: []string{"alice", "bob"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates,
				Waived: true},
			want: ApprovalStatus{Required: 1, Remaining: 1, UntrustedAuthor: true, NonAuthorApprovalNeeded: true},
		},
		{
			name: "mixed authors with a waiver and a non-author's approval are satisfied",
			in: ApprovalInput{Required: 1, Authors: []string{"alice", "bob"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates,
				Approvals: []string{"bob"}, Waived: true, Prospective: "carol"},
			want: ApprovalStatus{Required: 1, Recorded: 1, UntrustedAuthor: true, Satisfied: true},
		},
		{
			name: "an unknown author is never trusted",
			in:   ApprovalInput{Required: 0, UnknownAuthor: true, Gatekeepers: gates},
			want: ApprovalStatus{Required: 1, Remaining: 1, UntrustedAuthor: true, NonAuthorApprovalNeeded: true},
		},
		{
			name: "an empty trust list trusts operate_gates holders only",
			in:   ApprovalInput{Required: 0, Authors: []string{"former"}, Gatekeepers: gates},
			want: ApprovalStatus{Required: 1, Remaining: 1, UntrustedAuthor: true, NonAuthorApprovalNeeded: true},
		},
		{
			name: "zero required with trusted authors is satisfied",
			in:   ApprovalInput{Required: 0, Authors: []string{"alice"}, Gatekeepers: gates},
			want: ApprovalStatus{Required: 0, Satisfied: true},
		},
		{
			name: "no authors applies only the count",
			in:   ApprovalInput{Required: 1, Gatekeepers: gates, Prospective: "dave"},
			want: ApprovalStatus{Required: 1, Recorded: 1, Satisfied: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateApprovals(tc.in)
			got.ProspectiveEligible, got.ProspectiveReason = false, ""
			if got != tc.want {
				t.Fatalf("EvaluateApprovals = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A prospective approval reports why it does not count.
func TestEvaluateApprovalsProspectiveReason(t *testing.T) {
	gates := map[string]bool{"alice": true, "bob": true}
	for _, tc := range []struct {
		in     ApprovalInput
		reason string
	}{
		{ApprovalInput{Required: 1, Gatekeepers: gates, Prospective: "mallory"}, ApprovalReasonNoCapability},
		{ApprovalInput{Required: 1, Authors: []string{"alice"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates, Prospective: "alice"}, ApprovalReasonUntrustedAuthor},
		{ApprovalInput{Required: 2, Gatekeepers: gates, Approvals: []string{"bob"}, Prospective: "bob"}, ApprovalReasonAlreadyCounted},
		{ApprovalInput{Required: 1, Authors: []string{"alice", "bob"}, TrustedAuthors: []string{"bob"}, Gatekeepers: gates, Prospective: "bob"}, ApprovalReasonAuthorOfUntrusted},
		{ApprovalInput{Required: 1, Gatekeepers: gates, Prospective: "bob"}, ""},
	} {
		got := EvaluateApprovals(tc.in)
		if got.ProspectiveReason != tc.reason || got.ProspectiveEligible != (tc.reason == "") {
			t.Fatalf("prospective %q: eligible=%v reason=%q, want reason %q", tc.in.Prospective, got.ProspectiveEligible, got.ProspectiveReason, tc.reason)
		}
	}
}
