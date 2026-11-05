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
	"github.com/kidus-tiliksew/conveyor/internal/queue"
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
	// ErrCheckStatusUnread reports a read with no answer, which is no pass. It
	// wraps the forge error, so a retry and a rate-limit pause can see it.
	ErrCheckStatusUnread = errors.New("check status could not be read")
)

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
	stuck := standing.Hold != nil && standing.Hold.Reason == nextaction.CheckHoldStuck
	switch {
	case len(pending) > 0 && stuck:
		// A stuck hold stays until a read finds no check pending, and records no
		// new deadline while it stands (AC-1.9; invariant 61). It keeps naming
		// what this read found.
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
// deadline AC-1.5 asks of a failed read, unless a stuck hold stands. Only a
// refused permission names the operator at once; a failed read clears nothing.
func (d *Dispatcher) applyFailedCheckRead(ctx context.Context, task core.Task, head string, standing nextaction.CheckStatus, readErr error) error {
	hold := standing.Hold
	permission := github.ErrorCategory(readErr) == github.ForgePermission
	if hold == nil || hold.Reason != nextaction.CheckHoldStuck {
		reason := "read_failed"
		if permission {
			reason = nextaction.CheckHoldPermission
		}
		payload := checkEventPayload(task, head, reason, nil)
		payload["read_failed"], payload["read_failure"] = true, checkReadFailure(readErr)
		if category := github.ErrorCategory(readErr); category != "" {
			payload["forge_error_category"] = category
		}
		if err := d.recordCheckDeadline(ctx, task, head, standing, payload); err != nil {
			return err
		}
	}
	if !permission {
		return fmt.Errorf("%w for head %s: %w", ErrCheckStatusUnread, head, readErr)
	}
	// The hold keeps naming what a standing hold named, so an override still
	// names each check the last read that found any named.
	var named []github.Check
	if hold != nil {
		for _, check := range hold.Checks {
			named = append(named, github.Check{Name: check.Name, State: check.State, URL: check.URL})
		}
	}
	if err := d.recordCheckHold(ctx, task, head, hold, nextaction.CheckHoldPermission, named, checkReadFailure(readErr)); err != nil {
		return err
	}
	return fmt.Errorf("%w: GitHub refused the read of head %s: %w", ErrCheckStatusHeld, head, readErr)
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
	return d.recordCheckDeadline(ctx, task, head, standing, checkEventPayload(task, head, "pending", pending))
}

// recordCheckDeadline records the deadline and merge.check_pending together. A
// later read of the same head leaves the deadline where it is. It appends a
// further event only when the read changes between answered and unanswered, so
// the wait reads truthfully and a read that answered ends a retry.
func (d *Dispatcher) recordCheckDeadline(ctx context.Context, task core.Task, head string, standing nextaction.CheckStatus, payload map[string]any) error {
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now().UTC()
	}
	deadline := now.Add(CheckStatusDeadlineWindow)
	payload["deadline"] = deadline
	event := core.Event{TaskID: task.ID, Kind: store.EventCheckPending, Payload: core.JSONPayload(payload)}
	recorded, err := d.Store.RecordCheckStatusDeadline(ctx, task.ID, head, deadline, event)
	if err != nil || recorded {
		return err
	}
	readFailed, _ := payload["read_failed"].(bool)
	if readFailed == (standing.ReadFailure != "") {
		return nil
	}
	if task.CheckStatusDeadline != nil {
		payload["deadline"] = task.CheckStatusDeadline.UTC()
	}
	event.Payload = core.JSONPayload(payload)
	return d.Store.AppendEvent(ctx, event)
}

func (d *Dispatcher) clearCheckDeadline(ctx context.Context, task core.Task) error {
	_, err := d.Store.ClearCheckStatusDeadline(ctx, task.ID)
	return err
}

// recordCheckHold appends merge.check_hold unless the standing hold already
// says the same, so a repeated delivery appends nothing.
func (d *Dispatcher) recordCheckHold(ctx context.Context, task core.Task, head string, standing *nextaction.CheckHold, reason string, checks []github.Check, failure string) error {
	if standing != nil && standing.Reason == reason && sameChecks(standing.Checks, checks) {
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

// The bounded retry of an unanswered check-status read. A failed read at the
// verdict, or at a delivery whose own retries ran out, may have no later event:
// every check can have finished already. The retry is the task's dispatch_task
// job, a new enqueue of an existing kind (component-durable-queue, Extending
// this component). The job survives a restart, and the handler reads again at
// that kind's delays. It completes whatever it finds, as invariant 37 states for
// a task that is not queued, so an exhausted retry neither parks the task nor
// fails the job. The wakes of req-review-gates-evidence AC-1.5 still apply.

// CheckReadRetries bounds the reads one retry job makes for one failure.
const CheckReadRetries = 3

// checkReadRetryDue reads the durable state a retry acts on, so a job that
// fires late does nothing: the head moved, the task merged or closed, a later
// read answered, the retries ended, or GitHub refused the read, which only the
// operator's grant changes.
func (d *Dispatcher) checkReadRetryDue(ctx context.Context, task core.Task) (bool, nextaction.CheckStatus, error) {
	head := store.CheckStatusHead(task)
	if task.State != core.TaskApproved || task.MergeApproval || task.ApprovalStale || head == "" ||
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

// arrangeCheckReadRetry enqueues the retry job when one is due. An active job
// holds the stream, so repeated failures enqueue one job.
func (d *Dispatcher) arrangeCheckReadRetry(ctx context.Context, taskID string) error {
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
			log.Printf("[task %s] memory queue is full; the check-status retry waits for another wake", taskID)
		}
		return nil
	}
	_, err = d.Store.EnqueueCheckStatusRetry(ctx, taskID)
	return err
}

// retryCheckStatusRead is the job's bounded retry. It holds no lock while it
// waits, and it reads the task's state again after each wait. It returns only a
// failure of its own; a read that stays unanswered ends with a recorded
// merge.check_pending saying so.
func (d *Dispatcher) retryCheckStatusRead(ctx context.Context, taskID string) error {
	for attempt := 1; attempt <= CheckReadRetries; attempt++ {
		if due, _, err := d.loadCheckReadRetry(ctx, taskID); err != nil || !due {
			return err
		}
		if err := d.waitCheckReadRetry(ctx, queue.DispatchTaskRetryDelay(attempt)); err != nil {
			return err
		}
		due, _, err := d.loadCheckReadRetry(ctx, taskID)
		if err != nil || !due {
			return err
		}
		task, err := d.Store.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		mergeErr := d.readAndMergeAutomatically(ctx, task)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if mergeErr == nil || !retryableMergeError(mergeErr) {
			if mergeErr != nil {
				log.Printf("[task %s] check-status retry ended: %v", taskID, mergeErr)
			}
			return nil
		}
		log.Printf("[task %s] check-status retry %d of %d: %v", taskID, attempt, CheckReadRetries, mergeErr)
		// A rate limit outlasts this retry's delays, and every read spends more
		// of it. The timed fallback honours the reset GitHub names.
		if rateLimited(mergeErr) {
			break
		}
	}
	return d.recordCheckReadRetriesExhausted(ctx, taskID)
}

func (d *Dispatcher) loadCheckReadRetry(ctx context.Context, taskID string) (bool, nextaction.CheckStatus, error) {
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return false, nextaction.CheckStatus{}, err
	}
	return d.checkReadRetryDue(ctx, task)
}

func (d *Dispatcher) waitCheckReadRetry(ctx context.Context, delay time.Duration) error {
	if d.CheckReadRetryWait != nil {
		return d.CheckReadRetryWait(ctx, delay)
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

// recordCheckReadRetriesExhausted records that the retries ended, so the task
// reads truthfully and a later failed read of this episode enqueues no more
// jobs, which keeps a timed sweep at its one read a minute. A read that answers
// starts over. It moves no deadline.
func (d *Dispatcher) recordCheckReadRetriesExhausted(ctx context.Context, taskID string) error {
	return d.Store.WithTaskSideEffectLock(ctx, taskID, func(lockedCtx context.Context) error {
		task, err := d.Store.GetTask(lockedCtx, taskID)
		if err != nil {
			return err
		}
		due, status, err := d.checkReadRetryDue(lockedCtx, task)
		if err != nil || !due {
			return err
		}
		head := store.CheckStatusHead(task)
		payload := checkEventPayload(task, head, "read_failed", nil)
		payload["read_failed"], payload["read_failure"], payload["retries_exhausted"] = true, status.ReadFailure, true
		payload["retries"], payload["deadline"] = CheckReadRetries, task.CheckStatusDeadline.UTC()
		return d.Store.AppendEvent(lockedCtx, core.Event{TaskID: taskID, Kind: store.EventCheckPending, Payload: core.JSONPayload(payload)})
	})
}
