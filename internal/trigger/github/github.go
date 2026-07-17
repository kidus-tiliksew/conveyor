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
	"errors"
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

const ReviewCheckName = "Conveyor / Code review"

const reviewPublicationMarkerPrefix = "<!-- conveyor:review-publication "
const issueLifecycleMarkerPrefix = "<!-- conveyor:task="
const pullRequestLifecycleMarker = "<!-- conveyor:task-link -->"

var ErrPullRequestNotFound = errors.New("pull request not found")

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
}

type IssuePublication struct {
	Repo              string
	TaskID            string
	Title             string
	ApprovedSpec      string
	SpecVersion       int
	SourceIssueNumber int
}

type IssuePublicationResult struct {
	Number int
	URL    string
	Reused bool
}

// PublishIssue creates or updates the one issue associated with an approved
// task. The remote task marker is checked before creation so a retry after a
// lost local acknowledgement converges instead of duplicating the issue.
func PublishIssue(ctx context.Context, publication IssuePublication) (IssuePublicationResult, error) {
	return publishIssue(ctx, publication, gh)
}

func publishIssue(ctx context.Context, publication IssuePublication, run ghRunner) (IssuePublicationResult, error) {
	marker := issueLifecycleMarkerPrefix + publication.TaskID + " -->"
	number := publication.SourceIssueNumber
	reused := number > 0
	if number == 0 {
		out, err := run(ctx, "issue", "list", "--repo", publication.Repo, "--state", "all", "--limit", "100", "--search", publication.TaskID+" in:body", "--json", "number,url,body")
		if err != nil {
			return IssuePublicationResult{}, fmt.Errorf("find task issue: %w", err)
		}
		var matches []Issue
		if err = json.Unmarshal(out, &matches); err != nil {
			return IssuePublicationResult{}, fmt.Errorf("parse task issue search: %w", err)
		}
		for _, match := range matches {
			if strings.Contains(match.Body, marker) {
				number, reused = match.Number, true
				break
			}
		}
	}
	section := issuePublicationBody(publication)
	if number == 0 {
		out, err := run(ctx, "issue", "create", "--repo", publication.Repo, "--title", publication.Title, "--body", section)
		if err != nil {
			return IssuePublicationResult{}, fmt.Errorf("create task issue: %w", err)
		}
		issueURL := strings.TrimSpace(string(out))
		parts := strings.Split(strings.TrimRight(issueURL, "/"), "/")
		if len(parts) == 0 {
			return IssuePublicationResult{}, fmt.Errorf("parse created issue URL %q", issueURL)
		}
		parsed, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil || parsed <= 0 {
			return IssuePublicationResult{}, fmt.Errorf("parse created issue URL %q", issueURL)
		}
		return IssuePublicationResult{Number: parsed, URL: issueURL}, nil
	}
	view, err := run(ctx, "issue", "view", strconv.Itoa(number), "--repo", publication.Repo, "--json", "number,url,body")
	if err != nil {
		return IssuePublicationResult{}, fmt.Errorf("read associated issue: %w", err)
	}
	var existing Issue
	if err = json.Unmarshal(view, &existing); err != nil || existing.Number != number || existing.URL == "" {
		return IssuePublicationResult{}, fmt.Errorf("parse associated issue %s#%d", publication.Repo, number)
	}
	body := strings.TrimSpace(existing.Body)
	if index := strings.Index(body, issueLifecycleMarkerPrefix); index >= 0 {
		body = strings.TrimSpace(body[:index])
	}
	if body != "" {
		body += "\n\n"
	}
	body += section
	if _, err = run(ctx, "issue", "edit", strconv.Itoa(number), "--repo", publication.Repo, "--body", body); err != nil {
		return IssuePublicationResult{}, fmt.Errorf("update associated issue: %w", err)
	}
	return IssuePublicationResult{Number: number, URL: existing.URL, Reused: reused}, nil
}

func issuePublicationBody(publication IssuePublication) string {
	return fmt.Sprintf("%s%s -->\n## Conveyor approved specification\n\n- Task: `%s`\n- Approved spec version: `%d`\n\n%s", issueLifecycleMarkerPrefix, publication.TaskID, publication.TaskID, publication.SpecVersion, strings.TrimSpace(publication.ApprovedSpec))
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

// PullRequest is the authoritative forge view used by the final merge gate.
// Mergeable mirrors GitHub's MERGEABLE, CONFLICTING, and UNKNOWN values.
type PullRequest struct {
	Number    int
	URL       string
	State     string
	Mergeable string
	Merged    bool
}

// PullRequestForBranch resolves the pull request attached to one assigned
// task branch. The merge gate always reads this before and after a merge
// request; a successful command alone is never treated as merged.
func PullRequestForBranch(ctx context.Context, repo, branch string) (PullRequest, error) {
	return pullRequestForBranch(ctx, repo, branch, gh)
}

func pullRequestForBranch(ctx context.Context, repo, branch string, run ghRunner) (PullRequest, error) {
	out, err := run(ctx, "pr", "view", branch, "--repo", repo, "--json", "number,url,state,mergedAt,mergeable")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no pull requests found") || strings.Contains(strings.ToLower(err.Error()), "could not resolve to a pullrequest") {
			return PullRequest{}, fmt.Errorf("%w for branch %s", ErrPullRequestNotFound, branch)
		}
		return PullRequest{}, fmt.Errorf("view pull request for branch %s: %w", branch, err)
	}
	var view struct {
		Number    int        `json:"number"`
		URL       string     `json:"url"`
		State     string     `json:"state"`
		MergedAt  *time.Time `json:"mergedAt"`
		Mergeable string     `json:"mergeable"`
	}
	if err := json.Unmarshal(out, &view); err != nil || view.Number == 0 {
		return PullRequest{}, fmt.Errorf("parse pull request for branch %s", branch)
	}
	return PullRequest{
		Number: view.Number, URL: view.URL, State: strings.ToLower(view.State),
		Mergeable: strings.ToUpper(view.Mergeable), Merged: view.MergedAt != nil,
	}, nil
}

// MergePullRequest asks GitHub for a normal merge commit. Branch protections
// remain authoritative; Conveyor never forces or bypasses them.
func MergePullRequest(ctx context.Context, repo string, number int) error {
	return mergePullRequest(ctx, repo, number, gh)
}

func mergePullRequest(ctx context.Context, repo string, number int, run ghRunner) error {
	_, err := run(ctx, "pr", "merge", strconv.Itoa(number), "--repo", repo, "--merge")
	if err != nil {
		return fmt.Errorf("merge pull request %s#%d: %w", repo, number, err)
	}
	return nil
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
			if review.SubmittedAt != nil && review.State != "PENDING" && humanFeedback(review.Author.Login, review.Body) {
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
			if (cursor.Since.IsZero() || !comment.CreatedAt.Before(cursor.Since)) && humanFeedback(comment.User.Login, comment.Body) {
				feedback = append(feedback, ReviewFeedback{ID: fmt.Sprintf("comment:%d", comment.ID), Author: comment.User.Login, Body: comment.Body, PR: view.Number})
			}
		}
	}
	page.Feedback = feedback
	return page, nil
}

func humanFeedback(author, body string) bool {
	return strings.TrimSpace(body) != "" && !strings.HasSuffix(author, "[bot]") && !strings.Contains(body, reviewPublicationMarkerPrefix)
}

type ReviewPublication struct {
	Repo                   string
	Branch                 string
	TaskID                 string
	TaskLink               string
	ReviewWorkOrderID      string
	Verdict                string
	ReasonCode             string
	Summary                string
	Feedback               string
	ReviewedCommitSHA      string
	ReviewerModel          string
	ReviewerSession        string
	SameModelAsImplementer string
	ReviewRound            int
	ReviewSeat             int
	RequiredModel          string
	ModelEnforcement       string
	History                []ReviewHistoryItem
	BounceHistory          []string
	SkipComment            bool
}

type ReviewHistoryItem struct {
	WorkOrderID     string
	Round           int
	Seat            int
	Verdict         string
	ReasonCode      string
	Summary         string
	Feedback        string
	ReviewerModel   string
	ResolutionState string
}

type ReviewPublicationResult struct {
	CheckRunID        int64
	CommentID         int64
	ReviewedCommitSHA string
}

func PublishReview(ctx context.Context, publication ReviewPublication) (ReviewPublicationResult, error) {
	return publishReview(ctx, publication, gh)
}

func publishReview(ctx context.Context, publication ReviewPublication, run ghRunner) (ReviewPublicationResult, error) {
	target, err := reviewTarget(ctx, publication.Repo, publication.Branch, run)
	if err != nil {
		return ReviewPublicationResult{}, err
	}
	if publication.ReviewedCommitSHA == "" {
		publication.ReviewedCommitSHA = target.HeadSHA
	}
	conclusion := "success"
	if publication.Verdict == "changes_requested" {
		conclusion = "action_required"
	}
	body := reviewPublicationBody(publication)
	checkID, err := upsertReviewCheck(ctx, publication, conclusion, body, run)
	if err != nil {
		return ReviewPublicationResult{}, err
	}
	commentID := int64(0)
	if publication.SkipComment {
		return ReviewPublicationResult{CheckRunID: checkID, ReviewedCommitSHA: publication.ReviewedCommitSHA}, nil
	}
	commentID, err = upsertReviewComment(ctx, publication.Repo, target.Number, publication.TaskID, body, run)
	if err != nil {
		return ReviewPublicationResult{}, err
	}
	return ReviewPublicationResult{CheckRunID: checkID, CommentID: commentID, ReviewedCommitSHA: publication.ReviewedCommitSHA}, nil
}

type reviewTargetResult struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"headRefOid"`
}

type ReviewTarget struct {
	Number  int
	URL     string
	HeadSHA string
}

func ReviewTargetForBranch(ctx context.Context, repo, branch string) (ReviewTarget, error) {
	target, err := reviewTarget(ctx, repo, branch, gh)
	return ReviewTarget{Number: target.Number, URL: target.URL, HeadSHA: target.HeadSHA}, err
}

func reviewTarget(ctx context.Context, repo, branch string, run ghRunner) (reviewTargetResult, error) {
	out, err := run(ctx, "pr", "view", branch, "--repo", repo, "--json", "number,url,headRefOid")
	if err != nil {
		return reviewTargetResult{}, fmt.Errorf("resolve reviewed PR: %w", err)
	}
	var target reviewTargetResult
	if err := json.Unmarshal(out, &target); err != nil || target.Number == 0 || target.HeadSHA == "" {
		return reviewTargetResult{}, fmt.Errorf("parse reviewed PR target")
	}
	return target, nil
}

func upsertReviewCheck(ctx context.Context, publication ReviewPublication, conclusion, body string, run ghRunner) (int64, error) {
	endpoint := fmt.Sprintf("repos/%s/commits/%s/check-runs?check_name=%s", publication.Repo, publication.ReviewedCommitSHA, url.QueryEscape(ReviewCheckName))
	out, err := run(ctx, "api", endpoint)
	if err != nil {
		return 0, fmt.Errorf("list review checks: %w", err)
	}
	var checks struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			ExternalID string `json:"external_id"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(out, &checks); err != nil {
		return 0, fmt.Errorf("parse review checks: %w", err)
	}
	checkID := int64(0)
	for _, check := range checks.CheckRuns {
		if check.ExternalID == publication.ReviewWorkOrderID {
			checkID = check.ID
			break
		}
	}
	args := []string{"api", "--method"}
	if checkID == 0 {
		args = append(args, "POST", "repos/"+publication.Repo+"/check-runs", "-f", "head_sha="+publication.ReviewedCommitSHA, "-f", "external_id="+publication.ReviewWorkOrderID)
	} else {
		args = append(args, "PATCH", fmt.Sprintf("repos/%s/check-runs/%d", publication.Repo, checkID))
	}
	args = append(args, "-f", "name="+ReviewCheckName, "-f", "status=completed", "-f", "conclusion="+conclusion, "-f", "output[title]="+publication.Verdict, "-f", "output[summary]="+body)
	out, err = run(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("publish review check: %w", err)
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &result); err != nil || result.ID == 0 {
		return 0, fmt.Errorf("parse published review check")
	}
	return result.ID, nil
}

func upsertReviewComment(ctx context.Context, repo string, pr int, taskID, body string, run ghRunner) (int64, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, pr), "--paginate", "--slurp")
	if err != nil {
		return 0, fmt.Errorf("list review comments: %w", err)
	}
	var pages [][]struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return 0, fmt.Errorf("parse review comments: %w", err)
	}
	marker := reviewPublicationMarkerPrefix + "task=" + taskID + " -->"
	commentID := int64(0)
	for _, page := range pages {
		for _, comment := range page {
			if strings.Contains(comment.Body, marker) {
				commentID = comment.ID
				break
			}
		}
	}
	args := []string{"api", "--method", "POST", fmt.Sprintf("repos/%s/issues/%d/comments", repo, pr), "-f", "body=" + body}
	if commentID != 0 {
		args = []string{"api", "--method", "PATCH", fmt.Sprintf("repos/%s/issues/comments/%d", repo, commentID), "-f", "body=" + body}
	}
	out, err = run(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("publish review comment: %w", err)
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &result); err != nil || result.ID == 0 {
		return 0, fmt.Errorf("parse published review comment")
	}
	return result.ID, nil
}

func reviewPublicationBody(publication ReviewPublication) string {
	var body strings.Builder
	fmt.Fprintf(&body, "%stask=%s -->\n## Conveyor factory code review\n\n", reviewPublicationMarkerPrefix, publication.TaskID)
	fmt.Fprintf(&body, "- Task: `%s`", publication.TaskID)
	if publication.TaskLink != "" {
		fmt.Fprintf(&body, " ([link](%s))", publication.TaskLink)
	}
	fmt.Fprintf(&body, "\n- Review work order: `%s`\n- Verdict: **%s**\n- Reason: `%s`\n- Reviewed commit: `%s`\n- Reviewer model: `%s`\n- Independent reviewer session: `%s`\n- Same model as implementer: `%s`\n\n%s", publication.ReviewWorkOrderID, publication.Verdict, publication.ReasonCode, publication.ReviewedCommitSHA, publication.ReviewerModel, publication.ReviewerSession, publication.SameModelAsImplementer, publication.Summary)
	if publication.ReviewRound > 0 {
		fmt.Fprintf(&body, "\n- Review round / seat: `%d / %d`\n- Required model: `%s` (`%s`)", publication.ReviewRound, publication.ReviewSeat, publication.RequiredModel, publication.ModelEnforcement)
	}
	if strings.TrimSpace(publication.Feedback) != "" {
		fmt.Fprintf(&body, "\n\n### Actionable feedback\n\n%s", publication.Feedback)
	}
	if len(publication.BounceHistory) != 0 {
		body.WriteString("\n\n### Bounce history\n")
		for _, item := range publication.BounceHistory {
			fmt.Fprintf(&body, "\n- %s", item)
		}
	}
	if len(publication.History) != 0 {
		body.WriteString("\n\n### Review and resolution history\n")
		for _, item := range publication.History {
			round := fmt.Sprintf("round %d", item.Round)
			if item.Round == 0 {
				round = "single review"
			}
			if item.Seat > 0 {
				round += fmt.Sprintf(", seat %d", item.Seat)
			}
			fmt.Fprintf(&body, "\n- **%s** — %s — `%s` — %s (`%s`)", item.Verdict, round, item.WorkOrderID, item.ResolutionState, item.ReasonCode)
			if strings.TrimSpace(item.Feedback) != "" {
				fmt.Fprintf(&body, "\n  - %s", strings.ReplaceAll(strings.TrimSpace(item.Feedback), "\n", "\n  - "))
			}
		}
	}
	return body.String()
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
		current, viewErr := gh(ctx, "pr", "view", branch, "--repo", repo, "--json", "body", "--jq", ".body")
		if viewErr != nil {
			return "", fmt.Errorf("read existing PR body: %w", viewErr)
		}
		if _, err = gh(ctx, "pr", "edit", branch, "--repo", repo, "--body", reconcilePullRequestBody(string(current), body)); err != nil {
			return "", fmt.Errorf("update existing PR body: %w", err)
		}
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
		current, viewErr := runGH(ctx, "pr", "view", branch, "--repo", repo, "--json", "body", "--jq", ".body")
		if viewErr != nil {
			return "", fmt.Errorf("read existing PR body: %w", viewErr)
		}
		if _, err = runGH(ctx, "pr", "edit", branch, "--repo", repo, "--body", reconcilePullRequestBody(string(current), body)); err != nil {
			return "", fmt.Errorf("update existing PR body: %w", err)
		}
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

func reconcilePullRequestBody(existing, lifecycle string) string {
	existing = strings.TrimSpace(existing)
	lifecycle = strings.TrimSpace(lifecycle)
	if index := strings.Index(existing, pullRequestLifecycleMarker); index >= 0 {
		existing = strings.TrimSpace(existing[:index])
	} else if strings.HasPrefix(existing, "Conveyor task `") && strings.Contains(existing, "\n\nSource: ") {
		existing = ""
	}
	if existing == "" {
		return lifecycle
	}
	return existing + "\n\n" + lifecycle
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
