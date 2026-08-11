// Package github implements the Phase 1 trigger and output: issues
// labeled conveyor:ready become tasks, and completed tasks open PRs
// (DEC-8; design-git-delivery).
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ReadyLabel dispatches an issue into the factory (DEC-8).
const ReadyLabel = "conveyor:ready"

// DispatchedLabel is the durable claim marker for a GitHub issue. Moving
// an issue from ReadyLabel to DispatchedLabel prevents a conveyord restart
// from replaying it after the Phase 1 in-memory store is lost.
const DispatchedLabel = "conveyor:dispatched"

// ReviewStatusContext is the portable commit-status context used for the
// aggregate review result. Unlike Check Runs, commit statuses can be written
// by the user-owned credentials already required for GitHub coordination
// (design-git-delivery).
const ReviewStatusContext = "Conveyor / Code review"

const reviewPublicationMarkerPrefix = "<!-- conveyor:review-publication "
const issueLifecycleMarkerPrefix = "<!-- conveyor:task="
const pullRequestLifecycleMarker = "<!-- conveyor:task-link -->"
const pullRequestLifecycleEndMarker = "<!-- conveyor:lifecycle-end -->"
const verificationEvidenceMarker = "<!-- conveyor:verification-evidence -->"
const verificationEvidenceFooter = "Evidence media remains in Conveyor's task-scoped artifact store. This PR mirror intentionally publishes durable metadata only—no control-plane credentials or private artifact URLs."

var legacyPullRequestLifecyclePattern = regexp.MustCompile(`^<!-- conveyor:task-link -->\r?\nConveyor task \x60[^\r\n]*\x60\r?\n\r?\nSource: [^\r\n]*(?:\r?\n\r?\nCloses #[0-9]+)?`)

var (
	ErrPullRequestNotFound        = errors.New("pull request not found")
	ErrIssueReconciliationPending = errors.New("GitHub issue reconciliation pending")
)

// ForgeErrorCategory is the stable GitHub failure taxonomy recorded in
// operator evidence (design-git-delivery). It deliberately remains local to
// the one supported forge instead of introducing a provider abstraction.
type ForgeErrorCategory string

const (
	ForgeRequest     ForgeErrorCategory = "forge_request"
	ForgeStatus      ForgeErrorCategory = "forge_status"
	ForgeResponse    ForgeErrorCategory = "forge_response"
	ForgeRateLimited ForgeErrorCategory = "forge_rate_limited"
	ForgePermission  ForgeErrorCategory = "forge_permission"
)

// Error carries a stable category while preserving the underlying GitHub
// failure detail and errors.Is/errors.As behavior.
type Error struct {
	Category ForgeErrorCategory
	Err      error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// ErrorCategory returns the stable category carried by a GitHub boundary
// failure. Empty means the error did not originate from a forge call.
func ErrorCategory(err error) ForgeErrorCategory {
	var categorized *Error
	if errors.As(err, &categorized) {
		return categorized.Category
	}
	return ""
}

// CategorizeError applies the stable one-forge taxonomy to a new GitHub
// boundary such as the Phase 5.6 monitor.
func CategorizeError(err error) error { return forgeCallError(err) }

func forgeCallError(err error) error {
	if err == nil || ErrorCategory(err) != "" {
		return err
	}
	detail := strings.ToLower(err.Error())
	category := ForgeRequest
	switch {
	case strings.Contains(detail, "rate limit"),
		strings.Contains(detail, "rate-limit"),
		strings.Contains(detail, "http 429"),
		strings.Contains(detail, "status 429"):
		category = ForgeRateLimited
	case strings.Contains(detail, "bad credentials"),
		strings.Contains(detail, "authentication"),
		strings.Contains(detail, "not authorized"),
		strings.Contains(detail, "unauthorized"),
		strings.Contains(detail, "permission"),
		strings.Contains(detail, "resource not accessible"),
		strings.Contains(detail, "http 401"),
		strings.Contains(detail, "status 401"),
		strings.Contains(detail, "http 403"),
		strings.Contains(detail, "status 403"):
		category = ForgePermission
	case strings.Contains(detail, "http 4"),
		strings.Contains(detail, "http 5"),
		strings.Contains(detail, "status 4"),
		strings.Contains(detail, "status 5"),
		strings.Contains(detail, "exit status"):
		category = ForgeStatus
	}
	return &Error{Category: category, Err: err}
}

func forgeResponseError(format string, args ...any) error {
	return &Error{Category: ForgeResponse, Err: fmt.Errorf(format, args...)}
}

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
	AllowCreate       bool
	BeforeCreate      func(context.Context) error
}

type IssuePublicationResult struct {
	Number int
	URL    string
	Reused bool
}

// PublishIssue creates or updates the one issue associated with an approved
// task. The remote task marker is checked exhaustively before creation. Once
// BeforeCreate durably records an ambiguous create window, callers must retry
// with AllowCreate false until the exact marker is visible.
func PublishIssue(ctx context.Context, publication IssuePublication) (IssuePublicationResult, error) {
	return publishIssue(ctx, publication, gh)
}

func publishIssue(ctx context.Context, publication IssuePublication, run ghRunner) (IssuePublicationResult, error) {
	marker := issueLifecycleMarkerPrefix + publication.TaskID + " -->"
	number := publication.SourceIssueNumber
	reused := number > 0
	if number == 0 {
		var err error
		number, err = findIssueByMarker(ctx, publication.Repo, marker, run)
		if err != nil {
			return IssuePublicationResult{}, err
		}
		reused = number > 0
	}
	section := issuePublicationBody(publication)
	if number == 0 {
		if !publication.AllowCreate {
			return IssuePublicationResult{}, fmt.Errorf("%w for task %s", ErrIssueReconciliationPending, publication.TaskID)
		}
		if publication.BeforeCreate == nil {
			return IssuePublicationResult{}, errors.New("GitHub issue creation requires a durable create-attempt recorder")
		}
		if err := publication.BeforeCreate(ctx); err != nil {
			return IssuePublicationResult{}, fmt.Errorf("record GitHub issue create attempt: %w", err)
		}
		out, err := run(ctx, "issue", "create", "--repo", publication.Repo, "--title", publication.Title, "--body", section)
		if err != nil {
			return IssuePublicationResult{}, fmt.Errorf("create task issue: %w", forgeCallError(err))
		}
		issueURL := strings.TrimSpace(string(out))
		parts := strings.Split(strings.TrimRight(issueURL, "/"), "/")
		if len(parts) == 0 {
			return IssuePublicationResult{}, forgeResponseError("parse created issue URL %q", issueURL)
		}
		parsed, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil || parsed <= 0 {
			return IssuePublicationResult{}, forgeResponseError("parse created issue URL %q", issueURL)
		}
		return IssuePublicationResult{Number: parsed, URL: issueURL}, nil
	}
	view, err := run(ctx, "issue", "view", strconv.Itoa(number), "--repo", publication.Repo, "--json", "number,url,body")
	if err != nil {
		return IssuePublicationResult{}, fmt.Errorf("read associated issue: %w", forgeCallError(err))
	}
	var existing Issue
	if err = json.Unmarshal(view, &existing); err != nil || existing.Number != number || existing.URL == "" {
		return IssuePublicationResult{}, forgeResponseError("parse associated issue %s#%d", publication.Repo, number)
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
		return IssuePublicationResult{}, fmt.Errorf("update associated issue: %w", forgeCallError(err))
	}
	return IssuePublicationResult{Number: number, URL: existing.URL, Reused: reused}, nil
}

// findIssueByMarker walks every issue in stable creation order. Unlike GitHub
// search, this path is not index-dependent and is not capped at the first 100
// matches, so reconciliation cannot mistake a temporarily absent search hit
// for permission to create a second issue.
func findIssueByMarker(ctx context.Context, repo, marker string, run ghRunner) (int, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return 0, fmt.Errorf("find task issue: invalid GitHub repository %q", repo)
	}
	endpoint := fmt.Sprintf("repos/%s/%s/issues?state=all&sort=created&direction=asc&per_page=100", owner, name)
	out, err := run(ctx, "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return 0, fmt.Errorf("find task issue: %w", forgeCallError(err))
	}
	var pages [][]Issue
	if err = json.Unmarshal(out, &pages); err != nil {
		return 0, forgeResponseError("parse exhaustive task issue listing: %v", err)
	}
	if pages == nil {
		return 0, forgeResponseError("parse exhaustive task issue listing: missing page array")
	}
	for _, page := range pages {
		for _, issue := range page {
			if issue.Number > 0 && strings.Contains(issue.Body, marker) {
				return issue.Number, nil
			}
		}
	}
	return 0, nil
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
	HeadSHA   string
	BaseSHA   string
}

// PullRequestFiles returns GitHub's authoritative repository-relative file
// list for a pull request. Dispatch uses this only after merge confirmation;
// paths are never inferred from task or design metadata.
func PullRequestFiles(ctx context.Context, repo string, number int) ([]string, error) {
	return pullRequestFiles(ctx, repo, number, gh)
}

func pullRequestFiles(ctx context.Context, repo string, number int, run ghRunner) ([]string, error) {
	if number <= 0 || strings.Count(repo, "/") != 1 {
		return nil, fmt.Errorf("list pull request files: invalid repository or pull request")
	}
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", repo, number)
	out, err := run(ctx, "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("list pull request files for %s#%d: %w", repo, number, forgeCallError(err))
	}
	var pages [][]struct {
		Filename string `json:"filename"`
	}
	if err = json.Unmarshal(out, &pages); err != nil || pages == nil {
		return nil, forgeResponseError("parse pull request files for %s#%d", repo, number)
	}
	seen := map[string]bool{}
	paths := []string{}
	for _, page := range pages {
		for _, file := range page {
			name := strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(file.Filename, "\\", "/")), "./")
			if name == "" || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
				return nil, forgeResponseError("pull request files for %s#%d contain non-repository-relative path %q", repo, number, file.Filename)
			}
			if !seen[name] {
				seen[name] = true
				paths = append(paths, name)
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// PullRequestForBranch resolves the pull request attached to one assigned
// task branch. The merge gate always reads this before and after a merge
// request; a successful command alone is never treated as merged.
func PullRequestForBranch(ctx context.Context, repo, branch string) (PullRequest, error) {
	return pullRequestForBranch(ctx, repo, branch, gh)
}

func pullRequestForBranch(ctx context.Context, repo, branch string, run ghRunner) (PullRequest, error) {
	out, err := run(ctx, "pr", "view", branch, "--repo", repo, "--json", "number,url,state,mergedAt,mergeable,headRefOid,baseRefOid")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no pull requests found") || strings.Contains(strings.ToLower(err.Error()), "could not resolve to a pullrequest") {
			return PullRequest{}, &Error{Category: ForgeStatus, Err: fmt.Errorf("%w for branch %s: %v", ErrPullRequestNotFound, branch, err)}
		}
		return PullRequest{}, fmt.Errorf("view pull request for branch %s: %w", branch, forgeCallError(err))
	}
	var view struct {
		Number    int        `json:"number"`
		URL       string     `json:"url"`
		State     string     `json:"state"`
		MergedAt  *time.Time `json:"mergedAt"`
		Mergeable string     `json:"mergeable"`
		HeadSHA   string     `json:"headRefOid"`
		BaseSHA   string     `json:"baseRefOid"`
	}
	if err := json.Unmarshal(out, &view); err != nil || view.Number == 0 || view.URL == "" || view.State == "" || view.Mergeable == "" || view.HeadSHA == "" {
		return PullRequest{}, forgeResponseError("parse pull request for branch %s", branch)
	}
	return PullRequest{
		Number: view.Number, URL: view.URL, State: strings.ToLower(view.State),
		Mergeable: strings.ToUpper(view.Mergeable), Merged: view.MergedAt != nil,
		HeadSHA: strings.TrimSpace(view.HeadSHA), BaseSHA: strings.TrimSpace(view.BaseSHA),
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
		return fmt.Errorf("merge pull request %s#%d: %w", repo, number, forgeCallError(err))
	}
	return nil
}

// ListReviewFeedback returns human-authored PR review bodies and inline
// comments for the task branch. The dispatcher deduplicates IDs in its event
// log before converting them to redirect interventions (DEC-8).
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
	RequiredEffort         string
	ModelEnforcement       string
	History                []ReviewHistoryItem
	BounceHistory          []string
	// StatusState is the aggregate state of the current review round: pending,
	// success, or failure. It is computed from the durable round-completed event
	// rather than from one panel seat's verdict.
	StatusState string
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
	// CheckRunID is retained for wire/storage compatibility with historical
	// publications. Commit-status publications leave it zero.
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
	body := reviewPublicationBody(publication)
	if err = upsertReviewStatus(ctx, publication, target.URL, run); err != nil {
		return ReviewPublicationResult{}, err
	}
	commentID, err := upsertReviewComment(ctx, publication.Repo, target.Number, publication.TaskID, body, run)
	if err != nil {
		return ReviewPublicationResult{}, err
	}
	return ReviewPublicationResult{CommentID: commentID, ReviewedCommitSHA: publication.ReviewedCommitSHA}, nil
}

type reviewTargetResult struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"headRefOid"`
	BaseSHA string `json:"baseRefOid"`
}

type ReviewTarget struct {
	Number  int
	URL     string
	HeadSHA string
	BaseSHA string
}

func ReviewTargetForBranch(ctx context.Context, repo, branch string) (ReviewTarget, error) {
	target, err := reviewTarget(ctx, repo, branch, gh)
	return ReviewTarget{Number: target.Number, URL: target.URL, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA}, err
}

func reviewTarget(ctx context.Context, repo, branch string, run ghRunner) (reviewTargetResult, error) {
	out, err := run(ctx, "pr", "view", branch, "--repo", repo, "--json", "number,url,headRefOid,baseRefOid")
	if err != nil {
		return reviewTargetResult{}, fmt.Errorf("resolve reviewed PR: %w", forgeCallError(err))
	}
	var target reviewTargetResult
	if err := json.Unmarshal(out, &target); err != nil || target.Number == 0 || target.URL == "" || target.HeadSHA == "" {
		return reviewTargetResult{}, forgeResponseError("parse reviewed PR target")
	}
	return target, nil
}

func upsertReviewStatus(ctx context.Context, publication ReviewPublication, targetURL string, run ghRunner) error {
	state := publication.StatusState
	if state == "" {
		state = "success"
		if publication.Verdict == "changes_requested" {
			state = "failure"
		}
	}
	if state != "pending" && state != "success" && state != "failure" {
		return fmt.Errorf("publish review status: invalid aggregate state %q", state)
	}
	description := map[string]string{
		"pending": "Waiting for the remaining independent review verdicts",
		"success": "Independent review approved this commit",
		"failure": "Independent review requested changes",
	}[state]
	endpoint := fmt.Sprintf("repos/%s/commits/%s/status", publication.Repo, publication.ReviewedCommitSHA)
	out, err := run(ctx, "api", endpoint)
	if err != nil {
		return fmt.Errorf("read review status: %w", forgeCallError(err))
	}
	var combined struct {
		Statuses []struct {
			State       string `json:"state"`
			Context     string `json:"context"`
			Description string `json:"description"`
			TargetURL   string `json:"target_url"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(out, &combined); err != nil {
		return forgeResponseError("parse review status: %v", err)
	}
	if combined.Statuses == nil {
		return forgeResponseError("parse review status: missing statuses")
	}
	for _, status := range combined.Statuses {
		if status.Context == ReviewStatusContext && status.State == state && status.Description == description && status.TargetURL == targetURL {
			return nil
		}
	}
	_, err = run(ctx, "api", "--method", "POST", "repos/"+publication.Repo+"/statuses/"+publication.ReviewedCommitSHA,
		"-f", "state="+state, "-f", "context="+ReviewStatusContext, "-f", "description="+description, "-f", "target_url="+targetURL)
	if err != nil {
		return fmt.Errorf("publish review status: %w", forgeCallError(err))
	}
	return nil
}

func upsertReviewComment(ctx context.Context, repo string, pr int, taskID, body string, run ghRunner) (int64, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, pr), "--paginate", "--slurp")
	if err != nil {
		return 0, fmt.Errorf("list review comments: %w", forgeCallError(err))
	}
	var pages [][]struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return 0, forgeResponseError("parse review comments: %v", err)
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
		return 0, fmt.Errorf("publish review comment: %w", forgeCallError(err))
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &result); err != nil || result.ID == 0 {
		return 0, forgeResponseError("parse published review comment")
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
		if publication.RequiredEffort != "" {
			fmt.Fprintf(&body, "\n- Required effort: `%s`", publication.RequiredEffort)
		}
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
// (design-git-delivery).
func OpenPRForBranch(ctx context.Context, repo, branch, base, title, body string) (string, error) {
	body = reconcilePullRequestBody("", body)
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

// DiffBetween returns only the commits introduced after an approved review
// baseline. GitHub's compare endpoint is the authoritative delta source for
// refresh reviews (design-git-delivery).
func DiffBetween(ctx context.Context, repo, baseline, head string) (string, error) {
	out, err := gh(ctx, "api", "repos/"+repo+"/compare/"+baseline+"..."+head, "-H", "Accept: application/vnd.github.v3.diff")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

type gitRunner func(context.Context, string, string, ...string) error

func openPR(ctx context.Context, worktreeDir, repo, branch, base, title, body string, runGit gitRunner, runGH ghRunner) (string, error) {
	body = reconcilePullRequestBody("", body)
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
	before, after := existing, ""
	if start := strings.Index(existing, pullRequestLifecycleMarker); start >= 0 {
		end := legacyPullRequestLifecycleEnd(existing[start:]) + start
		before = strings.TrimSpace(existing[:start])
		after = strings.TrimSpace(existing[end:])
	} else if strings.HasPrefix(existing, "Conveyor task `") && strings.Contains(existing, "\n\nSource: ") {
		before = ""
	}

	// The explicit end marker makes future resyncs unambiguous: only this
	// generated region is replaced, while agent-authored Markdown on either
	// side remains untouched (design-git-delivery).
	lifecycle = strings.TrimSpace(strings.ReplaceAll(lifecycle, pullRequestLifecycleEndMarker, "")) + "\n" + pullRequestLifecycleEndMarker
	parts := make([]string, 0, 3)
	for _, part := range []string{before, lifecycle, after} {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}

func legacyPullRequestLifecycleEnd(body string) int {
	if end := strings.Index(body, pullRequestLifecycleEndMarker); end >= 0 {
		return end + len(pullRequestLifecycleEndMarker)
	}
	if evidence := strings.Index(body, verificationEvidenceMarker); evidence >= 0 {
		if footer := strings.Index(body[evidence:], verificationEvidenceFooter); footer >= 0 {
			return evidence + footer + len(verificationEvidenceFooter)
		}
		// Malformed legacy evidence has no safe boundary. Retain the old
		// replacement behavior rather than risk preserving stale generated data.
		return len(body)
	}
	if match := legacyPullRequestLifecyclePattern.FindStringIndex(body); match != nil {
		return match[1]
	}
	return len(body)
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
