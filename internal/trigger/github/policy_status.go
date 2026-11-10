package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PolicyStatusContext is the commit-status context of an enforcing task's
// risk requirements (req-risk-based-review AC-10.1, AC-10.2;
// component-git-delivery invariant 37; feature-risk-based-review RR-9). A
// repository owner's branch protection can require it. It is Conveyor's own
// published status, so the check-status read skips it as it skips
// ReviewStatusContext (component-submission-merge invariant 59).
const PolicyStatusContext = "conveyor/review-policy"

// Policy status states. GitHub's commit-status API names them the same way.
const (
	PolicyStatusPending = "pending"
	PolicyStatusSuccess = "success"
)

// PolicyStatusDescription is the fixed description of each policy status
// state. The read-then-post comparison matches it exactly, so a repeat
// publication writes nothing.
func PolicyStatusDescription(state string) string {
	switch state {
	case PolicyStatusSuccess:
		return "The risk policy's agent reviews and human approvals are complete"
	case PolicyStatusPending:
		return "Waiting for the agent reviews and human approvals the risk policy requires"
	}
	return ""
}

// PolicyStatusPublication is one conveyor/review-policy write: the state for
// the exact head, on the task branch's pull request.
type PolicyStatusPublication struct {
	Repo    string
	Branch  string
	HeadSHA string
	State   string
}

// PolicyStatusResult reports whether the publication wrote a status. Written
// is false when the latest status of the context on the head already held the
// same state, description, and target URL.
type PolicyStatusResult struct {
	Written   bool
	TargetURL string
}

// PublishPolicyStatus upserts the conveyor/review-policy commit status on the
// publication's head through the workspace App credential in ctx. It writes
// no comment (component-git-delivery invariant 37).
func PublishPolicyStatus(ctx context.Context, publication PolicyStatusPublication) (PolicyStatusResult, error) {
	return publishPolicyStatus(ctx, publication, gh)
}

func publishPolicyStatus(ctx context.Context, publication PolicyStatusPublication, run ghRunner) (PolicyStatusResult, error) {
	description := PolicyStatusDescription(publication.State)
	if description == "" {
		return PolicyStatusResult{}, fmt.Errorf("publish policy status: invalid state %q", publication.State)
	}
	if strings.TrimSpace(publication.Repo) == "" || strings.TrimSpace(publication.HeadSHA) == "" {
		return PolicyStatusResult{}, fmt.Errorf("publish policy status: repository and head are required")
	}
	target, err := reviewTarget(ctx, publication.Repo, publication.Branch, run)
	if err != nil {
		return PolicyStatusResult{}, err
	}
	latest, found, err := latestCommitStatus(ctx, publication.Repo, publication.HeadSHA, PolicyStatusContext, run)
	if err != nil {
		return PolicyStatusResult{}, err
	}
	if found && latest.State == publication.State && latest.Description == description && latest.TargetURL == target.URL {
		return PolicyStatusResult{TargetURL: target.URL}, nil
	}
	_, err = run(ctx, "api", "--method", "POST", "repos/"+publication.Repo+"/statuses/"+publication.HeadSHA,
		"-f", "state="+publication.State, "-f", "context="+PolicyStatusContext, "-f", "description="+description, "-f", "target_url="+target.URL)
	if err != nil {
		return PolicyStatusResult{}, fmt.Errorf("publish policy status: %w", forgeCallError(err))
	}
	return PolicyStatusResult{Written: true, TargetURL: target.URL}, nil
}

type commitStatus struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
	UpdatedAt   string `json:"updated_at"`
}

// latestCommitStatus reads the head's statuses and returns the latest status
// of one context. GitHub lists a commit's statuses newest first; the highest
// status ID decides when an answer holds several entries of the context, so
// an older matching entry never stands in for a newer different one.
func latestCommitStatus(ctx context.Context, repo, sha, statusContext string, run ghRunner) (commitStatus, bool, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/commits/%s/statuses?per_page=100", repo, sha))
	if err != nil {
		return commitStatus{}, false, fmt.Errorf("read policy status: %w", forgeCallError(err))
	}
	var statuses []commitStatus
	if err := json.Unmarshal(out, &statuses); err != nil || statuses == nil {
		return commitStatus{}, false, forgeResponseError("parse policy status: %v", err)
	}
	var latest commitStatus
	found := false
	for index, status := range statuses {
		if status.Context != statusContext {
			continue
		}
		if !found || status.ID > latest.ID || (status.ID == latest.ID && index == 0) {
			latest, found = status, true
		}
	}
	return latest, found, nil
}
