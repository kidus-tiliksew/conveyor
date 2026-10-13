package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// ErrConflictFixPending reports a merge or readiness action deferred because a
// queued or claimed conflict-fix implement order owns the task's conflict
// episode. The merge-readiness sweep treats it as ordinary pending work.
var ErrConflictFixPending = errors.New("conflict fix pending")

type MergeReadiness struct {
	State   string `json:"state"`
	HeadSHA string `json:"head_sha,omitempty"`
	URL     string `json:"url,omitempty"`
	Number  int    `json:"number,omitempty"`
}

func (d *Dispatcher) reviewedHeadFromEvents(ctx context.Context, taskID string) string {
	events, _ := d.Store.ListEvents(ctx, taskID)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_completed" && events[i].Kind != "review.completed" {
			continue
		}
		var payload struct {
			ApprovedHeadSHA   string `json:"approved_head_sha"`
			ReviewedCommitSHA string `json:"reviewed_commit_sha"`
		}
		if json.Unmarshal(events[i].Payload, &payload) == nil {
			if payload.ApprovedHeadSHA != "" {
				return payload.ApprovedHeadSHA
			}
			if payload.ReviewedCommitSHA != "" {
				return payload.ReviewedCommitSHA
			}
		}
	}
	return ""
}

// observedMergeExpectedHead is the head an observed forge merge must match.
// Callers decide eligibility; the writer has no state filter.
func (d *Dispatcher) observedMergeExpectedHead(ctx context.Context, task core.Task) (string, bool) {
	switch task.State {
	case core.TaskApproved:
		if task.ApprovalStale {
			return "", false
		}
		head := task.ApprovedHeadSHA
		if head == "" {
			head = task.ReviewedHeadSHA
		}
		return head, head != ""
	case core.TaskAwaiting:
		if task.GateKind != core.GateKindMergeApproval {
			return "", false
		}
		head := task.ReviewedHeadSHA
		if head == "" {
			head = d.reviewedHeadFromEvents(ctx, task.ID)
		}
		return head, head != ""
	default:
		return "", false
	}
}

func forgeMergeReconciled(events []core.Event, repository string, pr github.PullRequest) bool {
	for _, event := range events {
		if event.Kind != "merge.reconciled" {
			continue
		}
		var prior struct {
			Repository string `json:"repository"`
			Number     int    `json:"pull_request"`
			Head       string `json:"head_sha"`
			Merge      string `json:"merge_commit_sha"`
		}
		if json.Unmarshal(event.Payload, &prior) == nil && prior.Repository == repository && prior.Number == pr.Number && prior.Head == pr.HeadSHA && prior.Merge == pr.MergeCommitSHA {
			return true
		}
	}
	return false
}

func mergedHeadMismatchRecorded(events []core.Event, pr github.PullRequest) bool {
	for _, event := range events {
		if event.Kind != "merge.failed" {
			continue
		}
		var prior struct {
			Reason string `json:"reason_code"`
			Number int    `json:"pull_request"`
			Merged string `json:"merged_head_sha"`
			Commit string `json:"merge_commit_sha"`
		}
		if json.Unmarshal(event.Payload, &prior) == nil && prior.Reason == "merged_head_mismatch" && prior.Number == pr.Number && prior.Merged == pr.HeadSHA && prior.Commit == pr.MergeCommitSHA {
			return true
		}
	}
	return false
}

// reconcileForgeMergeLocked records an observed forge merge. The caller holds
// the task side-effect lock, pr.Merged is true, and the task is not merged.
// A match confirms the merge. A mismatch records drift and leaves the state.
func (d *Dispatcher) reconcileForgeMergeLocked(ctx context.Context, task core.Task, githubRepo string, pr github.PullRequest, expectedHead, result string) (bool, error) {
	if expectedHead != "" && expectedHead == pr.HeadSHA {
		events, err := d.Store.ListEvents(ctx, task.ID)
		if err != nil {
			return false, err
		}
		if !forgeMergeReconciled(events, githubRepo, pr) {
			payload := map[string]any{
				"repository": githubRepo, "pull_request": pr.Number, "url": pr.URL,
				"base_sha": pr.BaseSHA, "head_sha": pr.HeadSHA, "result": result,
				"merged_by": pr.MergedBy, "merge_commit_sha": pr.MergeCommitSHA,
				"factory_review_validated": true,
				"reviewed_head_sha":        task.ReviewedHeadSHA, "approved_head_sha": task.ApprovedHeadSHA,
			}
			if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.reconciled", Payload: core.JSONPayload(payload)}); err != nil {
				return false, err
			}
		}
		d.observeConfirmedMerge(ctx, task, githubRepo, pr, "merge.reconciled")
		if err := d.confirmTaskMerged(ctx, task.ID); err != nil {
			return false, err
		}
		return true, nil
	}
	events, err := d.Store.ListEvents(ctx, task.ID)
	if err != nil {
		return false, err
	}
	if mergedHeadMismatchRecorded(events, pr) {
		return false, nil
	}
	mismatch := fmt.Sprintf("pull request %s#%d merged at %s, which does not match expected head %s", githubRepo, pr.Number, pr.HeadSHA, expectedHead)
	payload := map[string]any{
		"reason_code": "merged_head_mismatch", "forge_error_category": "merged_head_mismatch",
		"pull_request": pr.Number, "url": pr.URL,
		"merged_head_sha": pr.HeadSHA, "expected_head_sha": expectedHead,
		"merge_commit_sha": pr.MergeCommitSHA, "merged_by": pr.MergedBy,
		"error": mismatch,
	}
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.failed", Payload: core.JSONPayload(payload)}); err != nil {
		return false, err
	}
	return false, nil
}

func (d *Dispatcher) refreshScope(task core.Task, conflict bool) string {
	scope := task.SetupContract.RefreshReview
	if scope == "" {
		scope = config.RefreshReviewDelta
	}
	if conflict && scope == config.RefreshReviewNone {
		scope = config.RefreshReviewDelta
	}
	return scope
}

func (d *Dispatcher) beginRefreshLocked(ctx context.Context, task core.Task, newHead, reason string, conflict bool) error {
	baseline := task.ApprovedHeadSHA
	if baseline == "" {
		baseline = task.ReviewedHeadSHA
	}
	if baseline == "" || newHead == "" || baseline == newHead {
		return fmt.Errorf("task %s requires distinct approved and current heads for refresh", task.ID)
	}
	scope := d.refreshScope(task, conflict)
	// A refresh already engaged for this exact head pair and scope must not be
	// re-marked: every re-mark re-runs the recover transition, demoting a task
	// whose refresh round may hold a claimed, deliberating seat — observed
	// live as a once-per-poll demotion loop that no completed verdict could
	// survive. The store repeats this guard atomically for concurrent observers;
	// only a changed head pair or a rebound approval starts a new episode.
	if task.ApprovalStale && task.RefreshBaselineSHA == baseline && task.RefreshHeadSHA == newHead && task.RefreshReviewScope == scope {
		return nil
	}
	created, err := d.Store.MarkTaskApprovalStale(ctx, task.ID, baseline, newHead, scope, reason)
	if err != nil {
		return err
	}
	if !created {
		return nil
	}
	if scope == config.RefreshReviewNone && !conflict && !task.SetupContract.VerifyStage {
		return d.Store.SkipTaskRefresh(ctx, task.ID, newHead, "clean-update")
	}
	command := core.TaskRefreshReview
	if task.State != core.TaskApproved {
		command = core.TaskRecoverRefresh
	}
	nextStage := core.StageReview
	if task.SetupContract.VerifyStage {
		nextStage = core.StageVerify
	}
	if err := d.transition(ctx, task.ID, command, nextStage, ""); err != nil {
		return err
	}
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	if current.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(current.SetupContract)
	}
	if current.NextStage == core.StageVerify {
		// Readiness has just observed this head and holds the task lock, so
		// the pre-dispatch base check is skipped here.
		return d.dispatchSnapshot(ctx, current, false)
	}
	return d.createReviewRound(ctx, cfg, current, cfg.Routing.Stages[string(core.StageReview)])
}

// ReadMergeReadiness resolves the gate-facing PR state with bounded backoff.
// UNKNOWN is an ordinary pending result and never creates merge.failed noise.
func (d *Dispatcher) ReadMergeReadiness(ctx context.Context, task core.Task) (MergeReadiness, error) {
	var result MergeReadiness
	err := d.Store.WithTaskSideEffectLock(ctx, task.ID, func(lockedCtx context.Context) error {
		var lockedErr error
		result, lockedErr = d.readMergeReadinessLocked(lockedCtx, task)
		return lockedErr
	})
	return result, err
}

func (d *Dispatcher) readMergeReadinessLocked(ctx context.Context, task core.Task) (MergeReadiness, error) {
	var result MergeReadiness
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return result, err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return result, err
	}
	repo, ok := cfg.Repo(current.Repo)
	if !ok || repo.GitHub == "" {
		return result, fmt.Errorf("repository %q does not configure GitHub", current.Repo)
	}
	pr, err := d.readPullRequestMergeability(ctx, repo.GitHub, current.Branch)
	if err != nil {
		return result, err
	}
	result = MergeReadiness{State: pr.Mergeable, HeadSHA: pr.HeadSHA, URL: pr.URL, Number: pr.Number}
	if pr.Merged {
		if current.State == core.TaskMerged {
			result.State = "MERGED"
			return result, nil
		}
		if pr.MergeCommitSHA == "" {
			result.State = "UNKNOWN"
			return result, nil
		}
		head, eligible := d.observedMergeExpectedHead(ctx, current)
		if !eligible {
			result.State = "MERGED_HEAD_MISMATCH"
			return result, nil
		}
		matched, matchErr := d.reconcileForgeMergeLocked(ctx, current, repo.GitHub, pr, head, "merged_outside_conveyor")
		if matchErr != nil {
			return result, matchErr
		}
		if matched {
			result.State = "MERGED"
		} else {
			result.State = "MERGED_HEAD_MISMATCH"
		}
		return result, nil
	}
	// A merge-gate wait is read-only except for the merged short-circuit above.
	// Conflict fixes and stale refreshes would bind an approval the operator never gave.
	// Queued and running reads keep the existing conflict and refresh path.
	if current.State == core.TaskAwaiting {
		return result, nil
	}
	// A queued or claimed conflict-fix order owns the conflict episode and its
	// refresh: readiness stays pending and mutates nothing until the order's
	// own submission (component-work-orders).
	if _, pending, pendingErr := d.activeImplementationWorkOrder(ctx, current.ID, "merge-conflict"); pendingErr != nil {
		return result, pendingErr
	} else if pending {
		result.State = "UNKNOWN"
		return result, nil
	}
	approved := current.ApprovedHeadSHA
	if approved == "" {
		approved = current.ReviewedHeadSHA
	}
	if result.State == "UNKNOWN" {
		return result, nil
	}
	if result.State == "CONFLICTING" {
		if err = d.recordMergeConflictBlocked(ctx, current, approved, pr.HeadSHA, "merge-conflict"); err != nil {
			return result, err
		}
		// Gate-on and gate-off both dispatch. This read never requests a merge.
		_, err = d.dispatchConflictFixLocked(ctx, current, pr, cfg, false, "")
		return result, err
	}
	if err = d.clearMergeConflictEpisode(ctx, current, result.State, pr.HeadSHA); err != nil {
		return result, err
	}
	if approved != "" && pr.HeadSHA != "" && approved != pr.HeadSHA {
		if err = d.beginRefreshLocked(ctx, current, pr.HeadSHA, "head-changed", false); err != nil {
			return result, err
		}
		result.State = "STALE"
	}
	return result, nil
}

// readPullRequestMergeability reads the branch pull request through the App at
// most three times, waiting 100ms then 200ms while GitHub still computes
// mergeability. A forge error is returned with its category; an answer that is
// still UNKNOWN after the third read is returned as is (component-submission-merge).
func (d *Dispatcher) readPullRequestMergeability(ctx context.Context, githubRepo, branch string) (github.PullRequest, error) {
	var pr github.PullRequest
	var err error
	for attempt, delay := 0, 100*time.Millisecond; attempt < 3; attempt, delay = attempt+1, delay*2 {
		pr, err = d.ViewPullRequest(ctx, githubRepo, branch)
		if err != nil {
			if category := github.ErrorCategory(err); category != "" {
				return github.PullRequest{}, fmt.Errorf("%s: %w", category, err)
			}
			return github.PullRequest{}, err
		}
		if pr.Merged || pr.Mergeable != "UNKNOWN" {
			break
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return github.PullRequest{}, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return pr, nil
}

type mergeConflictEpisode struct {
	ApprovedHead string `json:"approved_head"`
	NewHead      string `json:"new_head"`
}

type mergeConflictDispatchState struct {
	Failures        int
	NextRetryAt     time.Time
	Exhausted       bool
	RecoveryBlocked bool
}

// currentMergeConflictEpisode reads the append-only task history while the
// caller holds the task lock. Only clearing the conflict or observing a new
// approved/head pair ends the prior episode; terminal attempts remain inside
// the unresolved episode and may be retried under its backoff budget.
func (d *Dispatcher) currentMergeConflictEpisode(ctx context.Context, taskID string) (mergeConflictEpisode, bool, error) {
	events, err := d.Store.ListEvents(ctx, taskID)
	if err != nil {
		return mergeConflictEpisode{}, false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Kind {
		case "merge.conflict_cleared":
			return mergeConflictEpisode{}, false, nil
		case "merge.blocked":
			var episode mergeConflictEpisode
			if err := json.Unmarshal(event.Payload, &episode); err != nil {
				return mergeConflictEpisode{}, false, fmt.Errorf("decode merge conflict episode: %w", err)
			}
			return episode, true, nil
		}
	}
	return mergeConflictEpisode{}, false, nil
}

func sameConflictEpisode(payload json.RawMessage, episode mergeConflictEpisode) bool {
	var candidate mergeConflictEpisode
	return json.Unmarshal(payload, &candidate) == nil && candidate == episode
}

func (d *Dispatcher) conflictDispatchState(ctx context.Context, taskID string, episode mergeConflictEpisode, orders []core.WorkOrder) (mergeConflictDispatchState, error) {
	events, err := d.Store.ListEvents(ctx, taskID)
	if err != nil {
		return mergeConflictDispatchState{}, err
	}
	state := store.MergeConflictDispatchState(events, episode.ApprovedHead, episode.NewHead, orders)
	return mergeConflictDispatchState{Failures: state.Failures, NextRetryAt: state.NextRetryAt, Exhausted: state.Exhausted, RecoveryBlocked: state.RecoveryBlocked}, nil
}

func terminalConflictFixAttempt(order core.WorkOrder) bool {
	switch order.State {
	case core.WorkOrderCancelled, core.WorkOrderStale, core.WorkOrderTimedOut:
		return true
	case core.WorkOrderQueued, core.WorkOrderCompleted:
		return order.RetrySuppressed || order.LastAttemptOutcome != "" || order.LastFailureMessage != ""
	default:
		return false
	}
}

func (d *Dispatcher) recordMergeConflictBlocked(ctx context.Context, task core.Task, approvedHead, newHead, reasonCode string) error {
	episode, active, err := d.currentMergeConflictEpisode(ctx, task.ID)
	if err != nil {
		return err
	}
	if active && episode.ApprovedHead == approvedHead && episode.NewHead == newHead {
		return nil
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.blocked", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": task.ID, "reason_code": reasonCode, "approved_head": approvedHead, "new_head": newHead})})
}

func (d *Dispatcher) clearMergeConflictEpisode(ctx context.Context, task core.Task, readiness, head string) error {
	episode, active, err := d.currentMergeConflictEpisode(ctx, task.ID)
	if err != nil || !active {
		return err
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.conflict_cleared", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": task.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "readiness": readiness, "observed_head": head})})
}

func (d *Dispatcher) DispatchConflictFix(ctx context.Context, task core.Task) (core.WorkOrder, error) {
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	repo, ok := cfg.Repo(current.Repo)
	if !ok || repo.GitHub == "" {
		return core.WorkOrder{}, fmt.Errorf("repository %q does not configure GitHub", current.Repo)
	}
	pr, err := d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
	if err != nil {
		return core.WorkOrder{}, err
	}
	// The forge operation above is observational. Re-enter the task lock and
	// refresh state before the durable command so watcher and queue paths share
	// one serialized admission decision.
	var result core.WorkOrder
	err = d.Store.WithTaskSideEffectLock(ctx, current.ID, func(lockedCtx context.Context) error {
		var lockedErr error
		current, lockedErr = d.Store.GetTask(lockedCtx, current.ID)
		if lockedErr != nil {
			return lockedErr
		}
		approved := current.ApprovedHeadSHA
		if approved == "" {
			approved = current.ReviewedHeadSHA
		}
		if lockedErr = d.recordMergeConflictBlocked(lockedCtx, current, approved, pr.HeadSHA, "merge-conflict"); lockedErr != nil {
			return lockedErr
		}
		result, lockedErr = d.dispatchConflictFixLocked(lockedCtx, current, pr, cfg, true, "")
		return lockedErr
	})
	return result, err
}

// dispatchConflictFixLocked admits one conflict-fix implement order under the
// episode budget. An empty baselineHead is the approved-task path: the fix's
// baseline is the approved head, bound first if absent. A non-empty
// baselineHead is the pre-review path (feature-bounded-automatic-recovery
// AR-4): the conflicting submitted head is the baseline and no approval is bound.
func (d *Dispatcher) dispatchConflictFixLocked(ctx context.Context, current core.Task, pr github.PullRequest, cfg *config.Config, operatorRetry bool, baselineHead string) (core.WorkOrder, error) {
	active, ok, err := d.activeImplementationWorkOrder(ctx, current.ID, "merge-conflict")
	if err != nil {
		return core.WorkOrder{}, err
	}
	if ok {
		return active, nil
	}
	if pr.Mergeable != "CONFLICTING" {
		return core.WorkOrder{}, fmt.Errorf("pull request is not conflicting (%s)", pr.Mergeable)
	}
	episode, present, err := d.currentMergeConflictEpisode(ctx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if !present {
		approved, reasonCode := current.ApprovedHeadSHA, "pre-review-conflict"
		if baselineHead == "" {
			reasonCode = "merge-conflict"
			if approved == "" {
				approved = current.ReviewedHeadSHA
			}
		}
		if err = d.recordMergeConflictBlocked(ctx, current, approved, pr.HeadSHA, reasonCode); err != nil {
			return core.WorkOrder{}, err
		}
		episode = mergeConflictEpisode{ApprovedHead: approved, NewHead: pr.HeadSHA}
	}
	orders, err := d.Store.ListTaskWorkOrders(ctx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	events, err := d.Store.ListEvents(ctx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	dispatchState, err := d.conflictDispatchState(ctx, current.ID, episode, orders)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if recovery := store.InterruptedReviewRecoveryNeeded(current, store.CurrentReviewOrders(orders, events), events); recovery != nil {
		if !dispatchState.RecoveryBlocked {
			err = d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.conflict_recovery_blocked", Payload: core.JSONPayload(map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "review_round": recovery.ReviewRound, "error": recovery.Reason})})
		}
		return core.WorkOrder{}, err
	}
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now().UTC()
	}
	if dispatchState.Failures >= 3 && !dispatchState.Exhausted {
		payload := map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "failure_count": dispatchState.Failures, "retry_suppressed": true, "error": "conflict-fix replacement budget exhausted"}
		if err = d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.conflict_dispatch_exhausted", Payload: core.JSONPayload(payload)}); err != nil {
			return core.WorkOrder{}, err
		}
		dispatchState.Exhausted = true
	}
	// Automatic readiness and the merge act keep the episode budget. The
	// operator route retries after exhaustion and during backoff.
	if !operatorRetry && (dispatchState.Exhausted || (!dispatchState.NextRetryAt.IsZero() && now.Before(dispatchState.NextRetryAt))) {
		return core.WorkOrder{}, nil
	}
	if baselineHead == "" && current.ApprovedHeadSHA == "" {
		head := current.ReviewedHeadSHA
		if head == "" {
			head = pr.HeadSHA
		}
		if err = d.Store.BindTaskApproval(ctx, current.ID, head); err != nil {
			return core.WorkOrder{}, err
		}
		current.ApprovedHeadSHA = head
	}
	systemCtx := store.WithActor(ctx, store.SystemActor("system"))
	comment, baseline, newHead := "Merge the base branch into the task branch, resolve conflicts, validate, push, and submit for refresh review.", current.ApprovedHeadSHA, pr.HeadSHA
	if baselineHead != "" {
		comment, baseline, newHead = "Merge the base branch into the task branch, resolve conflicts, validate, push, and submit again for verification and review.", baselineHead, baselineHead
	}
	intervention := core.Intervention{TaskID: current.ID, ActorID: "system", ActorRole: core.ActorSystem, Action: core.InterventionRedirect, ReasonCode: "merge-conflict", Comment: comment}
	if current.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithPolicy(current.SetupContract)
	}
	if _, err = store.ServedRequirementsForTask(systemCtx, d.Store, current.ID, config.ServedRequirementAuthorityNodes(cfg)); err != nil {
		return core.WorkOrder{}, err
	}
	prior, err := d.Store.ListJobs(systemCtx, current.ID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	attempt := 1
	for _, job := range prior {
		if job.Stage == core.StageImplement {
			attempt++
		}
	}
	route := cfg.Routing.Stages[string(core.StageImplement)]
	jobID := fmt.Sprintf("%s-%s-%d", current.ID, core.StageImplement, attempt)
	job := core.Job{ID: jobID, TaskID: current.ID, Stage: core.StageImplement, Harness: "external-mcp", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
	queueTimeout := cfg.WorkOrderQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	order := core.WorkOrder{ID: jobID, TaskID: current.ID, JobID: jobID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, ReasonCode: "merge-conflict", BaselineSHA: baseline, ExecutionTimeoutText: route.TimeoutText, QueueEnteredAt: now, QueueDeadline: now.Add(queueTimeout), CreatedAt: now}
	request := store.ConflictFixRequest{TaskID: current.ID, Job: job, WorkOrder: order, Intervention: intervention, ApprovedHead: current.ApprovedHeadSHA, NewHead: newHead}
	var result store.ConflictFixResult
	_, err = taskops.ExecuteWorkOrder(systemCtx, d.Store, current.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (bool, error) {
		var commandErr error
		result, commandErr = d.Store.CreateConflictFixCommand(systemCtx, lease, request)
		return result.Created, commandErr
	})
	if errors.Is(err, store.ErrConflictReviewRecovery) {
		if !dispatchState.RecoveryBlocked {
			err = d.Store.AppendEvent(systemCtx, core.Event{TaskID: current.ID, Kind: "merge.conflict_recovery_blocked", Payload: core.JSONPayload(map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "error": err.Error()})})
		}
		return core.WorkOrder{}, err
	}
	if err != nil {
		failures := dispatchState.Failures + 1
		delay := time.Minute << (failures - 1)
		if delay > 15*time.Minute {
			delay = 15 * time.Minute
		}
		nextRetry := now.Add(delay)
		payload := map[string]any{"workspace": current.Workspace, "task_id": current.ID, "reason_code": "merge-conflict", "approved_head": episode.ApprovedHead, "new_head": episode.NewHead, "failure_count": failures, "next_retry_at": nextRetry, "error": err.Error()}
		if eventErr := d.Store.AppendEvent(systemCtx, core.Event{TaskID: current.ID, Kind: "merge.conflict_dispatch_failed", Payload: core.JSONPayload(payload)}); eventErr != nil {
			return core.WorkOrder{}, fmt.Errorf("conflict dispatch failed: %v; record retry: %w", err, eventErr)
		}
		if failures >= 3 && !dispatchState.Exhausted {
			payload["retry_suppressed"] = true
			if eventErr := d.Store.AppendEvent(systemCtx, core.Event{TaskID: current.ID, Kind: "merge.conflict_dispatch_exhausted", Payload: core.JSONPayload(payload)}); eventErr != nil {
				return core.WorkOrder{}, fmt.Errorf("conflict dispatch failed: %v; record exhaustion: %w", err, eventErr)
			}
		}
		return core.WorkOrder{}, err
	}
	if result.Created {
		d.Enqueue(systemCtx, current.ID)
	}
	return result.WorkOrder, nil
}

// gateOnConflictMergeError keeps the operator merge act failing closed. A nil
// return on an unmerged task becomes the misleading 409 "merge was not
// confirmed by the forge".
func (d *Dispatcher) gateOnConflictMergeError(ctx context.Context, current core.Task, githubRepo string, number int, order core.WorkOrder) error {
	if order.ID != "" {
		return fmt.Errorf("pull request %s#%d has merge conflicts; conflict fix %s dispatched", githubRepo, number, order.ID)
	}
	reason := "budget exhausted"
	episode, present, err := d.currentMergeConflictEpisode(ctx, current.ID)
	if err != nil {
		return err
	}
	if present {
		orders, listErr := d.Store.ListTaskWorkOrders(ctx, current.ID)
		if listErr != nil {
			return listErr
		}
		state, stateErr := d.conflictDispatchState(ctx, current.ID, episode, orders)
		if stateErr != nil {
			return stateErr
		}
		switch {
		case state.RecoveryBlocked:
			reason = "interrupted review recovery blocked"
		case state.Exhausted:
			reason = "budget exhausted"
		case !state.NextRetryAt.IsZero():
			reason = "backing off until " + state.NextRetryAt.UTC().Format(time.RFC3339)
		}
	}
	return fmt.Errorf("pull request %s#%d has merge conflicts; conflict fix not dispatched (%s)", githubRepo, number, reason)
}

// MergeApprovedTask performs the final human-gate transition. A durable-store
// task lock serializes browser retries across control-plane instances; the
// authoritative pre-merge read makes retries after a process restart safe by
// reconciling a PR that GitHub already merged (component-work-orders).
func (d *Dispatcher) MergeApprovedTask(ctx context.Context, task core.Task) error {
	return d.Store.WithTaskSideEffectLock(ctx, task.ID, func(lockedCtx context.Context) error {
		return d.mergeApprovedTaskLocked(lockedCtx, task)
	})
}

func (d *Dispatcher) mergeApprovedTaskLocked(ctx context.Context, task core.Task) error {
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.State == core.TaskMerged {
		return nil
	}
	if current.State != core.TaskApproved {
		if (current.State != core.TaskQueued && current.State != core.TaskRunning) || current.MergeApproval {
			return fmt.Errorf("task %s is not approved for merge", task.ID)
		}
		recoverable, evidenceErr := d.hasRecoverableApprovedReview(ctx, current)
		if evidenceErr != nil {
			return evidenceErr
		}
		if !recoverable {
			return fmt.Errorf("task %s has no accepted review evidence for merge recovery", task.ID)
		}
	}
	author := core.ForgeAuthoringIdentity{Class: core.ForgeAuthorWorkspace}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return d.recordMergeFailureWithAuthor(ctx, current, "workspace_config_unavailable", fmt.Errorf("load workspace repository configuration: %w", err), author)
	}
	repo, ok := cfg.Repo(current.Repo)
	if !ok || strings.TrimSpace(repo.GitHub) == "" {
		return d.recordMergeFailureWithAuthor(ctx, current, "unsupported_repository", fmt.Errorf("repository %q does not configure GitHub; merge it in its forge and retry reconciliation", current.Repo), author)
	}

	pr, err := d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
	if err != nil {
		if errors.Is(err, github.ErrPullRequestNotFound) {
			return d.recordMergeFailureWithAuthor(ctx, current, "missing_pull_request", fmt.Errorf("no pull request found for branch %s; push it and submit it for review before merging: %w", current.Branch, err), author)
		}
		return d.recordMergeFailureWithAuthor(ctx, current, "pull_request_lookup_failed", fmt.Errorf("could not read the pull request for branch %s; verify GitHub authentication and retry: %w", current.Branch, err), author)
	}
	if pr.Merged {
		if current.ApprovalStale {
			return fmt.Errorf("approval is stale for task %s; refresh review before reconciling the merged pull request", current.ID)
		}
		expected := current.ApprovedHeadSHA
		if expected == "" {
			expected = current.ReviewedHeadSHA
		}
		matched, reconcileErr := d.reconcileForgeMergeLocked(ctx, current, repo.GitHub, pr, expected, "already_merged")
		if reconcileErr != nil {
			return reconcileErr
		}
		if !matched {
			return fmt.Errorf("observed merge head %s does not match expected head %s for task %s", pr.HeadSHA, expected, current.ID)
		}
		return nil
	}
	// A queued or claimed conflict-fix order defers conflict clearing,
	// refresh, and merge requests; its own submission drives the refresh
	// round (req-review-gates-evidence AC-4.1, AC-4.2).
	if active, pending, pendingErr := d.activeImplementationWorkOrder(ctx, current.ID, "merge-conflict"); pendingErr != nil {
		return pendingErr
	} else if pending {
		return fmt.Errorf("%w: work order %s is %s", ErrConflictFixPending, active.ID, active.State)
	}
	author, mergeMessage, err := d.mergeAuthor(ctx, current)
	if err != nil {
		return d.recordMergeFailureWithAuthor(ctx, current, "merge_author_unavailable", err, author)
	}
	if pr.State != "open" {
		return d.recordMergeFailureWithAuthor(ctx, current, "pull_request_not_open", fmt.Errorf("pull request %s#%d is %s without a merge; reopen or replace it and retry", repo.GitHub, pr.Number, pr.State), author)
	}
	approvedHead := current.ApprovedHeadSHA
	if approvedHead == "" {
		approvedHead = current.ReviewedHeadSHA
	}
	if approvedHead == "" && pr.HeadSHA != "" {
		// One-time compatibility binding for a task approved before it recorded
		// an approved head: approval binds to the reviewed head (DEC-59(6);
		// component-submission-merge).
		approvedHead = pr.HeadSHA
		if err = d.Store.BindTaskApproval(ctx, current.ID, approvedHead); err != nil {
			return err
		}
	}
	if pr.Mergeable == "UNKNOWN" {
		return fmt.Errorf("pull request %s#%d merge readiness is still pending", repo.GitHub, pr.Number)
	}
	if pr.Mergeable == "CONFLICTING" {
		if err = d.recordMergeConflictBlocked(ctx, current, approvedHead, pr.HeadSHA, "merge-conflict"); err != nil {
			return err
		}
		order, dispatchErr := d.dispatchConflictFixLocked(ctx, current, pr, cfg, false, "")
		if !current.MergeApproval {
			return dispatchErr
		}
		if dispatchErr != nil {
			return dispatchErr
		}
		return d.gateOnConflictMergeError(ctx, current, repo.GitHub, pr.Number, order)
	}
	if err = d.clearMergeConflictEpisode(ctx, current, pr.Mergeable, pr.HeadSHA); err != nil {
		return err
	}
	if approvedHead != "" && pr.HeadSHA != "" && approvedHead != pr.HeadSHA {
		if err = d.beginRefreshLocked(ctx, current, pr.HeadSHA, "head-changed", false); err != nil {
			return err
		}
		return fmt.Errorf("approval is stale: reviewed head %s differs from current head %s; refresh review dispatched", approvedHead, pr.HeadSHA)
	}
	if pr.Mergeable != "MERGEABLE" {
		return d.recordMergeFailureWithAuthor(ctx, current, "pull_request_not_mergeable", fmt.Errorf("pull request %s#%d is not mergeable (%s); update the branch or resolve required checks and retry", repo.GitHub, pr.Number, pr.Mergeable), author)
	}
	requestPayload := map[string]any{"repository": repo.GitHub, "pull_request": pr.Number, "url": pr.URL}
	addForgeAuthor(requestPayload, author)
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.requested", Payload: core.JSONPayload(requestPayload)}); err != nil {
		return err
	}
	requestMerge := d.RequestMerge
	ctx = github.WithMergeMessage(ctx, mergeMessage)
	if err := requestMerge(ctx, repo.GitHub, pr.Number); err != nil {
		return d.recordMergeFailureWithAuthor(ctx, current, "forge_merge_failed", fmt.Errorf("GitHub could not merge pull request %s#%d; resolve required checks or branch protection and retry: %w", repo.GitHub, pr.Number, err), author)
	}
	confirmed, err := d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
	if err != nil {
		return d.recordMergeFailureWithAuthor(ctx, current, "merge_verification_failed", fmt.Errorf("GitHub accepted the merge request but Conveyor could not verify pull request %s#%d; inspect it and retry reconciliation: %w", repo.GitHub, pr.Number, err), author)
	}
	if !confirmed.Merged {
		return d.recordMergeFailureWithAuthor(ctx, current, "merge_unconfirmed", fmt.Errorf("GitHub did not confirm pull request %s#%d as merged; inspect checks or merge-queue status and retry", repo.GitHub, pr.Number), author)
	}
	confirmedPayload := map[string]any{"repository": repo.GitHub, "pull_request": confirmed.Number, "url": confirmed.URL, "base_sha": confirmed.BaseSHA, "head_sha": confirmed.HeadSHA, "merged_by": confirmed.MergedBy, "merge_commit_sha": confirmed.MergeCommitSHA}
	addForgeAuthor(confirmedPayload, author)
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(confirmedPayload)}); err != nil {
		return err
	}
	d.observeConfirmedMerge(ctx, current, repo.GitHub, confirmed, "merge.confirmed")
	return d.confirmTaskMerged(ctx, current.ID)
}

// ReconcileObservedPullRequest records a forge merge at the reviewed head.
// The monitor reports evidence; the dispatcher retains transition authority (AC-3.5).
func (d *Dispatcher) ReconcileObservedPullRequest(ctx context.Context, repository, githubRepo, taskID string, pr github.PullRequest) (bool, error) {
	reconciled := false
	err := d.Store.WithTaskSideEffectLock(ctx, taskID, func(lockedCtx context.Context) error {
		task, err := d.Store.GetTask(lockedCtx, taskID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !pr.Merged {
			return nil
		}
		events, err := d.Store.ListEvents(lockedCtx, taskID)
		if err != nil {
			return err
		}
		cfg, err := d.currentConfig(lockedCtx)
		if err != nil {
			return err
		}
		repo, ok := cfg.Repo(repository)
		if !ok || repo.GitHub != githubRepo || !monitor.RecordedLineage(task, events, repository, githubRepo, taskID, pr.HeadRef, pr.Number, pr.HeadSHA) {
			return nil
		}
		if pr.MergeCommitSHA == "" {
			return nil
		}
		if task.State == core.TaskMerged {
			if pr.HeadSHA != "" && (pr.HeadSHA == task.ApprovedHeadSHA || pr.HeadSHA == task.ReviewedHeadSHA) {
				reconciled = true
			}
			return nil
		}
		head, eligible := d.observedMergeExpectedHead(lockedCtx, task)
		if !eligible {
			return nil
		}
		matched, matchErr := d.reconcileForgeMergeLocked(lockedCtx, task, githubRepo, pr, head, "merged_outside_conveyor")
		if matchErr != nil {
			return matchErr
		}
		reconciled = matched
		return nil
	})
	return reconciled, err
}

// observeConfirmedMerge is deliberately non-gating: merge confirmation stays
// authoritative when GitHub file retrieval or drift evaluation fails. The
// observation commit is the PR head SHA recorded in the causal merge event,
// even when the landed squash or merge-commit SHA differs.
func (d *Dispatcher) observeConfirmedMerge(ctx context.Context, task core.Task, githubRepo string, pr github.PullRequest, eventKind string) {
	if d.ObserveDesignMerge == nil {
		return
	}
	events, err := d.Store.ListEvents(ctx, task.ID)
	eventID := int64(0)
	if err == nil {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Kind == eventKind {
				eventID = events[i].ID
				break
			}
		}
	}
	if err == nil && eventID == 0 {
		err = fmt.Errorf("causal %s event was not readable after append", eventKind)
	}
	var paths []string
	if err == nil {
		paths, err = d.ListPullRequestFiles(ctx, githubRepo, pr.Number)
	}
	if err == nil {
		err = d.ObserveDesignMerge(ctx, monitor.Observation{
			Repository: task.Repo, Kind: monitor.LineagedMerge, OccurrenceID: "pr:" + strconv.Itoa(pr.Number),
			SourceURL: pr.URL, CommitSHA: pr.HeadSHA, PullRequestNumber: pr.Number,
			ChangedPaths: paths, CausalEventID: eventID,
		}, task.ID)
	}
	if err != nil {
		if auditor, ok := d.Store.(interface {
			AuditMonitor(context.Context, string, map[string]any) error
		}); ok {
			_ = auditor.AuditMonitor(ctx, "system_design.drift_evaluation_failed", map[string]any{
				"task_id": task.ID, "merge_event_id": eventID, "repository": task.Repo,
				"github_repository": githubRepo, "pull_request": pr.Number, "reason": err.Error(),
			})
		}
	}
}

func (d *Dispatcher) hasRecoverableApprovedReview(ctx context.Context, task core.Task) (bool, error) {
	if task.ApprovedHeadSHA == "" && task.ReviewedHeadSHA == "" {
		return false, nil
	}
	events, err := d.Store.ListEvents(ctx, task.ID)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_completed" {
			continue
		}
		var payload struct {
			Verdict string `json:"verdict"`
		}
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			return false, fmt.Errorf("decode latest review resolution for %s: %w", task.ID, err)
		}
		return payload.Verdict == "approve", nil
	}
	return false, nil
}

// ReconcileMergeReadiness sweeps gate-on approved and merge-gate tasks for an
// already-merged pull request, then recovers gate-off accepted reviews whose
// approved-to-merge edge was interrupted. The gate-on pass never requests a
// merge. Every candidate is revalidated under the task side-effect lock.
func (d *Dispatcher) ReconcileMergeReadiness(ctx context.Context) (int, error) {
	tasks, err := d.Store.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	reconciled := 0
	var gateOnErrs []error
	for _, task := range tasks {
		if !task.MergeApproval || (task.State != core.TaskApproved && !(task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval)) {
			continue
		}
		before := task.State
		if _, readErr := d.ReadMergeReadiness(ctx, task); readErr != nil {
			gateOnErrs = append(gateOnErrs, fmt.Errorf("task %s: %w", task.ID, readErr))
			continue
		}
		after, getErr := d.Store.GetTask(ctx, task.ID)
		if getErr != nil {
			gateOnErrs = append(gateOnErrs, fmt.Errorf("task %s: %w", task.ID, getErr))
			continue
		}
		if before != after.State {
			reconciled++
		}
	}
	for _, task := range tasks {
		if task.MergeApproval || task.State == core.TaskMerged || task.State == core.TaskClosed || task.State == core.TaskParked || task.State == core.TaskAwaiting {
			continue
		}
		if task.State != core.TaskApproved {
			recoverable, evidenceErr := d.hasRecoverableApprovedReview(ctx, task)
			if evidenceErr != nil {
				return reconciled, errors.Join(append(gateOnErrs, evidenceErr)...)
			}
			if !recoverable {
				continue
			}
		}
		before := task.State
		if err = d.MergeApprovedTask(ctx, task); err != nil {
			if errors.Is(err, ErrConflictFixPending) {
				continue
			}
			after, getErr := d.Store.GetTask(ctx, task.ID)
			if getErr != nil {
				return reconciled, errors.Join(append(gateOnErrs, getErr)...)
			}
			if after.ApprovalStale || (after.State == core.TaskQueued && after.State != before) {
				reconciled++
				continue
			}
			return reconciled, errors.Join(append(gateOnErrs, err)...)
		}
		after, getErr := d.Store.GetTask(ctx, task.ID)
		if getErr != nil {
			return reconciled, errors.Join(append(gateOnErrs, getErr)...)
		}
		if before != after.State || after.State == core.TaskMerged {
			reconciled++
		}
	}
	return reconciled, errors.Join(gateOnErrs...)
}

func (d *Dispatcher) confirmTaskMerged(ctx context.Context, taskID string) error {
	// The task transition atomically resumes dependent queue clocks. Worker
	// polling discovers the now-claimable order; the old durable-queue nudge
	// was a no-op because the existing stage order already owns dispatch.
	current, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	command := core.TaskMergeConfirm
	if current.State != core.TaskApproved {
		command = core.TaskMergeRecover
	}
	return d.transition(ctx, taskID, command, "", "")
}

func (d *Dispatcher) recordMergeFailureWithAuthor(ctx context.Context, task core.Task, reason string, mergeErr error, author core.ForgeAuthoringIdentity) error {
	payload := map[string]any{"reason_code": reason, "error": mergeErr.Error()}
	if category := github.ErrorCategory(mergeErr); category != "" {
		payload["forge_error_category"] = category
	}
	if author.Class != "" {
		payload["forge_author_class"] = author.Class
	}
	if author.UserID != "" {
		payload["forge_author_user_id"] = author.UserID
		payload["approving_operator_user_id"] = author.UserID
		payload["approving_operator_display_name"] = author.DisplayName
	}
	if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.failed", Payload: core.JSONPayload(payload)}); err != nil {
		return fmt.Errorf("%v; record merge failure: %w", mergeErr, err)
	}
	return mergeErr
}

func addForgeAuthor(payload map[string]any, author core.ForgeAuthoringIdentity) {
	payload["forge_author_class"] = author.Class
	if author.UserID != "" {
		payload["forge_author_user_id"] = author.UserID
		payload["approving_operator_user_id"] = author.UserID
		payload["approving_operator_display_name"] = author.DisplayName
	}
}

func (d *Dispatcher) mergeAuthor(ctx context.Context, task core.Task) (core.ForgeAuthoringIdentity, string, error) {
	author := core.ForgeAuthoringIdentity{Class: core.ForgeAuthorWorkspace}
	userID, ok, err := store.ApprovingOperatorUserID(ctx, d.Store, task.ID)
	if err != nil {
		return author, "", fmt.Errorf("resolve approving operator: %w", err)
	}
	if !ok || strings.TrimSpace(userID) == "" {
		if task.MergeApproval {
			return author, "", github.PermissionError(fmt.Errorf("merge requires the approving operator identity"))
		}
		return author, "", nil
	}
	identities, ok := d.Store.(store.CallerIdentityStore)
	if !ok {
		return author, "", fmt.Errorf("approving operator identity store is unavailable")
	}
	identity, err := store.GitAuthorForUser(ctx, identities, userID)
	if err != nil {
		return author, "", err
	}
	if strings.ContainsAny(identity.Name+identity.Email, "\r\n") {
		return author, "", fmt.Errorf("approving operator identity contains a newline")
	}
	author.UserID, author.DisplayName = userID, identity.Name
	return author, fmt.Sprintf("Approved-by: %s <%s>", identity.Name, identity.Email), nil
}
