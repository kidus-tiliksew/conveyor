package workorder

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// submissionPRHeadBackoff is the bounded re-read schedule for a pull request
// whose observed head lags the pushed head: six reads and 3.1 seconds of delay
// at most (component-work-orders).
var submissionPRHeadBackoff = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}

// SubmitForReview admits the exact claimed implement session, or a replay by
// the same session of an order it already submitted at the identical head.
// Every other lifecycle call keeps claimed-only admission.
func (s *Service) SubmitForReview(ctx context.Context, id, session, headSHA string) (map[string]any, error) {
	order, _, err := s.admitSubmission(ctx, id, session, headSHA)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = s.Store.WithTaskSideEffectLock(ctx, order.TaskID, func(ctx context.Context) error {
		var submitErr error
		result, submitErr = s.submitForReviewLocked(ctx, id, session, headSHA)
		return submitErr
	})
	return result, err
}

// admitSubmission is the submit-specific admission path. A submitted order is
// admitted only for its own session at its recorded head; changed heads and
// foreign sessions are refused, and every non-submitted order goes through the
// ordinary claimed-session check.
func (s *Service) admitSubmission(ctx context.Context, id, session, headSHA string) (core.WorkOrder, bool, error) {
	current, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	if current.State == core.WorkOrderSubmitted && current.Stage == core.StageImplement {
		if session == "" || current.SessionID != session {
			return core.WorkOrder{}, false, fmt.Errorf("work order %s belongs to another session", id)
		}
		head := strings.TrimSpace(headSHA)
		if head == "" || current.HeadSHA == "" || head != current.HeadSHA {
			return core.WorkOrder{}, false, fmt.Errorf("work order %s is not claimed: it was already submitted at head %q; only that head replays the handoff", id, current.HeadSHA)
		}
		return current, true, nil
	}
	order, err := s.authorized(ctx, id, session)
	return order, false, err
}

func (s *Service) submitForReviewLocked(ctx context.Context, id, session, headSHA string) (map[string]any, error) {
	order, replay, err := s.admitSubmission(ctx, id, session, headSHA)
	if err != nil {
		return nil, err
	}
	if replay {
		return s.submissionReplayResult(ctx, order)
	}
	if order.Stage != core.StageImplement {
		return nil, fmt.Errorf("work order %s is not implement", id)
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return nil, fmt.Errorf("head_sha is required")
	}
	if err = s.enforce(ctx, order); err != nil {
		return nil, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return nil, err
	}
	if task.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(task.SetupContract)
	}
	// A refresh already dispatched for this exact conflict-fix head owns the
	// review; the order and its own job complete once without repeating any
	// PR, context, stale-approval, stage, or dispatch effect.
	if refresh, ok, refreshErr := s.dispatchedConflictRefresh(ctx, task, order, headSHA); refreshErr != nil {
		return nil, refreshErr
	} else if ok {
		return s.completeDispatchedRefresh(ctx, cfg, task, order, headSHA, refresh)
	}
	// Read-only admission: the task must accept stage.advance and the order's
	// own job must be completable before any submission side effect.
	if _, err = core.TransitionTask(task.State, core.TaskStageAdvance); err != nil {
		return nil, fmt.Errorf("submission refused before side effects: %w", err)
	}
	if err = s.submissionJobReady(ctx, order); err != nil {
		return nil, err
	}
	// Eligible task-owned evidence reaches the review seats and the pull
	// request; its absence never refuses the submission
	// (req-review-gates-evidence REQ-8/AC-8.2, AC-8.3; DEC-53).
	evidence, err := s.taskVerificationEvidence(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok {
		return nil, fmt.Errorf("repo %s not found", task.Repo)
	}
	var target github.SubmissionPullRequest
	authorID := ""
	forgeCtx := ctx
	if repo.GitHub != "" {
		if spec, exists, specErr := s.Store.GetLatestSpecVersion(ctx, task.ID); specErr != nil {
			return nil, specErr
		} else if exists && spec.Approved {
			if task.GitHub == nil {
				return nil, fmt.Errorf("approved task %s has no durable GitHub issue association yet; retry after reconciliation", task.ID)
			}
			if task.GitHub.Repository != repo.GitHub {
				return nil, fmt.Errorf("task GitHub association repository %q does not match configured repository %q", task.GitHub.Repository, repo.GitHub)
			}
			if task.GitHub.State != core.GitHubPublicationPublished || task.GitHub.IssueNumber <= 0 {
				return nil, fmt.Errorf("GitHub issue publication for task %s is %s; retry after publication reconciliation", task.ID, task.GitHub.State)
			}
		}
		lookup := s.SubmissionPR
		if lookup == nil {
			forgeCtx, err = s.workspaceForgeContext(ctx, repo.GitHub)
			if err != nil {
				return nil, err
			}
			lookup = github.SubmissionPRForBranch
		}
		target, err = s.observeSubmissionPR(ctx, forgeCtx, lookup, repo.GitHub, task.Branch, task.BaseBranch, headSHA)
		if err != nil {
			return nil, err
		}
		authorID, err = store.WorkOrderOwnerUserID(ctx, s.Store, order)
		if err != nil {
			return nil, err
		}
	}
	comparisonTask := task
	comparisonTask.ReviewedHeadSHA = headSHA
	if target.Base.SHA != "" {
		comparisonTask.BaseBranch = target.Base.SHA
	}
	// Diff-derived authority must be complete before the first PR, task-stage,
	// or review-dispatch side effect (req-task-centric-delivery REQ-6/AC-6.1–AC-6.4).
	governance, err := s.Store.ListGovernanceDesigns(ctx, task.Repo)
	if err != nil {
		return nil, fmt.Errorf("resolve submission governance: %w", err)
	}
	if len(governance) > 0 || repo.GitHub != "" {
		changedPaths := s.SubmissionChangedPaths
		if changedPaths == nil && s.Dispatcher != nil {
			changedPaths = s.Dispatcher.ReviewChangedPaths
		}
		if changedPaths == nil {
			changedPaths = dispatch.ReviewBranchChangedPaths
		}
		paths, pathErr := changedPaths(forgeCtx, cfg, comparisonTask)
		if pathErr != nil {
			return nil, fmt.Errorf("resolve submission diff changed paths: %w", pathErr)
		}
		if repo.GitHub != "" {
			diff, diffErr := s.reviewDiffBetween(ctx, repo.GitHub, comparisonTask.BaseBranch, headSHA)
			if diffErr != nil {
				return nil, fmt.Errorf("resolve submission diff for head %s: %w", headSHA, diffErr)
			}
			if len(diff) > 25<<20 {
				return nil, fmt.Errorf("submission diff for head %s exceeds the 25 MiB review input limit", headSHA)
			}
		}
		if _, err = s.Store.AttachSubmissionGovernance(ctx, task.ID, task.Repo, paths, store.SubmissionGovernanceAttribution{WorkOrderID: order.ID, SessionID: session}); err != nil {
			return nil, fmt.Errorf("attach submission governance: %w", err)
		}
	}
	prURL := target.URL
	reviewedHead := headSHA
	if repo.GitHub != "" {
		reconcile := s.ReconcileSubmissionPR
		if reconcile == nil {
			reconcile = github.ReconcileSubmissionPR
			forgeCtx, err = s.workspaceForgeContext(ctx, repo.GitHub)
			if err != nil {
				return nil, err
			}
		}
		if err = reconcile(forgeCtx, repo.GitHub, target, dispatch.PRBody(task, evidence...)); err != nil {
			return nil, fmt.Errorf("reconcile pull request body: %w", err)
		}
		evidenceIDs := make([]string, 0, len(evidence))
		for _, item := range evidence {
			evidenceIDs = append(evidenceIDs, item.ID)
		}
		recordPR := s.Store.AppendEvent
		if publications, ok := s.Store.(store.VerificationDeliveryStore); ok {
			recordPR = publications.RecordVerificationPullRequest
		}
		if err = recordPR(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{
			"url": prURL, "number": target.Number, "base_sha": target.Base.SHA, "head_sha": headSHA,
			"repository": repo.GitHub, "work_order_id": order.ID, "evidence_ids": evidenceIDs,
			"forge_author_class": core.ForgeAuthorExecutingUser, "forge_author_user_id": authorID,
		})}); err != nil {
			return nil, fmt.Errorf("record reviewed PR head: %w", err)
		}
	}
	freshness := s.refreshContext(ctx, order, "", "context.verdict_refresh_observed")
	if err = s.completeImplementationSubmission(ctx, order, headSHA); err != nil {
		return nil, err
	}
	if order.ReasonCode == "merge-conflict" {
		baseline := order.BaselineSHA
		if baseline == "" {
			baseline = task.ApprovedHeadSHA
		}
		if _, err = s.Store.MarkTaskApprovalStale(ctx, task.ID, baseline, reviewedHead, conflictRefreshScope(task), "merge-conflict"); err != nil {
			return nil, err
		}
	} else if task.ApprovalStale && reviewedHead != "" && reviewedHead != task.RefreshHeadSHA {
		// A fix submitted while the approval is stale must retarget the
		// refresh review to the pushed head; each refresh seat order
		// contracts the baseline and the new head (component-work-orders), so leaving
		// the recorded head behind would review a snapshot that predates
		// the fix on every subsequent round.
		if err = s.Store.AdvanceTaskRefreshHead(ctx, task.ID, reviewedHead); err != nil {
			return nil, err
		}
	}
	nextStage := core.StageReview
	if task.SetupContract.VerifyStage {
		nextStage = core.StageVerify
	}
	if _, err = taskops.New(s.Store).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: nextStage, ProjectStages: true}); err != nil {
		return nil, err
	}
	reviewExecution := cfg.Routing.Stages["review"].Execution
	if nextStage == core.StageReview && reviewExecution == config.ExecutionInProcess {
		if err = s.Dispatcher.DispatchNow(ctx, task.ID); err != nil {
			return nil, err
		}
		result, resultErr := s.latestReviewResult(ctx, task.ID, order.UpdatedAt)
		if resultErr != nil {
			return nil, resultErr
		}
		result["context_freshness"] = freshness
		result["pr_url"] = prURL
		result["review_execution"] = reviewExecution
		result["await_review"] = false
		return result, nil
	}
	s.Dispatcher.Enqueue(ctx, task.ID)
	result := map[string]any{"pr_url": prURL, "review_execution": reviewExecution, "await_review": true, "context_freshness": freshness}
	if nextStage == core.StageVerify {
		result["next_stage"] = nextStage
	}
	return result, nil
}

// observeSubmissionPR reads the branch PR immediately and re-reads a valid PR
// on the task branch and base whose head still differs from the pushed head,
// because GitHub updates a PR head asynchronously after a push. Only exact
// equality is accepted; exhaustion keeps the pull_request_head_mismatch
// refusal for the final observed head. Missing, mis-based, malformed, and
// forge-failed reads keep their existing refusal without mismatch retries.
func (s *Service) observeSubmissionPR(ctx, forgeCtx context.Context, lookup func(context.Context, string, string) (github.SubmissionPullRequest, error), repo, branch, base, head string) (github.SubmissionPullRequest, error) {
	for attempt := 0; ; attempt++ {
		target, err := lookup(forgeCtx, repo, branch)
		if err != nil {
			return github.SubmissionPullRequest{}, fmt.Errorf("read pull request for branch %s expected head %s: %w", branch, head, err)
		}
		lagging := target.Number > 0 && target.Head.Ref == branch && target.Base.Ref == base && target.Head.SHA != head
		if !lagging || attempt >= len(submissionPRHeadBackoff) {
			if err = github.ValidateSubmissionPR(target, branch, base, head); err != nil {
				return github.SubmissionPullRequest{}, err
			}
			return target, nil
		}
		if err = s.waitSubmissionPR(ctx, submissionPRHeadBackoff[attempt]); err != nil {
			return github.SubmissionPullRequest{}, fmt.Errorf("read pull request for branch %s expected head %s observed head %s: %w", branch, head, target.Head.SHA, err)
		}
	}
}

func (s *Service) waitSubmissionPR(ctx context.Context, delay time.Duration) error {
	if s.SubmissionPRWait != nil {
		return s.SubmissionPRWait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// submissionJobReady validates the job selected by the order itself, never
// the task's latest job.
func (s *Service) submissionJobReady(ctx context.Context, order core.WorkOrder) error {
	jobs, err := s.Store.ListJobs(ctx, order.TaskID)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.ID == order.JobID {
			return store.ValidateSubmissionJob(order, job, true)
		}
	}
	return store.ValidateSubmissionJob(order, core.Job{}, false)
}

// completeImplementationSubmission records the submitted order and completes
// its own job atomically; a completion failure leaves neither written.
func (s *Service) completeImplementationSubmission(ctx context.Context, order core.WorkOrder, headSHA string) error {
	order.State = core.WorkOrderSubmitted
	order.HeadSHA = headSHA
	_, err := taskops.ExecuteWorkOrder(ctx, s.Store, order.TaskID, core.WorkOrderCmdSubmitForReview, func(lease taskops.TaskLease) (core.Job, error) {
		return s.Store.SubmitImplementationCommand(ctx, lease, store.ImplementationSubmission{Order: order, EndedAt: time.Now().UTC()})
	})
	return err
}

// conflictRefreshScope is the minimum refresh scope for an authored conflict
// resolution: the frozen policy, with none raised to delta.
func conflictRefreshScope(task core.Task) string {
	scope := task.SetupContract.RefreshReview
	if scope == "" || scope == config.RefreshReviewNone {
		scope = config.RefreshReviewDelta
	}
	return scope
}

// dispatchedRefresh is the proven refresh already engaged for a conflict-fix
// head: its current unsuperseded stage orders and the round they form.
type dispatchedRefresh struct {
	Stage  core.Stage
	Round  int
	Orders []string
}

// dispatchedConflictRefresh proves that a refresh for exactly this claimed
// conflict-fix order's head already exists. The task must be in the same
// workspace with a stale approval bound to the order's baseline and head at
// no less than the conflict refresh scope, and the newest unsuperseded refresh
// orders of the task's frozen next stage must contract that same baseline,
// head, and scope. A matching task field or stale approval alone proves
// nothing.
func (s *Service) dispatchedConflictRefresh(ctx context.Context, task core.Task, order core.WorkOrder, head string) (dispatchedRefresh, bool, error) {
	if order.ReasonCode != "merge-conflict" || (order.State != core.WorkOrderClaimed && order.State != core.WorkOrderSubmitted) || order.TaskID != task.ID {
		return dispatchedRefresh{}, false, nil
	}
	baseline := order.BaselineSHA
	if baseline == "" {
		baseline = task.ApprovedHeadSHA
	}
	minimum := conflictRefreshScope(task)
	if !task.ApprovalStale || baseline == "" || task.RefreshBaselineSHA != baseline || task.RefreshHeadSHA != head || !refreshScopeCovers(task.RefreshReviewScope, minimum) {
		return dispatchedRefresh{}, false, nil
	}
	stage := core.StageReview
	if task.SetupContract.VerifyStage {
		stage = core.StageVerify
	}
	orders, err := s.Store.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		return dispatchedRefresh{}, false, err
	}
	events, err := s.Store.ListEvents(ctx, task.ID)
	if err != nil {
		return dispatchedRefresh{}, false, err
	}
	// Only stage orders created after this conflict-fix order can belong to
	// a refresh for its head; older rounds are unrelated history.
	current := make([]core.WorkOrder, 0)
	for _, candidate := range store.CurrentReviewOrders(orders, events) {
		if candidate.Stage == stage && !candidate.CreatedAt.Before(order.CreatedAt) {
			current = append(current, candidate)
		}
	}
	refresh := dispatchedRefresh{Stage: stage}
	if stage == core.StageReview {
		for _, candidate := range current {
			if candidate.ReviewRound > refresh.Round {
				refresh.Round = candidate.ReviewRound
			}
		}
	}
	for _, candidate := range current {
		if stage == core.StageReview && candidate.ReviewRound != refresh.Round {
			continue
		}
		if candidate.State == core.WorkOrderCancelled || candidate.State == core.WorkOrderStale || candidate.State == core.WorkOrderTimedOut {
			return dispatchedRefresh{}, false, nil
		}
		if candidate.HeadSHA != head || candidate.BaselineSHA != baseline || !refreshScopeCovers(candidate.ReviewScope, minimum) {
			return dispatchedRefresh{}, false, nil
		}
		if stage == core.StageReview && candidate.ReviewKind != "refresh" {
			return dispatchedRefresh{}, false, nil
		}
		refresh.Orders = append(refresh.Orders, candidate.ID)
	}
	if len(refresh.Orders) == 0 {
		return dispatchedRefresh{}, false, nil
	}
	return refresh, true, nil
}

func refreshScopeCovers(scope, minimum string) bool {
	return scope == minimum || scope == config.RefreshReviewFull
}

// completeDispatchedRefresh completes the claimed conflict-fix order and its
// own job once. The existing refresh keeps its round, seats, and dispatch.
func (s *Service) completeDispatchedRefresh(ctx context.Context, cfg *config.Config, task core.Task, order core.WorkOrder, head string, refresh dispatchedRefresh) (map[string]any, error) {
	if err := s.submissionJobReady(ctx, order); err != nil {
		return nil, err
	}
	if err := s.completeImplementationSubmission(ctx, order, head); err != nil {
		return nil, err
	}
	result := map[string]any{"pr_url": s.recordedPRURL(ctx, task.ID), "review_execution": cfg.Routing.Stages["review"].Execution, "await_review": true, "refresh_already_dispatched": true}
	if refresh.Stage == core.StageVerify {
		result["next_stage"] = refresh.Stage
	} else {
		result["review_round"] = refresh.Round
	}
	return result, nil
}

// submissionReplayResult answers a same-session, same-head replay of an
// already submitted order from durable state alone: no PR, context, order,
// job, approval, stage, or queue write. Success requires proof that the whole
// handoff was accepted, not only the atomic order/job completion: either the
// first task transition after this order's submission record is its own
// stage.advance from running, or, for a conflict fix completed against an
// existing refresh, that head-bound refresh is still the current one. A
// partial submission, such as one whose stale-approval mark or stage advance
// failed after completion, is refused explicitly and never reported as a
// handoff.
func (s *Service) submissionReplayResult(ctx context.Context, order core.WorkOrder) (map[string]any, error) {
	jobs, err := s.Store.ListJobs(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	completed := false
	for _, job := range jobs {
		if job.ID == order.JobID {
			completed = job.State == core.JobDone
		}
	}
	if !completed {
		return nil, fmt.Errorf("work order %s was submitted at head %s but its job %s is not complete; recovery is an operator action", order.ID, order.HeadSHA, order.JobID)
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	advanced, err := s.submissionStageAdvanced(ctx, order)
	if err != nil {
		return nil, err
	}
	refresh, refreshed := dispatchedRefresh{}, false
	if !advanced {
		if refresh, refreshed, err = s.dispatchedConflictRefresh(ctx, task, order, order.HeadSHA); err != nil {
			return nil, err
		}
	}
	if !advanced && !refreshed {
		return nil, fmt.Errorf("work order %s was submitted at head %s but its review handoff was not recorded (no stage advance or head-bound refresh); the submission is partial and recovery is an operator action", order.ID, order.HeadSHA)
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return nil, err
	}
	if task.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(task.SetupContract)
	}
	result := map[string]any{"pr_url": s.recordedPRURL(ctx, task.ID, order.ID), "review_execution": cfg.Routing.Stages["review"].Execution, "await_review": true, "replayed": true}
	if refreshed {
		result["pr_url"] = s.recordedPRURL(ctx, task.ID)
		result["refresh_already_dispatched"] = true
		if refresh.Stage == core.StageReview {
			result["review_round"] = refresh.Round
		}
	}
	if task.SetupContract.VerifyStage {
		result["next_stage"] = core.StageVerify
	}
	return result, nil
}

// submissionStageAdvanced reports whether the first task transition recorded
// after this order's submission is the submission's own stage.advance from
// running. Stale marking and refresh-head advancement precede that advance in
// the same locked handoff, so the advance proves them too.
func (s *Service) submissionStageAdvanced(ctx context.Context, order core.WorkOrder) (bool, error) {
	events, err := s.Store.ListEvents(ctx, order.TaskID)
	if err != nil {
		return false, err
	}
	submitted := -1
	for i, event := range events {
		if event.Kind != "work_order.updated" || event.JobID != order.JobID {
			continue
		}
		var recorded struct {
			ID      string              `json:"id"`
			State   core.WorkOrderState `json:"state"`
			HeadSHA string              `json:"head_sha"`
		}
		if json.Unmarshal(event.Payload, &recorded) == nil && recorded.ID == order.ID && recorded.State == core.WorkOrderSubmitted && recorded.HeadSHA == order.HeadSHA {
			submitted = i
			break
		}
	}
	if submitted < 0 {
		return false, nil
	}
	for _, event := range events[submitted+1:] {
		if event.Kind != "task.state_changed" {
			continue
		}
		var transition struct {
			From    core.TaskState   `json:"from"`
			Command core.TaskCommand `json:"command"`
		}
		if json.Unmarshal(event.Payload, &transition) != nil {
			return false, nil
		}
		return transition.Command == core.TaskStageAdvance && transition.From == core.TaskRunning, nil
	}
	return false, nil
}

// recordedPRURL reads the newest recorded pull_request.opened URL, optionally
// for one work order.
func (s *Service) recordedPRURL(ctx context.Context, taskID string, workOrderID ...string) string {
	events, err := s.Store.ListEvents(ctx, taskID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "pull_request.opened" {
			continue
		}
		var payload struct {
			URL         string `json:"url"`
			WorkOrderID string `json:"work_order_id"`
		}
		if json.Unmarshal(events[i].Payload, &payload) != nil {
			continue
		}
		if len(workOrderID) > 0 && payload.WorkOrderID != workOrderID[0] {
			continue
		}
		return payload.URL
	}
	return ""
}

// PullRequestTemplate is the server-composed delivery contract; it contains no
// forge credential (req-delivery-and-forge AC-2.4; req-delivery-and-forge
// AC-7.1).
type PullRequestTemplate struct {
	TaskID        string `json:"task_id"`
	Repository    string `json:"repository"`
	RepositoryURL string `json:"repository_url"`
	Branch        string `json:"branch"`
	Base          string `json:"base"`
	Title         string `json:"title"`
	Body          string `json:"body"`
}

// PullRequestTemplate is a read. Its own submitted session may reread it so a
// conveyor submit retry can replay an already accepted handoff at the same
// head; submit_for_review still decides admission.
func (s *Service) PullRequestTemplate(ctx context.Context, id, session string) (PullRequestTemplate, error) {
	var result PullRequestTemplate
	order, err := s.authorizedForObservation(ctx, id, session)
	if err != nil {
		return result, err
	}
	if order.Stage != core.StageImplement {
		return result, fmt.Errorf("work order %s is not implement", id)
	}
	if err = s.enforce(ctx, order); err != nil {
		return result, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return result, err
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return result, err
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok || repo.GitHub == "" {
		return result, fmt.Errorf("GitHub repository is required for submission")
	}
	evidence, err := s.taskVerificationEvidence(ctx, task.ID)
	if err != nil {
		return result, err
	}
	return PullRequestTemplate{TaskID: task.ID, Repository: repo.GitHub, RepositoryURL: repo.URL, Branch: task.Branch, Base: task.BaseBranch, Title: task.Title, Body: dispatch.PRBody(task, evidence...)}, nil
}

func (s *Service) taskVerificationEvidence(ctx context.Context, taskID string) ([]core.Artifact, error) {
	artifacts, err := s.Store.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageTask, ID: taskID}})
	if err != nil {
		return nil, fmt.Errorf("list verification evidence: %w", err)
	}
	evidence := make([]core.Artifact, 0)
	for _, artifact := range artifacts {
		if artifact.TaskID == taskID && artifact.EligibleVerificationEvidence() {
			artifact.DownloadURL = ""
			evidence = append(evidence, artifact)
		}
	}
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].CreatedAt.Equal(evidence[j].CreatedAt) {
			return evidence[i].ID < evidence[j].ID
		}
		return evidence[i].CreatedAt.Before(evidence[j].CreatedAt)
	})
	return evidence, nil
}
