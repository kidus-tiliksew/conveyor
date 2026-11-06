package monitor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type CommandRunner func(context.Context, ...string) ([]byte, error)

type TaskResolver func(context.Context, string, int) (string, bool, error)

type GitHubSource struct {
	WorkspaceID string
	Repository  string
	GitHubSlug  string
	// Base is the repository's configured base branch, which every commit
	// read names, whichever branch GitHub reports as the default
	// (req-delivery-and-forge AC-4.9).
	Base        string
	Run         CommandRunner
	ResolveTask TaskResolver
	// NewTaskResolver loads a fresh snapshot for each poll and takes precedence over ResolveTask.
	NewTaskResolver func(context.Context) (TaskResolver, error)
	ReconcileMerged func(context.Context, string, githubtrigger.PullRequest) (bool, error)
	LoadHints       func(context.Context, string) (*HintContext, error)
	OnSuppressed    func(context.Context, map[string]any) error
	// OnWindowFallback records a read that fell back to the window because the
	// stored tip diverged or is gone (component-monitor-drift invariant 4).
	OnWindowFallback func(context.Context, map[string]any) error
	// MergeCommitState classifies DirectPush and ExternalPRMerge commits
	// against recorded and in-flight Conveyor merges. Nil keeps today's
	// classification (component-monitor-drift; req-260811-228be6).
	MergeCommitState func(context.Context, string, string, time.Time) (MergeState, string, error)
}

type githubCommitParent struct {
	SHA string `json:"sha"`
}

type githubCommit struct {
	SHA     string               `json:"sha"`
	HTMLURL string               `json:"html_url"`
	Parents []githubCommitParent `json:"parents"`
	Commit  struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

type githubPull struct {
	Number   int        `json:"number"`
	HTMLURL  string     `json:"html_url"`
	MergedAt *time.Time `json:"merged_at"`
	MergedBy struct {
		Login string `json:"login"`
	} `json:"merged_by"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

type githubCheckRuns struct {
	CheckRuns []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		HTMLURL    string `json:"html_url"`
		Conclusion string `json:"conclusion"`
		RunAttempt int    `json:"run_attempt"`
	} `json:"check_runs"`
}

type failedCheckRun struct {
	ID   int64
	Name string
	URL  string
}

// RecordedLineage verifies that an observed pull request is the one Conveyor
// recorded for this task in this repository. Current Task.Branch must equal
// the observed head ref; a reconstructed default name is not identity
// (req-task-branch-assignment AC-3.4; component-monitor-drift). Identity is a
// recorded pull_request.opened for the number and repository; the merged head
// must then be one Conveyor recorded for that pull request, which survives a
// head moved by refresh or base merge (RecordedPullRequestHead;
// component-submission-merge "Observed merges"; DEC-64(5)).
func RecordedLineage(task core.Task, events []core.Event, repository, githubSlug, taskID, observedHeadRef string, pullRequestNumber int, headSHA string) bool {
	if task.ID != taskID || task.Repo != repository || task.Branch != observedHeadRef ||
		strings.TrimSpace(headSHA) == "" ||
		(task.GitHub != nil && task.GitHub.Repository != githubSlug) {
		return false
	}
	for _, event := range events {
		if event.Kind != "pull_request.opened" {
			continue
		}
		if opened, ok := decodeOpenedPullRequest(event); ok && opened.Number == pullRequestNumber && openedInRepository(opened, githubSlug) {
			return RecordedPullRequestHead(task, events, githubSlug, pullRequestNumber, headSHA)
		}
	}
	return false
}

// RecordedPullRequestHead reports whether headSHA is a head Conveyor recorded
// for pull request number in githubSlug: (a) the head of a pull_request.opened
// for it, (b) the head of a merge.confirmed or merge.reconciled for it, or (c)
// the task's approved head or `ReviewedHeadSHA` when approval is not stale and
// number is the task's latest recorded pull_request.opened. A pending refresh
// head never vouches for a merge, nor does approval vouch for an older or foreign PR.
// Empty heads never match (AC-3.5; AC-4.3; component-submission-merge
// "Observed merges"; DEC-64(5)).
func RecordedPullRequestHead(task core.Task, events []core.Event, githubSlug string, pullRequestNumber int, headSHA string) bool {
	if strings.TrimSpace(headSHA) == "" {
		return false
	}
	latest := 0
	for _, event := range events {
		switch event.Kind {
		case "pull_request.opened":
			opened, ok := decodeOpenedPullRequest(event)
			if !ok || !openedInRepository(opened, githubSlug) {
				continue
			}
			latest = opened.Number
			if opened.Number == pullRequestNumber && opened.HeadSHA == headSHA {
				return true
			}
		case "merge.confirmed", "merge.reconciled":
			var merged struct {
				Repository string `json:"repository"`
				Number     int    `json:"pull_request"`
				HeadSHA    string `json:"head_sha"`
			}
			if json.Unmarshal(event.Payload, &merged) == nil && merged.Repository == githubSlug &&
				merged.Number == pullRequestNumber && merged.HeadSHA == headSHA {
				return true
			}
		}
	}
	return latest != 0 && latest == pullRequestNumber &&
		(headSHA == task.ApprovedHeadSHA || (!task.ApprovalStale && headSHA == task.ReviewedHeadSHA))
}

// RecordedMergeCommit reports whether event is merge.confirmed or
// merge.reconciled whose payload repository is githubSlug and whose
// merge_commit_sha equals commitSHA. head_sha is not evidence: a later
// force-push or fast-forward of main onto a recorded PR head must stay
// visible (AC-4.3; component-monitor-drift; 261013-9f494b).
func RecordedMergeCommit(event core.Event, githubSlug, commitSHA string) bool {
	if strings.TrimSpace(commitSHA) == "" || strings.TrimSpace(githubSlug) == "" || (event.Kind != "merge.confirmed" && event.Kind != "merge.reconciled") {
		return false
	}
	var merged struct {
		Repository string `json:"repository"`
		MergeSHA   string `json:"merge_commit_sha"`
	}
	return json.Unmarshal(event.Payload, &merged) == nil && merged.Repository == githubSlug && merged.MergeSHA == commitSHA
}

// RecordedMergeHead reports whether commitSHA is the merge_commit_sha or
// head_sha of a merge.confirmed or merge.reconciled for this repository and
// pull request. Rule (b) of RecordedPullRequestHead only; stale approvals
// never match (AC-4.3; component-submission-merge "Observed merges").
func RecordedMergeHead(events []core.Event, githubSlug string, pullRequestNumber int, commitSHA string) bool {
	if strings.TrimSpace(commitSHA) == "" || strings.TrimSpace(githubSlug) == "" || pullRequestNumber == 0 {
		return false
	}
	for _, event := range events {
		if event.Kind != "merge.confirmed" && event.Kind != "merge.reconciled" {
			continue
		}
		var merged struct {
			Repository string `json:"repository"`
			Number     int    `json:"pull_request"`
			HeadSHA    string `json:"head_sha"`
			MergeSHA   string `json:"merge_commit_sha"`
		}
		if json.Unmarshal(event.Payload, &merged) == nil && merged.Repository == githubSlug && merged.Number == pullRequestNumber && (merged.MergeSHA == commitSHA || merged.HeadSHA == commitSHA) {
			return true
		}
	}
	return false
}

// EndsMergeFlight reports whether event closes an in-flight merge.requested.
// Only merge.confirmed, merge.reconciled, and definitive merge.failed
// reasons count. merge_unconfirmed and merge_verification_failed leave the
// request pending (DEC-64(6); dispatch/gate_merge.go pendingGateMergeRequest).
func EndsMergeFlight(event core.Event) bool {
	switch event.Kind {
	case "merge.confirmed", "merge.reconciled":
		return true
	case "merge.failed":
		var payload struct {
			Reason string `json:"reason_code"`
		}
		return json.Unmarshal(event.Payload, &payload) == nil && (payload.Reason == "forge_merge_failed" || payload.Reason == "merged_head_mismatch")
	default:
		return false
	}
}

// MergeRequestedInRepository reports a merge.requested whose payload
// repository equals the observed GitHub slug.
func MergeRequestedInRepository(event core.Event, githubSlug string) bool {
	if event.Kind != "merge.requested" || strings.TrimSpace(githubSlug) == "" {
		return false
	}
	var payload struct {
		Repository string `json:"repository"`
	}
	return json.Unmarshal(event.Payload, &payload) == nil && payload.Repository == githubSlug
}

type openedPullRequest struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	HeadSHA    string `json:"head_sha"`
}

func decodeOpenedPullRequest(event core.Event) (openedPullRequest, bool) {
	var opened openedPullRequest
	return opened, json.Unmarshal(event.Payload, &opened) == nil
}

func openedInRepository(opened openedPullRequest, githubSlug string) bool {
	return opened.Repository == "" || opened.Repository == githubSlug
}

// MatchObservedTask ranks workspace tasks for an observed pull. Recorded
// pull_request.opened owners of that number win first (non-terminal over
// terminal). Branch equality is only used when no recorded owner exists.
func MatchObservedTask(tasks []core.Task, eventsByID map[string][]core.Event, repository, githubSlug, headRef string, number int) (string, bool) {
	var recordedLive, recordedTerminal, branchLive, branchTerminal []core.Task
	for _, task := range tasks {
		if task.Repo != repository {
			continue
		}
		if task.GitHub != nil && task.GitHub.Repository != githubSlug {
			continue
		}
		recorded := false
		if number > 0 {
			for _, event := range eventsByID[task.ID] {
				if event.Kind != "pull_request.opened" {
					continue
				}
				var opened struct {
					Repository string `json:"repository"`
					Number     int    `json:"number"`
				}
				if json.Unmarshal(event.Payload, &opened) != nil || opened.Number != number {
					continue
				}
				if opened.Repository == "" || opened.Repository == githubSlug {
					recorded = true
					break
				}
			}
		}
		if recorded {
			if core.TaskTerminal(task.State) {
				recordedTerminal = append(recordedTerminal, task)
			} else {
				recordedLive = append(recordedLive, task)
			}
		}
		if task.Branch == headRef {
			if core.TaskTerminal(task.State) {
				branchTerminal = append(branchTerminal, task)
			} else {
				branchLive = append(branchLive, task)
			}
		}
	}
	switch {
	case len(recordedLive) > 0:
		return recordedLive[0].ID, true
	case len(recordedTerminal) > 0:
		return recordedTerminal[0].ID, true
	case len(branchLive) > 0:
		return branchLive[0].ID, true
	case len(branchTerminal) > 0:
		return branchTerminal[0].ID, true
	default:
		return "", false
	}
}

// Observations reads the base branch from the stored tip when there is one,
// otherwise from the window, and classifies what it reads. Read.NextTip stops
// short of every deferred commit (component-monitor-drift invariant 4).
func (s GitHubSource) Observations(ctx context.Context, from ReadFrom) (Read, error) {
	if s.Run == nil {
		return Read{}, githubtrigger.PermissionError(fmt.Errorf("workspace GitHub App is required for monitor reads; connect it in workspace settings"))
	}
	if strings.TrimSpace(s.Repository) == "" || strings.TrimSpace(s.GitHubSlug) == "" || strings.TrimSpace(s.Base) == "" {
		return Read{}, fmt.Errorf("monitor GitHub repository name, slug, and base branch are required")
	}
	resolveTask := s.ResolveTask
	if s.NewTaskResolver != nil {
		var err error
		resolveTask, err = s.NewTaskResolver(ctx)
		if err != nil {
			return Read{}, err
		}
	}
	listing, listErr := s.listCommits(ctx, from)
	if listErr != nil {
		return Read{}, listErr
	}
	commits := listing.commits
	// deferred holds the first-parent commits the tip may not pass yet.
	deferred := make(map[string]struct{})
	var observations []Observation
	seen := make(map[string]struct{})
	appendObservation := func(observation Observation) {
		key := string(observation.Kind) + ":" + observation.OccurrenceID
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		observations = append(observations, observation)
	}
	firstParent, ancestryErr := firstParentHistory(ctx, commits, s.isAncestor, s.firstParentOf)
	if ancestryErr != nil {
		return Read{}, ancestryErr
	}
	for _, commit := range commits {
		pulls, pullErr := s.pulls(ctx, commit.SHA)
		if pullErr != nil {
			return Read{}, pullErr
		}
		lineaged := false
		for _, pull := range pulls {
			if pull.MergedAt == nil {
				continue
			}
			var taskID string
			ok := false
			if resolveTask != nil {
				resolvedID, resolved, resolveErr := resolveTask(ctx, pull.Head.Ref, pull.Number)
				if resolveErr != nil {
					return Read{}, resolveErr
				}
				taskID, ok = resolvedID, resolved
			}
			if !ok {
				continue
			}
			if s.ReconcileMerged != nil {
				detail, err := s.mergedPull(ctx, pull.Number)
				if err != nil {
					return Read{}, err
				}
				if detail.MergedAt == nil || detail.Number != pull.Number || detail.Head.SHA != pull.Head.SHA || detail.Head.Ref != pull.Head.Ref {
					continue
				}
				pull = detail
				lineaged, err = s.ReconcileMerged(ctx, taskID, githubtrigger.PullRequest{
					Number: pull.Number, URL: pull.HTMLURL, State: "closed", Merged: true,
					HeadSHA: pull.Head.SHA, HeadRef: pull.Head.Ref,
					MergedBy: strings.TrimSpace(pull.MergedBy.Login), MergeCommitSHA: strings.TrimSpace(pull.MergeCommitSHA),
				})
				if err != nil {
					return Read{}, err
				}
			}
			if lineaged {
				break
			}
		}
		// Reconciliation above runs on every listed commit. Everything below
		// judges a commit as a change of its own, which only first-parent
		// history is: a commit reached through a merge's other parent counts
		// for no check signal, revert, external merge, or direct push, and
		// the merge's first-parent comparison already holds its diff
		// (req-delivery-and-forge AC-4.6, AC-4.7; req-260811-228be6 AC-4.1;
		// component-monitor-drift). A lost audit fails the pass, so a skipped
		// commit is never left unrecorded (req-260811-228be6 AC-4.4).
		if _, onFirstParent := firstParent[commit.SHA]; !onFirstParent {
			if s.OnSuppressed != nil {
				if err := s.OnSuppressed(ctx, map[string]any{
					"reason": "outside_first_parent", "repository": s.Repository,
					"commit_sha": commit.SHA,
				}); err != nil {
					return Read{}, fmt.Errorf("audit commit %s outside first-parent history: %w", commit.SHA, err)
				}
			}
			continue
		}
		hints, hintsSkipped, hintsErr := s.commitHints(ctx, commit.SHA)
		if hintsErr != nil {
			return Read{}, hintsErr
		}
		if lineaged {
			// Inside the window its checks and reruns may still conclude, so
			// the tip stays behind it and later polls read it again
			// (component-monitor-drift invariant 4).
			if !commit.Commit.Committer.Date.Before(from.Since) {
				deferred[commit.SHA] = struct{}{}
			}
			checks, checkErr := s.checks(ctx, commit.SHA)
			if checkErr != nil {
				return Read{}, checkErr
			}
			byAttempt := make(map[int][]failedCheckRun)
			green := false
			for _, check := range checks.CheckRuns {
				if check.Conclusion != "failure" && check.Conclusion != "timed_out" {
					green = green || check.Conclusion == "success"
					if s.OnSuppressed != nil {
						_ = s.OnSuppressed(ctx, map[string]any{
							"reason": "check_not_actionable", "repository": s.Repository,
							"commit_sha": commit.SHA, "check_run_id": check.ID,
							"conclusion": check.Conclusion,
						})
					}
					continue
				}
				attempt := check.RunAttempt
				if attempt <= 0 {
					attempt = 1
				}
				byAttempt[attempt] = append(byAttempt[attempt], failedCheckRun{
					ID: check.ID, Name: strings.TrimSpace(check.Name), URL: check.HTMLURL,
				})
			}
			attempts := make([]int, 0, len(byAttempt))
			for attempt := range byAttempt {
				attempts = append(attempts, attempt)
			}
			sort.Ints(attempts)
			for _, attempt := range attempts {
				failed := byAttempt[attempt]
				sort.Slice(failed, func(i, j int) bool { return failed[i].ID < failed[j].ID })
				checkIDs := make([]string, 0, len(failed))
				checkDetails := make([]string, 0, len(failed))
				for _, check := range failed {
					id := strconv.FormatInt(check.ID, 10)
					checkIDs = append(checkIDs, id)
					name := check.Name
					if name == "" {
						name = "unnamed check"
					}
					checkDetails = append(checkDetails, fmt.Sprintf("- %s (check run %s): %s", name, id, check.URL))
				}
				failedContext := map[string]string{"failed_check_runs": strings.Join(checkDetails, "\n")}
				if hintsSkipped != "" {
					failedContext[ContextHintsSkipped] = hintsSkipped
				}
				appendObservation(Observation{
					WorkspaceID: s.WorkspaceID, Repository: s.Repository,
					Kind:         PostMergeFailure,
					OccurrenceID: "commit:" + commit.SHA + ":attempt:" + strconv.Itoa(attempt),
					SourceURL:    failed[0].URL, CommitSHA: commit.SHA,
					CheckRunID: strings.Join(checkIDs, ","),
					Attempt:    attempt,
					ObservedAt: commit.Commit.Committer.Date, Hints: hints,
					Context: failedContext,
				})
			}
			if len(byAttempt) == 0 && green {
				appendObservation(Observation{
					WorkspaceID: s.WorkspaceID, Repository: s.Repository,
					Kind: PostMergeFailure, OccurrenceID: "recovery:commit:" + commit.SHA,
					SourceURL: commit.HTMLURL, CommitSHA: commit.SHA,
					ObservedAt: commit.Commit.Committer.Date, RecoveryObserved: true,
				})
			}
			continue
		}
		kind, sourceURL, occurrenceID, prNumber := DirectPush, commit.HTMLURL, commit.SHA, 0
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(commit.Commit.Message)), "revert") {
			kind = Revert
		} else {
			for _, pull := range pulls {
				if pull.MergedAt != nil {
					kind, sourceURL, prNumber = ExternalPRMerge, pull.HTMLURL, pull.Number
					occurrenceID = "pr:" + strconv.Itoa(pull.Number)
					break
				}
			}
		}
		state, skipErr := s.suppressMergeCommit(ctx, kind, commit)
		if skipErr != nil {
			return Read{}, skipErr
		}
		if state == MergeInFlight {
			deferred[commit.SHA] = struct{}{}
		}
		if state != MergeNone {
			continue
		}
		// Every out-of-pipeline kind (direct push, external pull-request
		// merge at a head no task approval covers, revert) carries the
		// observed commit's first-parent paths so governed-scope evaluation
		// raises document drift for it (req-260811-228be6 AC-4.3;
		// component-monitor-drift). The comparison reads the base-branch
		// commit against its first parent, never the pull request's branch
		// head, through the workspace App runner (DEC-59).
		var changed firstParentPaths
		if len(commit.Parents) > 0 {
			var diffErr error
			changed, diffErr = s.pathsFromFirstParent(ctx, commit.Parents[0].SHA, commit.SHA)
			if diffErr != nil {
				return Read{}, diffErr
			}
			// Only a direct push whose comparison lists no file at all is
			// suppressed. An empty external merge or revert comparison carries
			// no document paths but keeps its repository observation.
			if kind == DirectPush && changed.listed == 0 {
				if s.OnSuppressed != nil {
					_ = s.OnSuppressed(ctx, map[string]any{
						"reason": "first_parent_empty", "repository": s.Repository,
						"commit_sha": commit.SHA, "first_parent_sha": commit.Parents[0].SHA,
					})
				}
				continue
			}
		}
		var observationContext map[string]string
		if changed.incomplete != "" || hintsSkipped != "" {
			observationContext = map[string]string{}
			if changed.incomplete != "" {
				observationContext[ContextPathsIncomplete] = changed.incomplete
			}
			if hintsSkipped != "" {
				observationContext[ContextHintsSkipped] = hintsSkipped
			}
		}
		appendObservation(Observation{
			WorkspaceID: s.WorkspaceID, Repository: s.Repository, Kind: kind,
			OccurrenceID: occurrenceID, SourceURL: sourceURL, CommitSHA: commit.SHA,
			ChangedPaths:      changed.paths,
			PullRequestNumber: prNumber, ObservedAt: commit.Commit.Committer.Date, Hints: hints,
			Context: observationContext,
		})
	}
	return Read{Observations: observations, NextTip: listing.nextTip(firstParent, deferred)}, nil
}

// commitListing is what one pass reads from the base branch, newest first with
// the branch head as the first entry.
type commitListing struct {
	commits []githubCommit
	// fromTip says the commits are everything after the stored tip.
	fromTip bool
	// windowHead is the branch head read before a window listing.
	windowHead string
}

// nextTip returns the commit the tip may move to: the branch head, or the first
// parent of the oldest deferred commit. Comparing from the result reads every
// deferred commit again. Empty means the tip stays where it is.
//
// A window read has no tip to protect, so the first parent of its oldest listed
// commit is a valid start even though the window didn't list it. Without that a
// repository that merges more often than its window would never store a tip. A
// read from a tip keeps the tip instead, because only a listed commit is known
// to be newer than it.
func (l commitListing) nextTip(firstParent, deferred map[string]struct{}) string {
	if len(l.commits) == 0 {
		return l.windowHead
	}
	listed := make(map[string]githubCommit, len(l.commits))
	for _, commit := range l.commits {
		listed[commit.SHA] = commit
	}
	chained := make(map[string]struct{}, len(l.commits))
	var chain []githubCommit
	for sha := l.commits[0].SHA; ; {
		commit, isListed := listed[sha]
		if _, visited := chained[sha]; !isListed || visited {
			break
		}
		chained[sha] = struct{}{}
		chain = append(chain, commit)
		if len(commit.Parents) == 0 {
			break
		}
		sha = commit.Parents[0].SHA
	}
	// A deferred commit off the chain has no known place in the order, which
	// leaves nothing proven about how far the tip may move.
	for sha := range deferred {
		_, onHistory := firstParent[sha]
		if _, onChain := chained[sha]; onHistory && !onChain {
			return ""
		}
	}
	oldest := -1
	for i, commit := range chain {
		if _, held := deferred[commit.SHA]; held {
			oldest = i
		}
	}
	if oldest < 0 {
		return l.commits[0].SHA
	}
	if oldest+1 < len(chain) {
		return chain[oldest+1].SHA
	}
	if l.fromTip || len(chain[oldest].Parents) == 0 {
		return ""
	}
	return chain[oldest].Parents[0].SHA
}

// listCommits reads from the tip when there is one, and otherwise from the
// window. A tip that diverged from the branch or no longer exists falls back to
// the window, which is recorded once the window read succeeds.
func (s GitHubSource) listCommits(ctx context.Context, from ReadFrom) (commitListing, error) {
	fallback := ""
	if from.Tip != "" {
		commits, status, err := s.compareFromTip(ctx, from.Tip)
		switch {
		case err == nil && status != "diverged":
			return commitListing{commits: commits, fromTip: true}, nil
		case err == nil:
			fallback = "diverged"
		case tipGone(err):
			fallback = "tip_gone"
		default:
			return commitListing{}, err
		}
	}
	listing, err := s.listWindow(ctx, from.Since)
	if err != nil {
		return commitListing{}, err
	}
	if fallback != "" && s.OnWindowFallback != nil {
		if err = s.OnWindowFallback(ctx, map[string]any{
			"repository": s.Repository, "base": s.Base, "tip": from.Tip, "reason": fallback,
		}); err != nil {
			return commitListing{}, fmt.Errorf("audit window fallback from tip %s: %w", from.Tip, err)
		}
	}
	return listing, nil
}

// tipGone reports the answers GitHub gives a comparison whose tip it no longer
// has. A repository the App can't reach answers the same, and the window read
// that follows fails with its own category.
func tipGone(err error) bool {
	status := githubtrigger.HTTPStatus(err)
	return status == http.StatusNotFound || status == http.StatusUnprocessableEntity
}

// listWindow reads the commits dated at or after since. It reads the branch
// head first, so a quiet window still stores a tip, and a commit pushed during
// this read stays after that tip for the next pass to read.
func (s GitHubSource) listWindow(ctx context.Context, since time.Time) (commitListing, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/commits/"+s.Base,
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return commitListing{}, githubtrigger.CategorizeError(err)
	}
	var head githubCommit
	if err = json.Unmarshal(raw, &head); err != nil {
		return commitListing{}, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor branch head: %w", err)}
	}
	if head.SHA == "" {
		return commitListing{}, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("monitor branch head read for %s names no commit", s.Base)}
	}
	var commits []githubCommit
	for page := 1; ; page++ {
		raw, err = s.Run(ctx, "api", "--method", "GET",
			"repos/"+s.GitHubSlug+"/commits",
			"-f", "sha="+s.Base,
			"-f", "since="+since.UTC().Format(time.RFC3339),
			"-f", "per_page=100", "-f", "page="+strconv.Itoa(page))
		if err != nil {
			return commitListing{}, githubtrigger.CategorizeError(err)
		}
		var batch []githubCommit
		if err = json.Unmarshal(raw, &batch); err != nil {
			return commitListing{}, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor commits: %w", err)}
		}
		commits = append(commits, batch...)
		if len(batch) < 100 {
			return commitListing{commits: commits, windowHead: head.SHA}, nil
		}
	}
}

// compareFromTip reads every commit GitHub lists after tip and the comparison's
// status, stopping at a diverged answer. A count that disagrees with
// total_commits fails the read, so nothing is classified from a partial answer.
func (s GitHubSource) compareFromTip(ctx context.Context, tip string) ([]githubCommit, string, error) {
	var commits []githubCommit
	for page := 1; ; page++ {
		raw, err := s.Run(ctx, "api", "--method", "GET",
			"repos/"+s.GitHubSlug+"/compare/"+tip+"..."+s.Base,
			"-f", "per_page=100", "-f", "page="+strconv.Itoa(page),
			"-H", "Accept: application/vnd.github+json")
		if err != nil {
			return nil, "", githubtrigger.CategorizeError(err)
		}
		var comparison struct {
			Status       string         `json:"status"`
			TotalCommits *int           `json:"total_commits"`
			Commits      []githubCommit `json:"commits"`
		}
		if err = json.Unmarshal(raw, &comparison); err != nil {
			return nil, "", &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse tip comparison: %w", err)}
		}
		switch comparison.Status {
		case "diverged":
			return nil, comparison.Status, nil
		case "ahead", "identical", "behind":
		default:
			return nil, "", &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("comparison from tip %s has an unknown status %q", tip, comparison.Status)}
		}
		commits = append(commits, comparison.Commits...)
		if len(comparison.Commits) == 100 {
			continue
		}
		if comparison.TotalCommits == nil || *comparison.TotalCommits != len(commits) {
			return nil, "", &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("comparison from tip %s listed %d commits and did not state that total", tip, len(commits))}
		}
		ordered, err := newestFirst(commits)
		if err != nil {
			return nil, "", err
		}
		return ordered, comparison.Status, nil
	}
}

// newestFirst adapts a comparison, which answers oldest first, to the order
// firstParentHistory and the classifier expect: newest first with the branch
// head as the first entry. The head is the one commit no listed commit names as
// a parent, whatever order the pages arrived in. Anything other than one such
// commit is a malformed answer.
func newestFirst(commits []githubCommit) ([]githubCommit, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	ordered := slices.Clone(commits)
	slices.Reverse(ordered)
	parents := make(map[string]struct{}, len(ordered))
	for _, commit := range ordered {
		for _, parent := range commit.Parents {
			parents[parent.SHA] = struct{}{}
		}
	}
	head := -1
	for i, commit := range ordered {
		if _, isParent := parents[commit.SHA]; isParent {
			continue
		}
		if head >= 0 {
			return nil, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("tip comparison lists more than one head: %s and %s", ordered[head].SHA, commit.SHA)}
		}
		head = i
	}
	if head < 0 {
		return nil, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: errors.New("tip comparison lists no head")}
	}
	headCommit := ordered[head]
	copy(ordered[1:head+1], ordered[:head])
	ordered[0] = headCommit
	return ordered, nil
}

// commitHints loads a commit's hints. An unusable file processes the commit
// without hints and returns why, after recording it. A failed read fails the
// pass (req-delivery-and-forge AC-4.11; component-monitor-drift invariant 14).
func (s GitHubSource) commitHints(ctx context.Context, sha string) (*HintContext, string, error) {
	if s.LoadHints == nil {
		return nil, "", nil
	}
	hints, err := s.LoadHints(ctx, sha)
	var unusable *UnusableHintsError
	if !errors.As(err, &unusable) {
		return hints, "", err
	}
	// The reason can quote the repository's own file.
	reason, _ := redact.New(nil).Redact(unusable.Reason)
	if s.OnSuppressed != nil {
		if auditErr := s.OnSuppressed(ctx, map[string]any{
			"reason": "hints_unusable", "repository": s.Repository, "commit_sha": sha,
			"path": hintsPath, "detail": reason,
		}); auditErr != nil {
			return nil, "", fmt.Errorf("audit unusable hints at %s: %w", sha, auditErr)
		}
	}
	return nil, reason, nil
}

// unlistedFirstParentReads is how far past the listed commits one pass reads
// to settle commits that an old-dated first parent hides. A listed commit needs
// that only when committer dates run backwards along first-parent history.
const unlistedFirstParentReads = 16

// firstParentHistory returns the listed commits a pass judges as changes of
// their own: those on the base branch's first-parent history, and those it
// can't tell apart from it. The first failed read fails it, so nothing is
// classified from a partial answer (component-monitor-drift invariant 15, State
// and failure). GitHub lists newest first from the requested branch, so the
// first commit is the tip, and the chain follows Parents[0] through the listed
// commits to a stop, the first parent that isn't listed.
//
// A stop says nothing about the commits still open, because first-parent
// history continues behind it: a commit older than the window can have a listed
// ancestor with a newer committer date. How an open commit was reached proves
// nothing either, since a merged branch and the first parent can share an
// ancestor. The proof that it is outside first-parent history is that it isn't
// an ancestor of the stop. A parent that is the stop, is on the chain, or is
// already excluded shows that without a read, because an ancestor of the stop
// can't descend from any of them, and isAncestor answers the rest.
//
// A commit that is an ancestor of the stop stays open, and the chain continues
// through firstParentOf, so every stop is on first-parent history and every
// exclusion stays proven. Reads are bounded by one per listed commit plus
// unlistedFirstParentReads. Past that bound, or at a chain with no stop, the
// commits still open fail toward visibility.
func firstParentHistory(ctx context.Context, commits []githubCommit,
	isAncestor func(ctx context.Context, candidate, descendant string) (bool, error),
	firstParentOf func(ctx context.Context, sha string) (string, bool, error),
) (map[string]struct{}, error) {
	listed := make(map[string]githubCommit, len(commits))
	unique := make([]githubCommit, 0, len(commits))
	for _, commit := range commits {
		if _, duplicate := listed[commit.SHA]; !duplicate {
			listed[commit.SHA] = commit
			unique = append(unique, commit)
		}
	}
	history := make(map[string]struct{}, len(unique))
	if len(unique) == 0 {
		return history, nil
	}
	excluded := make(map[string]struct{})
	settled := func(sha string) bool {
		_, chained := history[sha]
		_, outside := excluded[sha]
		return chained || outside
	}
	reads := len(unique) + unlistedFirstParentReads
	next := unique[0].SHA
	for {
		stop := ""
		for sha := next; ; {
			commit, isListed := listed[sha]
			if !isListed {
				stop = sha
				break
			}
			if _, visited := history[sha]; visited {
				break
			}
			history[sha] = struct{}{}
			if len(commit.Parents) == 0 {
				break
			}
			sha = commit.Parents[0].SHA
		}
		// Oldest first, so an excluded commit settles its descendants without
		// a read of their own.
		open := false
		for i := len(unique) - 1; i >= 0; i-- {
			commit := unique[i]
			if settled(commit.SHA) {
				continue
			}
			if slices.ContainsFunc(commit.Parents, func(parent githubCommitParent) bool {
				return parent.SHA == stop || settled(parent.SHA)
			}) {
				excluded[commit.SHA] = struct{}{}
				continue
			}
			if stop == "" || reads == 0 {
				open = true
				continue
			}
			reads--
			ancestor, err := isAncestor(ctx, commit.SHA, stop)
			if err != nil {
				return nil, err
			}
			if ancestor {
				open = true
			} else {
				excluded[commit.SHA] = struct{}{}
			}
		}
		if !open || stop == "" || reads == 0 {
			break
		}
		reads--
		parent, hasParent, err := firstParentOf(ctx, stop)
		if err != nil {
			return nil, err
		}
		if !hasParent {
			break
		}
		next = parent
	}
	for _, commit := range unique {
		if !settled(commit.SHA) {
			history[commit.SHA] = struct{}{}
		}
	}
	return history, nil
}

// firstParentOf reads one commit below the window and returns its first
// parent, or false for a root commit.
func (s GitHubSource) firstParentOf(ctx context.Context, sha string) (string, bool, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/commits/"+sha,
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return "", false, githubtrigger.CategorizeError(err)
	}
	var commit githubCommit
	if err = json.Unmarshal(raw, &commit); err != nil {
		return "", false, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor commit %s: %w", sha, err)}
	}
	if commit.SHA != sha {
		return "", false, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("monitor commit read for %s answered %q", sha, commit.SHA)}
	}
	if len(commit.Parents) == 0 {
		return "", false, nil
	}
	return commit.Parents[0].SHA, true, nil
}

// isAncestor reports whether candidate is descendant or one of its ancestors,
// from GitHub's comparison of the two. Any other answer than the four statuses
// GitHub documents fails the read rather than guessing.
func (s GitHubSource) isAncestor(ctx context.Context, candidate, descendant string) (bool, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/compare/"+descendant+"..."+candidate,
		"-f", "per_page=1",
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return false, githubtrigger.CategorizeError(err)
	}
	var comparison struct {
		Status string `json:"status"`
	}
	if err = json.Unmarshal(raw, &comparison); err != nil {
		return false, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse ancestry comparison: %w", err)}
	}
	switch comparison.Status {
	case "behind", "identical":
		return true, nil
	case "ahead", "diverged":
		return false, nil
	default:
		return false, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("ancestry comparison of %s and %s has an unknown status %q", descendant, candidate, comparison.Status)}
	}
}

// suppressMergeCommit returns the commit's merge state after recording a
// suppression. MergeNone means the commit is classified as observed.
func (s GitHubSource) suppressMergeCommit(ctx context.Context, kind SignalKind, commit githubCommit) (MergeState, error) {
	if s.MergeCommitState == nil || (kind != DirectPush && kind != ExternalPRMerge) {
		return MergeNone, nil
	}
	state, taskID, err := s.MergeCommitState(ctx, s.GitHubSlug, commit.SHA, commit.Commit.Committer.Date)
	if err != nil {
		return MergeNone, err
	}
	var reason string
	switch state {
	case MergeRecorded:
		reason = "conveyor_merge_recorded"
	case MergeInFlight:
		reason = "merge_in_flight"
	default:
		return MergeNone, nil
	}
	if s.OnSuppressed != nil {
		_ = s.OnSuppressed(ctx, map[string]any{
			"reason": reason, "repository": s.Repository,
			"commit_sha": commit.SHA, "task_id": taskID,
		})
	}
	return state, nil
}

// firstParentPaths is one commit's comparison against its first parent.
type firstParentPaths struct {
	paths []string
	// listed counts the files the comparison named, valid or not.
	listed int
	// incomplete is the paths_incomplete context value, empty when paths names
	// every changed file.
	incomplete string
}

// Carry the first-parent paths into governed-scope evaluation instead of only
// testing for an empty push (req-260811-228be6 AC-4.3; component-monitor-drift).
// A missing file list aborts the read. A list cut off at GitHub's limit, or a
// filename that isn't a clean repository-relative path, returns the valid paths
// and says what is missing, because neither gets better on a later read
// (component-monitor-drift invariant 13).
func (s GitHubSource) pathsFromFirstParent(ctx context.Context, parentSHA, commitSHA string) (firstParentPaths, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/compare/"+parentSHA+"..."+commitSHA,
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return firstParentPaths{}, githubtrigger.CategorizeError(err)
	}
	var comparison struct {
		Files *[]struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err = json.Unmarshal(raw, &comparison); err != nil {
		return firstParentPaths{}, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse first-parent comparison: %w", err)}
	}
	if comparison.Files == nil {
		return firstParentPaths{}, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse first-parent comparison: files are missing")}
	}
	result := firstParentPaths{listed: len(*comparison.Files)}
	var paths []string
	for _, file := range *comparison.Files {
		names := []string{file.Filename}
		if file.PreviousFilename != "" {
			names = append(names, file.PreviousFilename)
		}
		for _, name := range names {
			if name == "" || name == "." || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
				result.incomplete = PathsInvalidFilename
				continue
			}
			paths = append(paths, name)
		}
	}
	// Comparison pagination covers commits, not files. At the same limit used
	// by the delivery comparison reader, completeness cannot be established.
	// This takes precedence because it widens document drift.
	if result.listed >= 300 {
		result.incomplete = PathsFileLimit
	}
	sort.Strings(paths)
	result.paths = compactStrings(paths)
	return result, nil
}

// The associated-PR endpoint returns pull-request-simple without merged_by.
// Read the detail before attributing completion (req-delivery-and-forge AC-3.5).
func (s GitHubSource) mergedPull(ctx context.Context, number int) (githubPull, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET", "repos/"+s.GitHubSlug+"/pulls/"+strconv.Itoa(number))
	if err != nil {
		return githubPull{}, githubtrigger.CategorizeError(err)
	}
	var pull githubPull
	if err := json.Unmarshal(raw, &pull); err != nil || pull.Number == 0 || pull.Head.SHA == "" {
		return githubPull{}, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor pull request detail")}
	}
	return pull, nil
}

func (s GitHubSource) pulls(ctx context.Context, sha string) ([]githubPull, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/commits/"+sha+"/pulls",
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return nil, githubtrigger.CategorizeError(err)
	}
	var pulls []githubPull
	if err = json.Unmarshal(raw, &pulls); err != nil {
		return nil, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor pull requests: %w", err)}
	}
	return pulls, nil
}

func (s GitHubSource) checks(ctx context.Context, sha string) (githubCheckRuns, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/commits/"+sha+"/check-runs",
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return githubCheckRuns{}, githubtrigger.CategorizeError(err)
	}
	var checks githubCheckRuns
	if err = json.Unmarshal(raw, &checks); err != nil {
		return githubCheckRuns{}, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor check runs: %w", err)}
	}
	return checks, nil
}

// FetchGitHubHints reads the advisory file from the exact observed revision.
// A missing file means the repository supplies no hints. A file that can't be
// used returns *UnusableHintsError, and any other failure is a failed read
// (component-monitor-drift invariant 14).
func FetchGitHubHints(ctx context.Context, slug, revision string, run CommandRunner) (*HintContext, error) {
	if run == nil {
		return nil, githubtrigger.PermissionError(fmt.Errorf("workspace forge token is required for monitor hints; add it in workspace settings"))
	}
	raw, err := run(ctx, "api", "--method", "GET",
		"repos/"+slug+"/contents/"+hintsPath, "-f", "ref="+revision)
	if err != nil {
		detail := strings.ToLower(err.Error())
		if strings.Contains(detail, "404") || strings.Contains(detail, "not found") {
			return nil, nil
		}
		return nil, githubtrigger.CategorizeError(err)
	}
	var response struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		// Valid JSON of another shape is what GitHub answers when the path is
		// a directory. Only unparsable JSON is a malformed answer.
		var wrongShape *json.UnmarshalTypeError
		if errors.As(err, &wrongShape) {
			return nil, &UnusableHintsError{Reason: hintsPath + " is not a file"}
		}
		return nil, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse advisory hints response")}
	}
	if response.Encoding != "base64" {
		return nil, &UnusableHintsError{Reason: fmt.Sprintf("GitHub returned %s with encoding %q", hintsPath, response.Encoding)}
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(response.Content, "\n", ""))
	if err != nil {
		return nil, &UnusableHintsError{Reason: fmt.Sprintf("decode advisory hints: %v", err)}
	}
	hints, err := ParseHints(data, revision)
	if err != nil {
		return nil, &UnusableHintsError{Reason: err.Error()}
	}
	return &hints, nil
}
