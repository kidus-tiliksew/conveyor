package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
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
}

// policyStatusMaxPages bounds the combined-status walk. The combined status
// lists one entry per context, 100 to a page, so 20 pages hold 2,000
// distinct contexts on one commit, far beyond what a repository's checks
// publish. Reaching the bound fails the read, and so the publication, under
// the forge response category; it never counts as an absent status.
const policyStatusMaxPages = 20

// latestCommitStatus reads the latest status of one context on a commit from
// GitHub's combined status, which returns the most recent status of each
// context, 100 contexts to a page. The runner follows every Link page up to
// policyStatusMaxPages, so a context on a later page is found, and a failed
// page read fails the whole read; absence is reported only after every page
// was read. If an answer ever held several entries of the context, the
// highest status ID decides, so an older matching entry never stands in for a
// newer different one.
func latestCommitStatus(ctx context.Context, repo, sha, statusContext string, run ghRunner) (commitStatus, bool, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/commits/%s/status?per_page=100", repo, url.PathEscape(sha)), "--paginate", "--slurp", "--max-pages", strconv.Itoa(policyStatusMaxPages))
	if err != nil {
		return commitStatus{}, false, fmt.Errorf("read policy status: %w", forgeCallError(err))
	}
	var pages []struct {
		Statuses *[]commitStatus `json:"statuses"`
	}
	if err := json.Unmarshal(out, &pages); err != nil || len(pages) == 0 {
		return commitStatus{}, false, forgeResponseError("parse policy status of %s", sha)
	}
	var latest commitStatus
	found := false
	for _, page := range pages {
		if page.Statuses == nil {
			return commitStatus{}, false, forgeResponseError("parse policy status of %s: missing statuses", sha)
		}
		for _, status := range *page.Statuses {
			if status.Context != statusContext {
				continue
			}
			if !found || status.ID > latest.ID {
				latest, found = status, true
			}
		}
	}
	return latest, found, nil
}
