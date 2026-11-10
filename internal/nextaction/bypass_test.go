package nextaction

import (
	"strings"
	"testing"
)

// A bypass entry names the forge actor, the head, and what was missing
// (component-next-action invariant 21).
func TestMergeBypassSummary(t *testing.T) {
	summary := MergeBypass{ForgeActor: "octo", HeadSHA: "0123456789abcdef", RequiredHumanApprovals: 2, MissingHumanApprovals: 1,
		TrustedAuthorApprovalNeeded: true, NonAuthorApprovalNeeded: true, AuthorWaiverNeeded: true, RequiredAgentReviews: 2, MissingAgentReviews: 2}.Summary()
	for _, want := range []string{"GitHub account octo", "head 0123456789ab ", "1 of 2 human approvals", "a trusted author's approval", "an approval from a person who is not an author", "a waiver of the trusted author's approval", "2 of 2 agent reviews"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q lacks %q", summary, want)
		}
	}
	unknown := MergeBypass{ForgeActor: "unknown", HeadSHA: "h", RequiredHumanApprovals: 1, MissingHumanApprovals: 1}.Summary()
	if !strings.HasPrefix(unknown, "An unknown GitHub account merged head h") || !strings.Contains(unknown, "1 of 1 human approval.") {
		t.Fatalf("summary=%q", unknown)
	}
}
