package core

import (
	"fmt"
	"strings"
	"time"
)

// Risk policy status publication (req-risk-based-review AC-10.1, AC-10.2;
// component-git-delivery invariant 37; feature-risk-based-review RR-9). An
// enforcing task holds at most one row per head. The row records the state
// the head's requirements last called for (Desired) and the state GitHub last
// accepted for the conveyor/review-policy context (Published). The handler
// re-reads the requirements before every forge call, so Desired is a hint for
// recovery and audit, never the authority for a write.

// ReviewPolicyStatusState is a publication row's lifecycle state.
type ReviewPolicyStatusState string

const (
	ReviewPolicyStatusQueued    ReviewPolicyStatusState = "queued"
	ReviewPolicyStatusRetrying  ReviewPolicyStatusState = "retrying"
	ReviewPolicyStatusPublished ReviewPolicyStatusState = "published"
	ReviewPolicyStatusFailed    ReviewPolicyStatusState = "failed"
)

// Policy status values, the GitHub commit-status states the context uses.
const (
	PolicyStatusPending = "pending"
	PolicyStatusSuccess = "success"
)

// Policy status events. Each carries the row as its payload.
const (
	ReviewPolicyStatusQueuedEvent    = "review_policy_status.publication_queued"
	ReviewPolicyStatusRetryEvent     = "review_policy_status.publication_retry"
	ReviewPolicyStatusPublishedEvent = "review_policy_status.publication_published"
	ReviewPolicyStatusFailedEvent    = "review_policy_status.publication_failed"
)

// ReviewPolicyStatus is one head's policy status publication.
type ReviewPolicyStatus struct {
	TaskID  string `json:"task_id"`
	HeadSHA string `json:"head_sha"`
	// Desired is the state the head's requirements called for when the row
	// was last queued or attempted.
	Desired string `json:"desired"`
	// Published is the state GitHub last accepted, or empty.
	Published          string                  `json:"published,omitempty"`
	State              ReviewPolicyStatusState `json:"state"`
	Attempts           int                     `json:"attempts"`
	ForgeErrorCategory string                  `json:"forge_error_category,omitempty"`
	LastError          string                  `json:"last_error,omitempty"`
	// Revision counts the row's writes from 1, so a backend can wait until
	// its latest write is visible.
	Revision  int       `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ValidPolicyStatus reports whether value is a policy status state.
func ValidPolicyStatus(value string) bool {
	return value == PolicyStatusPending || value == PolicyStatusSuccess
}

// Validate checks a row's identity and values.
func (s ReviewPolicyStatus) Validate() error {
	if strings.TrimSpace(s.TaskID) == "" || strings.TrimSpace(s.HeadSHA) == "" {
		return fmt.Errorf("policy status needs a task and a head")
	}
	if !ValidPolicyStatus(s.Desired) {
		return fmt.Errorf("policy status desired state %q is not pending or success", s.Desired)
	}
	if s.Published != "" && !ValidPolicyStatus(s.Published) {
		return fmt.Errorf("policy status published state %q is not pending or success", s.Published)
	}
	switch s.State {
	case ReviewPolicyStatusQueued, ReviewPolicyStatusRetrying, ReviewPolicyStatusFailed:
	case ReviewPolicyStatusPublished:
		if s.Published == "" {
			return fmt.Errorf("a published policy status names the state GitHub accepted")
		}
	default:
		return fmt.Errorf("policy status state %q is unknown", s.State)
	}
	return nil
}

// reviewPolicyStatusTransitions is the row lifecycle. A queue that changes
// the desired state moves any row to queued; an attempt moves a queued,
// failed, or published row to retrying; a synchronous merge publication moves
// a queued row straight to published.
var reviewPolicyStatusTransitions = map[ReviewPolicyStatusState][]ReviewPolicyStatusState{
	ReviewPolicyStatusQueued:    {ReviewPolicyStatusQueued, ReviewPolicyStatusRetrying, ReviewPolicyStatusPublished, ReviewPolicyStatusFailed},
	ReviewPolicyStatusRetrying:  {ReviewPolicyStatusQueued, ReviewPolicyStatusRetrying, ReviewPolicyStatusPublished, ReviewPolicyStatusFailed},
	ReviewPolicyStatusPublished: {ReviewPolicyStatusQueued, ReviewPolicyStatusRetrying, ReviewPolicyStatusPublished},
	ReviewPolicyStatusFailed:    {ReviewPolicyStatusQueued, ReviewPolicyStatusRetrying},
}

// TransitionReviewPolicyStatus checks one row transition.
func TransitionReviewPolicyStatus(from, to ReviewPolicyStatusState) error {
	for _, allowed := range reviewPolicyStatusTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("policy status cannot move from %q to %q", from, to)
}

// ReviewPolicyStatusEventKind is the event an update to state appends.
func ReviewPolicyStatusEventKind(state ReviewPolicyStatusState) string {
	switch state {
	case ReviewPolicyStatusQueued:
		return ReviewPolicyStatusQueuedEvent
	case ReviewPolicyStatusPublished:
		return ReviewPolicyStatusPublishedEvent
	case ReviewPolicyStatusFailed:
		return ReviewPolicyStatusFailedEvent
	}
	return ReviewPolicyStatusRetryEvent
}

// MergeBypassRecordedEvent records a forge merge of an enforcing task whose
// risk requirements were unmet at the merged head (req-risk-based-review
// AC-10.3; component-submission-merge invariant 66).
const MergeBypassRecordedEvent = "merge.bypass_recorded"

// UnknownForgeActor names a bypass whose merging GitHub account is unknown.
const UnknownForgeActor = "unknown"

// MergeBypass is the payload of merge.bypass_recorded. The completing
// transaction appends it once per task and head.
type MergeBypass struct {
	Repository  string `json:"repository,omitempty"`
	PullRequest int    `json:"pull_request,omitempty"`
	URL         string `json:"url,omitempty"`
	// ForgeActor is the GitHub login that merged, or UnknownForgeActor.
	ForgeActor     string `json:"forge_actor"`
	HeadSHA        string `json:"head_sha"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	Tier           string `json:"risk_tier,omitempty"`
	// RequiredHumanApprovals and RecordedHumanApprovals are the distinct
	// eligible people the head required and had.
	RequiredHumanApprovals int `json:"required_human_approvals"`
	RecordedHumanApprovals int `json:"recorded_human_approvals"`
	// MissingHumanApprovals is the number of further approvals the head
	// needed, author conditions included.
	MissingHumanApprovals       int  `json:"missing_human_approvals"`
	TrustedAuthorApprovalNeeded bool `json:"trusted_author_approval_needed"`
	NonAuthorApprovalNeeded     bool `json:"non_author_approval_needed"`
	AuthorWaiverNeeded          bool `json:"author_waiver_needed"`
	// RequiredAgentReviews and RecordedAgentReviews are the approving agent
	// reviews the head required and had.
	RequiredAgentReviews int `json:"required_agent_reviews"`
	RecordedAgentReviews int `json:"recorded_agent_reviews"`
	MissingAgentReviews  int `json:"missing_agent_reviews"`
}

// Validate checks a bypass record before a backend appends it.
func (b MergeBypass) Validate() error {
	if strings.TrimSpace(b.HeadSHA) == "" {
		return fmt.Errorf("a merge bypass names the merged head")
	}
	if strings.TrimSpace(b.ForgeActor) == "" {
		return fmt.Errorf("a merge bypass names the forge actor or %q", UnknownForgeActor)
	}
	if b.MissingHumanApprovals < 0 || b.MissingAgentReviews < 0 || b.RequiredHumanApprovals < 0 || b.RecordedHumanApprovals < 0 || b.RequiredAgentReviews < 0 || b.RecordedAgentReviews < 0 {
		return fmt.Errorf("a merge bypass holds no negative count")
	}
	if b.MissingHumanApprovals == 0 && b.MissingAgentReviews == 0 && !b.TrustedAuthorApprovalNeeded && !b.NonAuthorApprovalNeeded && !b.AuthorWaiverNeeded {
		return fmt.Errorf("a merge bypass names at least one missing requirement")
	}
	return nil
}
