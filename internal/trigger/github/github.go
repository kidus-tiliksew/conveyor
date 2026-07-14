// Package github implements the Phase 1 trigger and output: issues
// labeled conveyor:ready become tasks, and completed tasks open PRs
// (spec §9, §19 Phase 1: "GitHub issue → PR").
//
// Phase 1 shells out to the gh CLI (already authenticated on the
// user's machine); webhook ingestion arrives with the HTTP API in
// later phases.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ReadyLabel dispatches an issue into the factory (spec §9).
const ReadyLabel = "conveyor:ready"

// DispatchedLabel is the durable claim marker for a GitHub issue. Moving
// an issue from ReadyLabel to DispatchedLabel prevents a conveyord restart
// from replaying it after the Phase 1 in-memory store is lost.
const DispatchedLabel = "conveyor:dispatched"

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
}

type ReviewFeedback struct {
	ID     string
	Author string
	Body   string
	PR     int
}

type ReviewCursor struct {
	Since       time.Time
	ReviewAfter string
}

type ReviewFeedbackPage struct {
	PR       int
	State    string
	Feedback []ReviewFeedback
	Cursor   ReviewCursor
}

// ListReviewFeedback returns human-authored PR review bodies and inline
// comments for the task branch. The dispatcher deduplicates IDs in its event
// log before converting them to redirect interventions (spec §9).
func ListReviewFeedback(ctx context.Context, repo, branch string, cursor ReviewCursor) (ReviewFeedbackPage, error) {
	return listReviewFeedback(ctx, repo, branch, cursor, gh)
}

func listReviewFeedback(ctx context.Context, repo, branch string, cursor ReviewCursor, run ghRunner) (ReviewFeedbackPage, error) {
	out, err := run(ctx, "pr", "view", branch, "--repo", repo, "--json", "number,state,mergedAt")
	if err != nil {
		return ReviewFeedbackPage{}, err
	}
	var view struct {
		Number   int        `json:"number"`
		State    string     `json:"state"`
		MergedAt *time.Time `json:"mergedAt"`
	}
	if err := json.Unmarshal(out, &view); err != nil {
		return ReviewFeedbackPage{}, fmt.Errorf("parse gh pr view: %w", err)
	}
	page := ReviewFeedbackPage{PR: view.Number, State: strings.ToLower(view.State), Cursor: cursor}
	if view.MergedAt != nil {
		page.State = "merged"
	}
	if page.State != "open" {
		return page, nil
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ReviewFeedbackPage{}, fmt.Errorf("invalid GitHub repository slug %q", repo)
	}
	const reviewsQuery = `query($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviews(first:100,after:$after){nodes{id body state submittedAt author{login}}pageInfo{hasNextPage endCursor}}}}}`
	reviewAfter := cursor.ReviewAfter
	feedback := []ReviewFeedback{}
	for {
		args := []string{"api", "graphql", "-f", "query=" + reviewsQuery, "-F", "owner=" + owner, "-F", "name=" + name, "-F", "number=" + strconv.Itoa(view.Number)}
		if reviewAfter != "" {
			args = append(args, "-f", "after="+reviewAfter)
		}
		reviewsOut, err := run(ctx, args...)
		if err != nil {
			return ReviewFeedbackPage{}, err
		}
		var reviews struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						Reviews struct {
							Nodes []struct {
								ID          string     `json:"id"`
								Body        string     `json:"body"`
								State       string     `json:"state"`
								SubmittedAt *time.Time `json:"submittedAt"`
								Author      struct {
									Login string `json:"login"`
								} `json:"author"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviews"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(reviewsOut, &reviews); err != nil {
			return ReviewFeedbackPage{}, fmt.Errorf("parse gh review page: %w", err)
		}
		connection := reviews.Data.Repository.PullRequest.Reviews
		for _, review := range connection.Nodes {
			if review.SubmittedAt != nil && review.State != "PENDING" && strings.TrimSpace(review.Body) != "" && !strings.HasSuffix(review.Author.Login, "[bot]") {
				feedback = append(feedback, ReviewFeedback{ID: "review:" + review.ID, Author: review.Author.Login, Body: review.Body, PR: view.Number})
			}
		}
		if connection.PageInfo.EndCursor != "" {
			reviewAfter = connection.PageInfo.EndCursor
			page.Cursor.ReviewAfter = reviewAfter
		}
		if !connection.PageInfo.HasNextPage {
			break
		}
	}
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", repo, view.Number)
	if !cursor.Since.IsZero() {
		endpoint += "&since=" + url.QueryEscape(cursor.Since.UTC().Format(time.RFC3339Nano))
	}
	inline, err := run(ctx, "api", endpoint, "--paginate", "--slurp")
	if err != nil {
		return ReviewFeedbackPage{}, err
	}
	type reviewComment struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	var commentPages [][]reviewComment
	if err := json.Unmarshal(inline, &commentPages); err != nil {
		return ReviewFeedbackPage{}, fmt.Errorf("parse gh review comments: %w", err)
	}
	for _, comments := range commentPages {
		for _, comment := range comments {
			if (cursor.Since.IsZero() || !comment.CreatedAt.Before(cursor.Since)) && strings.TrimSpace(comment.Body) != "" && !strings.HasSuffix(comment.User.Login, "[bot]") {
				feedback = append(feedback, ReviewFeedback{ID: fmt.Sprintf("comment:%d", comment.ID), Author: comment.User.Login, Body: comment.Body, PR: view.Number})
			}
		}
	}
	page.Feedback = feedback
	return page, nil
}

// ListReadyIssues polls a repo for issues carrying the ready label.
func ListReadyIssues(ctx context.Context, repo string) ([]Issue, error) {
	out, err := gh(ctx, "issue", "list",
		"--repo", repo,
		"--label", ReadyLabel,
		"--state", "open",
		"--json", "number,title,body,url")
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parse gh issue list: %w", err)
	}
	return issues, nil
}

// MarkIssueDispatched durably claims an issue before it is enqueued. The
// label is maintained by Conveyor so scratch repositories need no manual
// label setup beyond conveyor:ready.
func MarkIssueDispatched(ctx context.Context, repo string, number int, taskID string) error {
	return markIssueDispatched(ctx, repo, number, taskID, gh)
}

type ghRunner func(context.Context, ...string) ([]byte, error)

func markIssueDispatched(ctx context.Context, repo string, number int, taskID string, run ghRunner) error {
	if _, err := run(ctx, "label", "create", DispatchedLabel,
		"--repo", repo,
		"--color", "1D76DB",
		"--description", "Claimed by Conveyor for dispatch",
		"--force"); err != nil {
		return fmt.Errorf("ensure %s label: %w", DispatchedLabel, err)
	}
	if _, err := run(ctx, "issue", "edit", strconv.Itoa(number),
		"--repo", repo,
		"--remove-label", ReadyLabel,
		"--add-label", DispatchedLabel); err != nil {
		return fmt.Errorf("move issue labels: %w", err)
	}
	_, _ = run(ctx, "issue", "comment", strconv.Itoa(number),
		"--repo", repo,
		"--body", fmt.Sprintf("<!-- conveyor:dispatched -->\nConveyor accepted this issue as task `%s`.", taskID))
	// The label transition is the deduplication boundary. The comment is
	// audit-friendly, but its failure must not suppress dispatch after the
	// ready label has already been removed.
	return nil
}

// OpenPR pushes the task branch and opens a PR against base. The PR
// body records provenance (task ID, source issue) for the audit chain.
func OpenPR(ctx context.Context, worktreeDir, repo, branch, base, title, body string) (string, error) {
	return openPR(ctx, worktreeDir, repo, branch, base, title, body, run, gh)
}

// OpenPRForBranch trusts the operator-owned agent to have pushed branch. It
// creates or reuses the PR without requiring Conveyor to own a worktree
// (spec §21.4 change 5, amended by §21.7).
func OpenPRForBranch(ctx context.Context, repo, branch, base, title, body string) (string, error) {
	existing, err := gh(ctx, "pr", "list", "--repo", repo, "--head", branch, "--state", "open", "--json", "url", "--jq", ".[0].url")
	if err != nil {
		return "", fmt.Errorf("find existing PR: %w", err)
	}
	if value := strings.TrimSpace(string(existing)); value != "" {
		return value, nil
	}
	out, err := gh(ctx, "pr", "create", "--repo", repo, "--head", branch, "--base", base, "--title", title, "--body", body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func DiffForBranch(ctx context.Context, repo, branch string) (string, error) {
	out, err := gh(ctx, "pr", "diff", branch, "--repo", repo)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

type gitRunner func(context.Context, string, string, ...string) error

func openPR(ctx context.Context, worktreeDir, repo, branch, base, title, body string, runGit gitRunner, runGH ghRunner) (string, error) {
	if err := runGit(ctx, worktreeDir, "git", "push", "--set-upstream", "origin", branch); err != nil {
		return "", err
	}
	// Redirect/re-dispatch pushes new commits to the existing task branch.
	// Reuse its open PR rather than failing `gh pr create` on the second job.
	existing, err := runGH(ctx, "pr", "list",
		"--repo", repo,
		"--head", branch,
		"--state", "open",
		"--json", "url",
		"--jq", ".[0].url")
	if err != nil {
		return "", fmt.Errorf("find existing PR: %w", err)
	}
	if url := strings.TrimSpace(string(existing)); url != "" {
		return url, nil
	}
	out, err := runGH(ctx, "pr", "create",
		"--repo", repo,
		"--head", branch,
		"--base", base,
		"--title", title,
		"--body", body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil // gh prints the PR URL
}

func gh(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, ee.Stderr)
		}
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}
