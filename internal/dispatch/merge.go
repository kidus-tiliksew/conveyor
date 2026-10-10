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
		return d.runTaskForSnapshot(ctx, current)
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
	var pr github.PullRequest
	for attempt, delay := 0, 100*time.Millisecond; attempt < 3; attempt, delay = attempt+1, delay*2 {
		pr, err = d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
		if err != nil {
			if category := github.ErrorCategory(err); category != "" {
				return result, fmt.Errorf("%s: %w", category, err)
			}
			return result, err
		}
		if pr.Mergeable != "UNKNOWN" {
			break
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	result = MergeReadiness{State: pr.Mergeable, HeadSHA: pr.HeadSHA, URL: pr.URL, Number: pr.Number}
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
		if err = d.recordMergeConflictBlocked(ctx, current, approved, pr.HeadSHA); err != nil {
			return result, err
		}
		if !current.MergeApproval {
			_, err = d.dispatchConflictFixLocked(ctx, current, pr, cfg)
		}
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
	state := mergeConflictDispatchState{}
	inside := false
	dispatched := map[string]bool{}
	for _, event := range events {
		switch event.Kind {
		case "merge.conflict_cleared":
			inside, state = false, mergeConflictDispatchState{}
		case "merge.blocked":
			inside = sameConflictEpisode(event.Payload, episode)
			state = mergeConflictDispatchState{}
		case "merge.conflict_fix_dispatched":
			if inside && sameConflictEpisode(event.Payload, episode) {
				var payload struct {
					WorkOrderID string `json:"work_order_id"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID != "" {
					dispatched[payload.WorkOrderID] = true
				}
				state.NextRetryAt = time.Time{}
			}
		case "merge.conflict_dispatch_failed":
			if !inside || !sameConflictEpisode(event.Payload, episode) {
				continue
			}
			var payload struct {
				FailureCount int       `json:"failure_count"`
				NextRetryAt  time.Time `json:"next_retry_at"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				state.Failures++
				state.NextRetryAt = payload.NextRetryAt
			}
		case "merge.conflict_dispatch_exhausted":
			if inside && sameConflictEpisode(event.Payload, episode) {
				state.Exhausted = true
			}
		case "merge.conflict_recovery_blocked":
			if inside && sameConflictEpisode(event.Payload, episode) {
				state.RecoveryBlocked = true
			}
		}
	}
	for _, order := range orders {
		if dispatched[order.ID] && terminalConflictFixAttempt(order) {
			state.Failures++
		}
	}
	return state, nil
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

func (d *Dispatcher) recordMergeConflictBlocked(ctx context.Context, task core.Task, approvedHead, newHead string) error {
	episode, active, err := d.currentMergeConflictEpisode(ctx, task.ID)
	if err != nil {
		return err
	}
	if active && episode.ApprovedHead == approvedHead && episode.NewHead == newHead {
		return nil
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.blocked", Payload: core.JSONPayload(map[string]any{"workspace": task.Workspace, "task_id": task.ID, "reason_code": "merge-conflict", "approved_head": approvedHead, "new_head": newHead})})
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
		if lockedErr = d.recordMergeConflictBlocked(lockedCtx, current, approved, pr.HeadSHA); lockedErr != nil {
			return lockedErr
		}
		result, lockedErr = d.dispatchConflictFixLocked(lockedCtx, current, pr, cfg)
		return lockedErr
	})
	return result, err
}

func (d *Dispatcher) dispatchConflictFixLocked(ctx context.Context, current core.Task, pr github.PullRequest, cfg *config.Config) (core.WorkOrder, error) {
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
		approved := current.ApprovedHeadSHA
		if approved == "" {
			approved = current.ReviewedHeadSHA
		}
		if err = d.recordMergeConflictBlocked(ctx, current, approved, pr.HeadSHA); err != nil {
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
	if dispatchState.Exhausted || (!dispatchState.NextRetryAt.IsZero() && now.Before(dispatchState.NextRetryAt)) {
		return core.WorkOrder{}, nil
	}
	if current.ApprovedHeadSHA == "" {
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
	intervention := core.Intervention{TaskID: current.ID, ActorID: "system", ActorRole: core.ActorSystem, Action: core.InterventionRedirect, ReasonCode: "merge-conflict", Comment: "Merge the base branch into the task branch, resolve conflicts, validate, push, and submit for refresh review."}
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
	order := core.WorkOrder{ID: jobID, TaskID: current.ID, JobID: jobID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, ReasonCode: "merge-conflict", BaselineSHA: current.ApprovedHeadSHA, ExecutionTimeoutText: route.TimeoutText, QueueEnteredAt: now, QueueDeadline: now.Add(queueTimeout), CreatedAt: now}
	request := store.ConflictFixRequest{TaskID: current.ID, Job: job, WorkOrder: order, Intervention: intervention, ApprovedHead: current.ApprovedHeadSHA, NewHead: pr.HeadSHA}
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
		return d.reconcileObservedMergeLocked(ctx, current, repo.GitHub, pr)
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
		if err = d.recordMergeConflictBlocked(ctx, current, approvedHead, pr.HeadSHA); err != nil {
			return err
		}
		if !current.MergeApproval {
			_, err = d.dispatchConflictFixLocked(ctx, current, pr, cfg)
			return err
		}
		return fmt.Errorf("pull request %s#%d has merge conflicts; dispatch the conflict fix", repo.GitHub, pr.Number)
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

// ReconcileObservedPullRequest accepts only approved, recorded lineage. The
// monitor reports evidence; the dispatcher retains transition authority (AC-3.5).
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
		if !pr.Merged || (task.State != core.TaskApproved && task.State != core.TaskMerged) || task.ApprovalStale {
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
		head := task.ApprovedHeadSHA
		if head == "" {
			head = task.ReviewedHeadSHA
		}
		if head == "" || head != pr.HeadSHA || pr.MergeCommitSHA == "" {
			return nil
		}
		if task.State == core.TaskMerged {
			reconciled = true
			return nil
		}
		if err := d.reconcileObservedMergeLocked(lockedCtx, task, githubRepo, pr); err != nil {
			return err
		}
		reconciled = true
		return nil
	})
	return reconciled, err
}

func (d *Dispatcher) reconcileObservedMergeLocked(ctx context.Context, task core.Task, repository string, pr github.PullRequest) error {
	head := task.ApprovedHeadSHA
	if head == "" {
		head = task.ReviewedHeadSHA
	}
	if head == "" || head != pr.HeadSHA || task.ApprovalStale {
		return fmt.Errorf("observed merge does not match the approved head for task %s", task.ID)
	}
	events, err := d.Store.ListEvents(ctx, task.ID)
	if err != nil {
		return err
	}
	recorded := false
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
			recorded = true
			break
		}
	}
	if !recorded {
		payload := map[string]any{"repository": repository, "pull_request": pr.Number, "url": pr.URL, "base_sha": pr.BaseSHA, "head_sha": pr.HeadSHA, "result": "already_merged", "merged_by": pr.MergedBy, "merge_commit_sha": pr.MergeCommitSHA, "factory_review_validated": true, "reviewed_head_sha": task.ReviewedHeadSHA, "approved_head_sha": task.ApprovedHeadSHA}
		if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.reconciled", Payload: core.JSONPayload(payload)}); err != nil {
			return err
		}
	}
	d.observeConfirmedMerge(ctx, task, repository, pr, "merge.reconciled")
	return d.confirmTaskMerged(ctx, task.ID)
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

// ReconcileMergeReadiness is a level-triggered sweep for accepted review
// evidence whose approved-to-merge edge was interrupted. Every candidate is
// revalidated under the task side-effect lock before a forge mutation.
func (d *Dispatcher) ReconcileMergeReadiness(ctx context.Context) (int, error) {
	tasks, err := d.Store.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	reconciled := 0
	for _, task := range tasks {
		if task.MergeApproval || task.State == core.TaskMerged || task.State == core.TaskClosed || task.State == core.TaskParked || task.State == core.TaskAwaiting {
			continue
		}
		if task.State != core.TaskApproved {
			recoverable, evidenceErr := d.hasRecoverableApprovedReview(ctx, task)
			if evidenceErr != nil {
				return reconciled, evidenceErr
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
				return reconciled, getErr
			}
			if after.ApprovalStale || (after.State == core.TaskQueued && after.State != before) {
				reconciled++
				continue
			}
			return reconciled, err
		}
		after, getErr := d.Store.GetTask(ctx, task.ID)
		if getErr != nil {
			return reconciled, getErr
		}
		if before != after.State || after.State == core.TaskMerged {
			reconciled++
		}
	}
	return reconciled, nil
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
