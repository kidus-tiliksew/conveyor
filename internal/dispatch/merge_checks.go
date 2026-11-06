package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// The check status of the approved head, read before an automatic merge with
// the merge gate off (req-review-gates-evidence AC-1.1, AC-1.4 to AC-1.7;
// component-submission-merge, Check status; DEC-108(5) to (7)). The operator's
// merge act and the merge gate read none (AC-1.8).

// CheckStatusDeadlineWindow is how long after the first read that finds a check
// pending, or that fails, the deadline falls due (AC-1.5).
const CheckStatusDeadlineWindow = 2 * time.Hour

// CheckReadRemedy is the workspace-settings remedy a refused read names
// (req-delivery-and-forge AC-1.8).
const CheckReadRemedy = "Open workspace settings, grant the workspace GitHub App Checks read on GitHub, and accept the changed permission on its installation, or replace the App in workspace settings."

var (
	// ErrCheckStatusHeld reports a merge that a failed or stuck check, or a
	// refused read, holds. The hold is recorded; a later read decides again.
	ErrCheckStatusHeld = errors.New("check status holds the merge")
	// ErrCheckStatusPending reports a merge waiting on a pending check with
	// none failed. The wait is recorded and the factory stays the actor.
	ErrCheckStatusPending = errors.New("check status is pending")
	// ErrCheckStatusUnread reports a read with no answer, which is no pass: of the
	// check status, or of the pull request an automatic merge reads first. It
	// wraps the forge error, so a retry and a rate-limit pause can see it.
	ErrCheckStatusUnread = errors.New("check status could not be read")
)

// unreadPullRequestError is an automatic merge's read of the pull request with
// no answer (AC-1.10; component-submission-merge invariant 63). It counts as
// ErrCheckStatusUnread, and failure is the fixed text its events name it by.
type unreadPullRequestError struct {
	failure string
	err     error
}

func (e *unreadPullRequestError) Error() string        { return e.err.Error() }
func (e *unreadPullRequestError) Unwrap() error        { return e.err }
func (e *unreadPullRequestError) Is(target error) bool { return target == ErrCheckStatusUnread }

// unreadPullRequestApplies selects the merges invariant 63 governs: the automatic
// merge of an approved gate-off task with a head to record the deadline at. The
// operator's act, a gate-on task, and a queued or running task finishing through
// the accepted-review path stay outside it.
func unreadPullRequestApplies(task core.Task, act mergeAct) bool {
	return act.operator == nil && checkReadRowApplies(task) && store.CheckStatusHead(task) != ""
}

// recordUnreadPullRequest applies invariant 63: the read counts as a failed read,
// so it records the deadline and reads no check status. A deadline's own merge
// records nothing here, because the deadline holds instead of waiting again. The
// wait is recorded before the caller's merge.failed, so a process that dies
// between them still has a deadline to wake it.
func (d *Dispatcher) recordUnreadPullRequest(ctx context.Context, task core.Task, act mergeAct, failure string, cause error) error {
	unread := &unreadPullRequestError{failure: failure, err: cause}
	if act.checkStatus != "" {
		return unread
	}
	head := store.CheckStatusHead(task)
	standing, err := d.foldCheckStatus(ctx, task.ID, head)
	if err != nil {
		return err
	}
	if err := d.recordUnansweredRead(ctx, task, head, standing, "read_failed", failure, cause); err != nil {
		return err
	}
	return unread
}

// pullRequestReadFailure is the fixed text an event names a failed pull-request
// read by, as checkReadFailure does for a check-status read.
func pullRequestReadFailure(err error) string {
	if category := github.ErrorCategory(err); category != "" {
		return fmt.Sprintf("The pull-request read failed (%s).", category)
	}
	return "The pull-request read failed."
}

// pullRequestMergeabilityUnknown names GitHub's UNKNOWN answer.
const pullRequestMergeabilityUnknown = "GitHub has not settled the pull request's mergeability."

// checkStatusDeferred reports the outcomes that leave an approved task
// unmerged with the reason recorded, which a verdict's caller cannot act on.
func checkStatusDeferred(err error) bool {
	return errors.Is(err, ErrCheckStatusHeld) || errors.Is(err, ErrCheckStatusPending) || errors.Is(err, ErrCheckStatusUnread)
}

// newCommitChecksReader is the reader New wires. The package's tests replace it
// so a fixture stubbing only ViewPullRequest never reads a real forge.
var newCommitChecksReader = realCommitChecksReader

func realCommitChecksReader(d *Dispatcher) func(context.Context, string, string) (github.CheckStatus, error) {
	return func(ctx context.Context, repo, sha string) (github.CheckStatus, error) {
		forgeCtx, err := d.workspaceForgeContext(ctx, repo)
		if err != nil {
			return github.CheckStatus{}, err
		}
		return github.CommitChecks(forgeCtx, repo, sha)
	}
}

// NoChecksReportedForTest is the check reader of a fixture that builds a
// dispatcher outside this package and stubs its forge.
func NoChecksReportedForTest(context.Context, string, string) (github.CheckStatus, error) {
	return github.CheckStatus{}, nil
}

// checkHoldStands reads the ledger for a hold of head with the gate off.
func (d *Dispatcher) checkHoldStands(ctx context.Context, task core.Task, head string) (bool, error) {
	if task.MergeApproval {
		return false, nil
	}
	status, err := d.foldCheckStatus(ctx, task.ID, head)
	if err != nil {
		return false, err
	}
	return status.Hold != nil, nil
}

func (d *Dispatcher) foldCheckStatus(ctx context.Context, taskID, head string) (nextaction.CheckStatus, error) {
	events, err := d.Store.ListCheckStatusEvents(ctx, []string{taskID})
	if err != nil {
		return nextaction.CheckStatus{}, err
	}
	return store.FoldCheckStatus(events[taskID], head), nil
}

// applyCheckStatusLocked reads the head's check status and applies the first
// row of invariant 60 that matches. It returns the check_status merge.requested
// carries when the merge may go on, and a sentinel otherwise. The caller holds
// the task side-effect lock.
func (d *Dispatcher) applyCheckStatusLocked(ctx context.Context, task core.Task, githubRepo, head string) (string, error) {
	read, readErr := d.ReadCommitChecks(ctx, githubRepo, head)
	if readErr != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	standing, err := d.foldCheckStatus(ctx, task.ID, head)
	if err != nil {
		return "", err
	}
	if readErr != nil {
		return "", d.applyFailedCheckRead(ctx, task, head, standing, readErr)
	}
	failed, pending := read.Named(github.CheckFailed), read.Named(github.CheckPending)
	switch {
	case len(pending) > 0 && standing.StuckStands():
		// A stuck hold stays until a read finds no check pending, and records no
		// new deadline while it stands (AC-1.9; invariant 61). It keeps naming
		// what this read found. A permission hold that replaced it gives way to it
		// on the first answered read, with nothing cleared (AC-1.11; invariant 62).
		if err := d.recordCheckHold(ctx, task, head, standing.Hold, nextaction.CheckHoldStuck, append(failed, pending...), ""); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: head %s", ErrCheckStatusHeld, head)
	case len(failed) > 0:
		if len(pending) > 0 {
			if err := d.recordPendingDeadline(ctx, task, head, standing, pending); err != nil {
				return "", err
			}
		} else if err := d.clearCheckDeadline(ctx, task); err != nil {
			return "", err
		}
		if err := d.recordCheckHold(ctx, task, head, standing.Hold, nextaction.CheckHoldFailed, append(failed, pending...), ""); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: head %s has failed checks", ErrCheckStatusHeld, head)
	case len(pending) > 0:
		if err := d.recordPendingDeadline(ctx, task, head, standing, pending); err != nil {
			return "", err
		}
		if err := d.clearCheckHold(ctx, task, head, standing.Hold, "pending"); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: head %s has pending checks", ErrCheckStatusPending, head)
	}
	if err := d.clearCheckDeadline(ctx, task); err != nil {
		return "", err
	}
	// "None reported" also covers a head read seconds after a push, before its
	// checks register, and a workflow waiting on first-time-contributor approval.
	// DEC-108(5) accepts both: a head with no reported check merges.
	status := "none_reported"
	if len(read.Checks) > 0 {
		status = "passed"
	}
	if err := d.clearCheckHold(ctx, task, head, standing.Hold, status); err != nil {
		return "", err
	}
	return status, nil
}

// applyFailedCheckRead applies the two read-failure rows. Both record the
// deadline AC-1.5 asks of a failed read, unless a stuck hold or the permission
// hold that replaced it stands. Only a refused permission names the operator at
// once; a failed read clears nothing.
func (d *Dispatcher) applyFailedCheckRead(ctx context.Context, task core.Task, head string, standing nextaction.CheckStatus, readErr error) error {
	hold := standing.Hold
	permission := github.ErrorCategory(readErr) == github.ForgePermission
	reason := "read_failed"
	if permission {
		reason = nextaction.CheckHoldPermission
	}
	if err := d.recordUnansweredRead(ctx, task, head, standing, reason, checkReadFailure(readErr), readErr); err != nil {
		return err
	}
	if !permission {
		return fmt.Errorf("%w for head %s: %w", ErrCheckStatusUnread, head, readErr)
	}
	// The hold keeps naming what a standing hold named, so an override still
	// names each check the last read that found any named.
	var named []github.Check
	if hold != nil {
		named = checksFromRefs(hold.Checks)
	}
	if err := d.recordCheckHold(ctx, task, head, hold, nextaction.CheckHoldPermission, named, checkReadFailure(readErr)); err != nil {
		return err
	}
	return fmt.Errorf("%w: GitHub refused the read of head %s: %w", ErrCheckStatusHeld, head, readErr)
}

// recordUnansweredRead records merge.check_pending and the deadline for a read
// with no answer, unless StuckStands: that state records no new deadline.
func (d *Dispatcher) recordUnansweredRead(ctx context.Context, task core.Task, head string, standing nextaction.CheckStatus, reason, failure string, cause error) error {
	if standing.StuckStands() {
		return nil
	}
	payload := checkEventPayload(task, head, reason, nil)
	payload["read_failed"], payload["read_failure"] = true, failure
	if category := github.ErrorCategory(cause); category != "" {
		payload["forge_error_category"] = category
	}
	return d.recordCheckDeadline(ctx, task, head, standing, payload, reason != nextaction.CheckHoldPermission && !rateLimited(cause))
}

// checkReadFailure is the fixed text an event names a failed read by. The
// category carries the rest, so no response text reaches a surface.
func checkReadFailure(err error) string {
	switch category := github.ErrorCategory(err); category {
	case github.ForgePermission:
		return "GitHub refused the check-status read for lack of permission."
	case "":
		return "The check-status read failed."
	default:
		return fmt.Sprintf("The check-status read failed (%s).", category)
	}
}

func checkEventPayload(task core.Task, head, reason string, checks []github.Check) map[string]any {
	if checks == nil {
		checks = []github.Check{}
	}
	return map[string]any{"workspace": task.Workspace, "task_id": task.ID, "head_sha": head, "reason": reason, "checks": checks}
}

// recordPendingDeadline records the deadline of the first read that finds a
// check pending.
func (d *Dispatcher) recordPendingDeadline(ctx context.Context, task core.Task, head string, standing nextaction.CheckStatus, pending []github.Check) error {
	return d.recordCheckDeadline(ctx, task, head, standing, checkEventPayload(task, head, "pending", pending), false)
}

// recordCheckDeadline records the deadline and merge.check_pending together. A
// later read of the same head leaves the deadline where it is. It appends a
// further event only when the read changes between answered and unanswered, when
// an answered read finds different checks pending, when an unanswered read names
// a different failure, or when reopen is set and a failed read follows the end of
// the retries, so the wait reads truthfully and the job finds what is due. A
// failed read in a wait keeps naming what the wait's last answered read found
// pending, so the deadline can name it.
func (d *Dispatcher) recordCheckDeadline(ctx context.Context, task core.Task, head string, standing nextaction.CheckStatus, payload map[string]any, reopen bool) error {
	deadline := d.checkClock().Add(CheckStatusDeadlineWindow)
	payload["deadline"] = deadline
	event := core.Event{TaskID: task.ID, Kind: store.EventCheckPending, Payload: core.JSONPayload(payload)}
	recorded, err := d.Store.RecordCheckStatusDeadline(ctx, task.ID, head, deadline, event)
	if err != nil || recorded {
		return err
	}
	readFailed, _ := payload["read_failed"].(bool)
	changed := readFailed != (standing.ReadFailure != "")
	if failure, _ := payload["read_failure"].(string); readFailed && failure != standing.ReadFailure {
		changed = true
	}
	if pending, _ := payload["checks"].([]github.Check); !readFailed && !sameChecks(standing.Waiting, pending) {
		changed = true
	}
	if !changed && !(reopen && standing.RetriesExhausted) {
		d.announceCheckDeadline(task)
		return nil
	}
	if task.CheckStatusDeadline != nil {
		payload["deadline"] = task.CheckStatusDeadline.UTC()
	}
	if readFailed {
		payload["checks"] = waitingChecks(standing)
	}
	event.Payload = core.JSONPayload(payload)
	return d.Store.AppendEvent(ctx, event)
}

func (d *Dispatcher) clearCheckDeadline(ctx context.Context, task core.Task) error {
	_, err := d.Store.ClearCheckStatusDeadline(ctx, task.ID)
	return err
}

// waitingChecks is what a standing wait names, as an event payload carries it.
func waitingChecks(standing nextaction.CheckStatus) []nextaction.CheckRef {
	if standing.Waiting == nil {
		return []nextaction.CheckRef{}
	}
	return standing.Waiting
}

// recordCheckHold appends merge.check_hold unless the standing hold already
// says the same, so a repeated delivery appends nothing.
func (d *Dispatcher) recordCheckHold(ctx context.Context, task core.Task, head string, standing *nextaction.CheckHold, reason string, checks []github.Check, failure string) error {
	if standing != nil && standing.Reason == reason && standing.ReadFailure == failure && sameChecks(standing.Checks, checks) {
		return nil
	}
	payload := checkEventPayload(task, head, reason, checks)
	if failure != "" {
		payload["read_failure"] = failure
	}
	if reason == nextaction.CheckHoldPermission {
		payload["remedy"] = CheckReadRemedy
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: store.EventCheckHold, Payload: core.JSONPayload(payload)})
}

func sameChecks(recorded []nextaction.CheckRef, read []github.Check) bool {
	return slices.EqualFunc(recorded, read, func(a nextaction.CheckRef, b github.Check) bool {
		return a.Name == b.Name && a.State == b.State && a.URL == b.URL
	})
}

// clearCheckHold appends merge.check_hold_cleared when a hold stood.
func (d *Dispatcher) clearCheckHold(ctx context.Context, task core.Task, head string, standing *nextaction.CheckHold, status string) error {
	if standing == nil {
		return nil
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: store.EventCheckHoldCleared, Payload: core.JSONPayload(map[string]any{
		"workspace": task.Workspace, "task_id": task.ID, "head_sha": head, "reason": standing.Reason, "check_status": status,
	})})
}

// mergeActAuthor resolves who a merge is authored for. The operator's act on a
// gate-off task names the acting operator, because such a task usually records
// no approving one (AC-1.8; component-submission-merge invariant 42). Every
// other merge keeps the approving operator.
func (d *Dispatcher) mergeActAuthor(ctx context.Context, task core.Task, act mergeAct) (core.ForgeAuthoringIdentity, string, error) {
	if act.operator != nil && !task.MergeApproval {
		return d.gateMergeAuthor(ctx, *act.operator)
	}
	return d.mergeAuthor(ctx, task)
}

// recordCheckOverrideLocked appends merge.check_override naming the operator
// and each check the standing hold, which is what the last read named, named.
// A second call for the head appends nothing.
func (d *Dispatcher) recordCheckOverrideLocked(ctx context.Context, task core.Task, head, userID, displayName string) error {
	status, err := d.foldCheckStatus(ctx, task.ID, head)
	if err != nil || status.Hold == nil {
		return err
	}
	events, err := d.Store.ListCompactEvents(ctx, task.ID)
	if err != nil {
		return err
	}
	if checkOverrideRecorded(events, head) {
		return nil
	}
	checks := status.Hold.Checks
	if checks == nil {
		checks = []nextaction.CheckRef{}
	}
	return d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: store.EventCheckOverride, Payload: core.JSONPayload(map[string]any{
		"workspace": task.Workspace, "task_id": task.ID, "head_sha": head, "hold_reason": status.Hold.Reason, "checks": checks,
		"operator_user_id": userID, "operator_display_name": displayName,
	})})
}

func checkOverrideRecorded(events []core.Event, head string) bool {
	for _, event := range events {
		if event.Kind != store.EventCheckOverride {
			continue
		}
		var prior struct {
			Head string `json:"head_sha"`
		}
		if json.Unmarshal(event.Payload, &prior) == nil && prior.Head == head {
			return true
		}
	}
	return false
}

// recoverCheckOverrideLocked finishes an operator's act whose process died
// after GitHub merged the head and before the override was written. The latest
// operator merge.requested at the head that GitHub did not refuse names the
// operator. A merge on GitHub outside Conveyor has none, and records no override
// (req-delivery-and-forge AC-3.5).
func (d *Dispatcher) recoverCheckOverrideLocked(ctx context.Context, task core.Task, head string, events []core.Event) error {
	if task.MergeApproval {
		return nil
	}
	type operatorRequest struct {
		Operator    bool   `json:"operator_act"`
		Head        string `json:"head_sha"`
		UserID      string `json:"forge_author_user_id"`
		DisplayName string `json:"approving_operator_display_name"`
	}
	var request *operatorRequest
	for _, event := range events {
		switch event.Kind {
		case "merge.requested":
			var candidate operatorRequest
			if json.Unmarshal(event.Payload, &candidate) == nil && candidate.Operator && candidate.Head == head {
				request = &candidate
			}
		case "merge.failed":
			var failed struct {
				Reason string `json:"reason_code"`
			}
			if json.Unmarshal(event.Payload, &failed) == nil && failed.Reason == "forge_merge_failed" {
				request = nil
			}
		}
	}
	if request == nil {
		return nil
	}
	return d.recordCheckOverrideLocked(ctx, task, head, request.UserID, request.DisplayName)
}

// The retry of an unanswered check-status read is the task's dispatch_task job
// (component-submission-merge invariant 62; component-durable-queue invariant
// 37). A failed read enqueues it unless one is active. Each attempt reads once
// and holds nothing afterward: the queue's job.failed schedules the next attempt
// at the kind's delay, and the alarm wakes the workers for it. The job's attempts
// bound the retries.

// checkReadRetryDue reports whether the task's latest check-status read went
// unanswered and its retries are still open. Otherwise a job has nothing to read
// for: the head moved, the task left approved, a later read answered, GitHub
// refused the read, or the retries ended.
func (d *Dispatcher) checkReadRetryDue(ctx context.Context, task core.Task) (bool, nextaction.CheckStatus, error) {
	head := store.CheckStatusHead(task)
	if !checkReadRowApplies(task) || task.ApprovalStale || head == "" ||
		task.CheckStatusDeadline == nil || task.CheckStatusDeadlineHead != head {
		return false, nextaction.CheckStatus{}, nil
	}
	status, err := d.foldCheckStatus(ctx, task.ID, head)
	if err != nil {
		return false, status, err
	}
	refused := status.Hold != nil && status.Hold.Reason == nextaction.CheckHoldPermission
	return status.ReadFailure != "" && !status.RetriesExhausted && !refused, status, nil
}

// checkReadRowApplies selects the task invariant 37's check-status rows and its
// no-read completion govern: approved with the merge gate off.
func checkReadRowApplies(task core.Task) bool {
	return task.State == core.TaskApproved && !task.MergeApproval
}

// arrangeCheckReadRetry answers a failed read outside a job. A rate-limited read
// ends the retries at once. Any other enqueues the task's job, and an active job
// holds the stream.
func (d *Dispatcher) arrangeCheckReadRetry(ctx context.Context, taskID string, readErr error) error {
	if rateLimited(readErr) {
		return d.recordCheckReadRetriesEnded(ctx, taskID, 0)
	}
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	due, _, err := d.checkReadRetryDue(ctx, task)
	if err != nil || !due {
		return err
	}
	if !d.durableQueue {
		workspace, _ := store.WorkspaceFromContext(ctx)
		select {
		case d.memoryQueue <- queuedTask{Workspace: workspace, TaskID: taskID}:
		default:
			log.Printf("[task %s] memory queue is full; the check-status read waits for another wake", taskID)
		}
		return nil
	}
	_, err = d.Store.EnqueueCheckStatusRetry(ctx, taskID)
	return err
}

// readCheckStatusAttempt is one attempt of the job on an approved gate-off task
// (component-durable-queue invariant 37). It returns an error only for the queue
// to schedule the next attempt, and it records the end of the retries instead on
// the final attempt or a rate-limited read. A task with nothing due completes
// with no read, which it decides under the task lock, so an attempt that waited
// on a firing deadline finds the deadline gone and adds no read to it.
func (d *Dispatcher) readCheckStatusAttempt(ctx context.Context, task core.Task, attempt int, final bool) error {
	mergeErr := d.readAndMergeIfRetryDue(ctx, task.ID)
	if mergeErr == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if !retryableMergeError(mergeErr) {
		log.Printf("[task %s] check-status attempt %d ended: %v", task.ID, attempt, mergeErr)
		return nil
	}
	if rateLimited(mergeErr) || final {
		log.Printf("[task %s] check-status retries ended at attempt %d: %v", task.ID, attempt, mergeErr)
		return d.recordCheckReadRetriesEnded(ctx, task.ID, attempt)
	}
	// An answer the merge then failed on is not an unanswered read.
	current, err := d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	due, _, err := d.checkReadRetryDue(ctx, current)
	if err != nil {
		return err
	}
	if !due {
		log.Printf("[task %s] check-status attempt %d answered: %v", task.ID, attempt, mergeErr)
		return nil
	}
	return mergeErr
}

// readAndMergeIfRetryDue is one attempt's read and merge, if its read is still
// due once it holds the task lock.
func (d *Dispatcher) readAndMergeIfRetryDue(ctx context.Context, taskID string) error {
	return d.Store.WithTaskSideEffectLock(ctx, taskID, func(lockedCtx context.Context) error {
		current, err := d.Store.GetTask(lockedCtx, taskID)
		if err != nil {
			return err
		}
		due, _, err := d.checkReadRetryDue(lockedCtx, current)
		if err != nil || !due {
			return err
		}
		return d.mergeApprovedTaskLocked(lockedCtx, current, mergeAct{})
	})
}

// recordCheckReadRetriesEnded appends merge.check_pending saying the retries
// ended, so the task reads truthfully and an active job finds nothing due. It
// moves no deadline. A later failed read opens the retries again.
func (d *Dispatcher) recordCheckReadRetriesEnded(ctx context.Context, taskID string, attempts int) error {
	return d.Store.WithTaskSideEffectLock(ctx, taskID, func(lockedCtx context.Context) error {
		task, err := d.Store.GetTask(lockedCtx, taskID)
		if err != nil {
			return err
		}
		due, status, err := d.checkReadRetryDue(lockedCtx, task)
		if err != nil || !due {
			return err
		}
		payload := checkEventPayload(task, store.CheckStatusHead(task), "read_failed", nil)
		payload["checks"] = waitingChecks(status)
		payload["read_failed"], payload["read_failure"], payload["retries_exhausted"] = true, status.ReadFailure, true
		payload["attempts"], payload["deadline"] = attempts, task.CheckStatusDeadline.UTC()
		return d.Store.AppendEvent(lockedCtx, core.Event{TaskID: taskID, Kind: store.EventCheckPending, Payload: core.JSONPayload(payload)})
	})
}
