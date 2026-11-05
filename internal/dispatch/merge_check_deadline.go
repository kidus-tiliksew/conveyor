package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// The check-status deadline pass of the order clock (req-review-gates-evidence
// AC-1.9; req-quiet-idle-runtime AC-2.4, AC-3.9; component-submission-merge
// invariant 61; component-durable-queue invariant 33; DEC-108(5), (7)).
//
// A due deadline costs one check-status read. The read comes before any
// pull-request read, because a merge is the only other GitHub call a deadline
// may cause, and only a read that lets the task merge goes on to one.
//
// The deadline clears after what the read decided is recorded, all under the
// task lock, which is also what any other read of the task waits on. A process
// that dies in between leaves the deadline due, so the next fire reads again and
// nothing stops silently; recordCheckHold appends nothing when the hold already
// says the same.

// ApplyDueCheckStatusDeadlines fires every due check-status deadline of the
// context's workspace. A failure on one task doesn't excuse another's deadline,
// and a returned error makes the alarm fire again (component-durable-queue
// invariant 32).
func (d *Dispatcher) ApplyDueCheckStatusDeadlines(ctx context.Context) error {
	approved, err := d.Store.ListTasksFiltered(ctx, store.TaskFilter{States: []core.TaskState{core.TaskApproved}})
	if err != nil {
		return err
	}
	now := d.checkClock()
	// The volatile store's list spans its workspaces.
	workspace, _ := store.WorkspaceFromContext(ctx)
	var errs []error
	for _, task := range approved {
		if task.Workspace != workspace || !checkStatusDeadlineDue(task, now) {
			continue
		}
		if err := d.fireCheckStatusDeadline(ctx, task.ID, now); err != nil {
			log.Printf("[task %s] check-status deadline: %v", task.ID, err)
			errs = append(errs, fmt.Errorf("task %s: %w", task.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (d *Dispatcher) checkClock() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// checkStatusDeadlineDue selects the rows component-durable-queue invariant 34
// names as a deadline source, once the deadline has passed.
func checkStatusDeadlineDue(task core.Task, now time.Time) bool {
	return checkReadRowApplies(task) && task.CheckStatusDeadline != nil && !task.CheckStatusDeadline.After(now)
}

// fireCheckStatusDeadline applies one due deadline under the task lock. It
// decides from the row it reads there, so a second fire or a read that got the
// lock first leaves it nothing to do and costs no GitHub call.
func (d *Dispatcher) fireCheckStatusDeadline(ctx context.Context, taskID string, now time.Time) error {
	return d.Store.WithTaskSideEffectLock(ctx, taskID, func(lockedCtx context.Context) error {
		task, err := d.Store.GetTask(lockedCtx, taskID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !checkStatusDeadlineDue(task, now) {
			return nil
		}
		// The deadline belongs to a head no longer approved: a new head starts
		// fresh, so it holds nothing and reads nothing.
		head := store.CheckStatusHead(task)
		if task.ApprovalStale || head == "" || task.CheckStatusDeadlineHead != head {
			return d.clearCheckDeadline(lockedCtx, task)
		}
		cfg, err := d.currentConfig(lockedCtx)
		if err != nil {
			return fmt.Errorf("load workspace repository configuration: %w", err)
		}
		var read github.CheckStatus
		readErr := fmt.Errorf("repository %q does not configure GitHub", task.Repo)
		if repo, ok := cfg.Repo(task.Repo); ok && strings.TrimSpace(repo.GitHub) != "" {
			read, readErr = d.ReadCommitChecks(lockedCtx, repo.GitHub, head)
		}
		if readErr != nil && lockedCtx.Err() != nil {
			return lockedCtx.Err()
		}
		standing, err := d.foldCheckStatus(lockedCtx, task.ID, head)
		if err != nil {
			return err
		}
		if readErr == nil && len(read.Named(github.CheckFailed)) == 0 && len(read.Named(github.CheckPending)) == 0 {
			return d.mergeAtCheckStatusDeadline(lockedCtx, task, head, standing, read)
		}
		if err := d.recordDeadlineHold(lockedCtx, task, head, standing, read, readErr); err != nil {
			return err
		}
		return d.clearCheckDeadline(lockedCtx, task)
	})
}

// recordDeadlineHold appends what the deadline's read decides when no merge may
// follow. Invariant 60 decides a refused read and a failed check. A pending check
// with none failed, or any other failed read, is stuck. The read records no new
// deadline, so the alarm does not fire again for it.
func (d *Dispatcher) recordDeadlineHold(ctx context.Context, task core.Task, head string, standing nextaction.CheckStatus, read github.CheckStatus, readErr error) error {
	if readErr != nil {
		// A read with no answer names what the last read found: the standing
		// hold's checks when one stands, or else what the wait names.
		last := standing.Waiting
		if standing.Hold != nil && len(standing.Hold.Checks) > 0 {
			last = standing.Hold.Checks
		}
		reason := nextaction.CheckHoldStuck
		if github.ErrorCategory(readErr) == github.ForgePermission {
			reason = nextaction.CheckHoldPermission
		}
		return d.recordCheckHold(ctx, task, head, standing.Hold, reason, checksFromRefs(last), checkReadFailure(readErr))
	}
	failed, pending := read.Named(github.CheckFailed), read.Named(github.CheckPending)
	if len(failed) > 0 {
		return d.recordCheckHold(ctx, task, head, standing.Hold, nextaction.CheckHoldFailed, append(failed, pending...), "")
	}
	return d.recordCheckHold(ctx, task, head, standing.Hold, nextaction.CheckHoldStuck, pending, "")
}

// mergeAtCheckStatusDeadline merges through the automatic variant with the
// status the deadline's read found, so that read is the merge's only one
// (req-review-gates-evidence AC-1.4; req-quiet-idle-runtime AC-3.9). This is the
// one case where a deadline makes more GitHub calls than its read, because a lost
// delivery has no other wake on an active workspace.
//
// A failure a retry can fix returns with the deadline still due, so the alarm's
// failed-fire retry tries again. Nothing else would wake the task. Anything else
// the merge path has recorded, and the deadline has done its work.
func (d *Dispatcher) mergeAtCheckStatusDeadline(ctx context.Context, task core.Task, head string, standing nextaction.CheckStatus, read github.CheckStatus) error {
	status := "none_reported"
	if len(read.Checks) > 0 {
		status = "passed"
	}
	if err := d.clearCheckHold(ctx, task, head, standing.Hold, status); err != nil {
		return err
	}
	mergeErr := d.mergeApprovedTaskLocked(ctx, task, mergeAct{checkStatus: status})
	if ctx.Err() != nil {
		return errors.Join(mergeErr, ctx.Err())
	}
	if retryableMergeError(mergeErr) {
		return fmt.Errorf("merge after the deadline's read: %w", mergeErr)
	}
	if mergeErr != nil {
		log.Printf("[task %s] merge after the check-status deadline: %v", task.ID, mergeErr)
	}
	return d.clearCheckDeadline(ctx, task)
}

func checksFromRefs(refs []nextaction.CheckRef) []github.Check {
	checks := make([]github.Check, 0, len(refs))
	for _, ref := range refs {
		checks = append(checks, github.Check{Name: ref.Name, State: ref.State, URL: ref.URL})
	}
	return checks
}

// announceCheckDeadline tells a sleeping alarm about a deadline a read found
// already recorded and appended nothing for. A task that left approved and came
// back keeps its deadline at the same head, and the fire in between skipped it,
// so without this nothing would apply it until an unrelated fire.
func (d *Dispatcher) announceCheckDeadline(task core.Task) {
	if task.CheckStatusDeadline == nil {
		return
	}
	if notifier, ok := d.Store.(interface{ Changes() *changes.Hub }); ok {
		notifier.Changes().Publish(changes.Notice{Workspace: task.Workspace, Kind: changes.Deadline, At: task.CheckStatusDeadline.UTC()})
	}
}
