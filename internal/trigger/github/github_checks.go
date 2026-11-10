package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// The check-status read of one commit (req-review-gates-evidence AC-1.4;
// component-submission-merge, Check status; DEC-108(5)). A check is a check
// run or commit status GitHub reports for the commit at its latest attempt.
// GitHub documents both endpoints in its REST reference: "List check runs for
// a Git reference" takes filter=latest and "Get the combined status for a
// specific reference" returns the latest status of each context. Each pages
// at 100 through the Link header.

// Check states a read classifies. A completed check run concluding success,
// neutral, or skipped and a status in state success are passed.
const (
	CheckPassed  = "passed"
	CheckFailed  = "failed"
	CheckPending = "pending"
)

// Check is one check run or commit status as an operator needs it named.
type Check struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// URL is the check's page on GitHub, the operator's way to rerun it. It
	// is empty when GitHub reports none or reports one that is not an
	// absolute HTTP(S) URL without credentials.
	URL string `json:"url,omitempty"`
}

// CheckStatus is what one read found for one commit, in name order.
type CheckStatus struct {
	Checks []Check
}

// Named returns the checks in the given state.
func (s CheckStatus) Named(state string) []Check {
	var out []Check
	for _, check := range s.Checks {
		if check.State == state {
			out = append(out, check)
		}
	}
	return out
}

// CommitChecks reads every check of a commit under the credential in ctx.
func CommitChecks(ctx context.Context, repo, sha string) (CheckStatus, error) {
	return commitChecks(ctx, repo, sha, gh)
}

func commitChecks(ctx context.Context, repo, sha string, run ghRunner) (CheckStatus, error) {
	if strings.Count(repo, "/") != 1 || strings.TrimSpace(sha) == "" {
		return CheckStatus{}, &Error{Category: ForgeRequest, Err: fmt.Errorf("invalid GitHub repository or commit for a check-status read")}
	}
	runs, err := commitCheckRuns(ctx, repo, sha, run)
	if err != nil {
		return CheckStatus{}, err
	}
	statuses, err := commitStatuses(ctx, repo, sha, run)
	if err != nil {
		return CheckStatus{}, err
	}
	checks := append(runs, statuses...)
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].Name != checks[j].Name {
			return checks[i].Name < checks[j].Name
		}
		if checks[i].State != checks[j].State {
			return checks[i].State < checks[j].State
		}
		return checks[i].URL < checks[j].URL
	})
	return CheckStatus{Checks: checks}, nil
}

func commitCheckRuns(ctx context.Context, repo, sha string, run ghRunner) ([]Check, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/commits/%s/check-runs?filter=latest&per_page=100", repo, url.PathEscape(sha)), "--paginate", "--slurp")
	if err != nil {
		return nil, fmt.Errorf("read check runs of %s: %w", sha, forgeCallError(err))
	}
	var pages []struct {
		CheckRuns *[]struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
			DetailsURL string `json:"details_url"`
		} `json:"check_runs"`
	}
	if json.Unmarshal(out, &pages) != nil || len(pages) == 0 {
		return nil, forgeResponseError("parse check runs of %s", sha)
	}
	var checks []Check
	for _, page := range pages {
		// A page without the array is not "none reported"; only a real empty
		// array may let a head merge.
		if page.CheckRuns == nil {
			return nil, forgeResponseError("parse check runs of %s", sha)
		}
		for _, item := range *page.CheckRuns {
			link := checkURL(item.HTMLURL)
			if link == "" {
				link = checkURL(item.DetailsURL)
			}
			checks = append(checks, Check{Name: item.Name, State: checkRunState(item.Status, item.Conclusion), URL: link})
		}
	}
	return checks, nil
}

func commitStatuses(ctx context.Context, repo, sha string, run ghRunner) ([]Check, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/commits/%s/status?per_page=100", repo, url.PathEscape(sha)), "--paginate", "--slurp")
	if err != nil {
		return nil, fmt.Errorf("read commit statuses of %s: %w", sha, forgeCallError(err))
	}
	var pages []struct {
		Statuses *[]struct {
			Context   string `json:"context"`
			State     string `json:"state"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	if json.Unmarshal(out, &pages) != nil || len(pages) == 0 {
		return nil, forgeResponseError("parse commit statuses of %s", sha)
	}
	var checks []Check
	for _, page := range pages {
		if page.Statuses == nil {
			return nil, forgeResponseError("parse commit statuses of %s", sha)
		}
		for _, item := range *page.Statuses {
			// Conveyor's own published review and risk policy statuses are
			// not checks (component-submission-merge invariant 59;
			// req-review-gates-evidence AC-1.4), so the policy status never
			// holds an automatic merge.
			if item.Context == ReviewStatusContext || item.Context == PolicyStatusContext {
				continue
			}
			checks = append(checks, Check{Name: item.Context, State: commitStatusState(item.State), URL: checkURL(item.TargetURL)})
		}
	}
	return checks, nil
}

// checkRunState classifies a check run. Any status but completed is pending,
// and a completed run with any conclusion but success, neutral, or skipped is
// failed, which covers failure, cancelled, timed_out, action_required, stale,
// and a conclusion GitHub adds later.
func checkRunState(status, conclusion string) string {
	if status != "completed" {
		return CheckPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return CheckPassed
	default:
		return CheckFailed
	}
}

// commitStatusState classifies a commit status: success passes, pending
// waits, and error, failure, or a state GitHub adds later fails.
func commitStatusState(state string) string {
	switch state {
	case "success":
		return CheckPassed
	case "pending":
		return CheckPending
	default:
		return CheckFailed
	}
}

// checkURL keeps an absolute HTTP(S) URL without credentials. A status's
// target URL is whatever its writer sent, and surfaces render it as a link.
func checkURL(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return raw
}
