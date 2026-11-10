package nextaction

import (
	"fmt"
	"strings"
)

// MergeBypass is what the inbox acts log names for one merge.bypass_recorded:
// a forge merge of an enforcing task whose risk requirements were unmet at the
// merged head (req-risk-based-review AC-10.3; component-next-action invariant
// 21). The entry needs no judgment, is never flagged no_judgment_needed, and
// adds nothing to attention.
type MergeBypass struct {
	ForgeActor                  string `json:"forge_actor"`
	HeadSHA                     string `json:"head_sha"`
	RequiredHumanApprovals      int    `json:"required_human_approvals"`
	RecordedHumanApprovals      int    `json:"recorded_human_approvals"`
	MissingHumanApprovals       int    `json:"missing_human_approvals"`
	TrustedAuthorApprovalNeeded bool   `json:"trusted_author_approval_needed"`
	NonAuthorApprovalNeeded     bool   `json:"non_author_approval_needed"`
	AuthorWaiverNeeded          bool   `json:"author_waiver_needed"`
	RequiredAgentReviews        int    `json:"required_agent_reviews"`
	RecordedAgentReviews        int    `json:"recorded_agent_reviews"`
	MissingAgentReviews         int    `json:"missing_agent_reviews"`
}

// Summary states who merged which head and what the head still needed, in
// plain words.
func (b MergeBypass) Summary() string {
	actor := strings.TrimSpace(b.ForgeActor)
	if actor == "" || actor == "unknown" {
		actor = "An unknown GitHub account"
	} else {
		actor = "GitHub account " + actor
	}
	head := strings.TrimSpace(b.HeadSHA)
	if len(head) > 12 {
		head = head[:12]
	}
	var missing []string
	if b.MissingHumanApprovals > 0 {
		missing = append(missing, fmt.Sprintf("%d of %d human %s", b.MissingHumanApprovals, max(b.RequiredHumanApprovals, b.MissingHumanApprovals), plural("approval", max(b.RequiredHumanApprovals, b.MissingHumanApprovals))))
	}
	if b.TrustedAuthorApprovalNeeded {
		missing = append(missing, "a trusted author's approval")
	}
	if b.NonAuthorApprovalNeeded {
		missing = append(missing, "an approval from a person who is not an author")
	}
	if b.AuthorWaiverNeeded {
		missing = append(missing, "a waiver of the trusted author's approval")
	}
	if b.MissingAgentReviews > 0 {
		missing = append(missing, fmt.Sprintf("%d of %d agent %s", b.MissingAgentReviews, max(b.RequiredAgentReviews, b.MissingAgentReviews), plural("review", max(b.RequiredAgentReviews, b.MissingAgentReviews))))
	}
	text := fmt.Sprintf("%s merged head %s on GitHub before the risk policy was met.", actor, head)
	if len(missing) > 0 {
		text += " Missing: " + strings.Join(missing, "; ") + "."
	}
	return text
}

func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}
