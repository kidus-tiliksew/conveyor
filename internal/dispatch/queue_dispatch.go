package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// The task dispatch handler and its failure policy.

func (w *dispatchTaskWorker) Work(ctx context.Context, job queue.Job) error {
	args, err := queue.DecodeArgs[queue.DispatchTaskArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.SystemActor(fmt.Sprintf("queue:%s", job.ID)))
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	// Invariant 37's check-status rows return from here, so their errors never
	// reach handleFailure. A task the read moved out of approved runs as usual
	// with this job, because the transition can't enqueue while it is active.
	if settled, outcome := w.checkReadOutcome(ctx, job, args.TaskID); settled {
		return outcome
	}
	err = w.dispatcher.runTask(ctx, args.TaskID)
	if err == nil {
		// An in-process review may have just approved the task and found the
		// read unanswered, while this job held the stream.
		if settled, outcome := w.checkReadOutcome(ctx, job, args.TaskID); settled {
			return outcome
		}
		task, getErr := w.dispatcher.Store.GetTask(ctx, args.TaskID)
		if getErr != nil {
			return getErr
		}
		if task.State == core.TaskQueued {
			if isBlueprintAnchor(task) {
				// Blueprint parents are passive batch anchors. Their children
				// own implementation delivery, so this job is complete rather
				// than a staged pipeline job to snooze.
				return nil
			}
			if _, active, activeErr := w.dispatcher.activeWorkOrder(ctx, task.ID, task.NextStage, ""); activeErr != nil {
				return activeErr
			} else if active {
				// The stage has its work order and the task is waiting for a
				// claimant. The job is done; the claim enqueues nothing and the
				// transition after it enqueues a fresh job. Snoozing here would
				// append a claim and a snooze to the log every second for as
				// long as the task waits.
				return nil
			}
			// The run advanced the task to a queued stage that has no order
			// yet. A queued stage transition cannot enqueue a duplicate while
			// this job is active, so snooze the same job into the next stage.
			return queue.Snooze(time.Second)
		}
		return nil
	}
	if w.shutdown.Stopping() && errors.Is(err, context.Canceled) {
		// Shutdown interruption is not a dispatch failure. Snoozing hands the
		// attempt back and keeps the same durable job available for another
		// instance (REQ-6/AC-6.2; component-durable-queue).
		log.Printf("[task %s] queue job %s interrupted by daemon shutdown; preserving attempt %d", args.TaskID, job.ID, job.Attempt)
		return queue.Snooze(shutdownRetryDelay)
	}
	return w.handleFailure(ctx, job, err)
}

// checkReadOutcome applies invariant 37's rows for an approved task with the
// merge gate off, and reports whether they settled this attempt. It returns the
// error for job.failed to schedule the next attempt, with no dispatch.failed and
// no transition. Daemon shutdown hands the attempt back. A final attempt returns
// nothing, so the queue never discards this job.
func (w *dispatchTaskWorker) checkReadOutcome(ctx context.Context, job queue.Job, taskID string) (bool, error) {
	task, err := w.dispatcher.Store.GetTask(ctx, taskID)
	if err != nil || !checkReadRowApplies(task) {
		return false, nil
	}
	final := job.Attempt >= job.MaxAttempts
	err = w.dispatcher.readCheckStatusAttempt(ctx, task, job.Attempt, final)
	switch {
	case err == nil:
	case w.shutdown.Stopping() && errors.Is(err, context.Canceled):
		log.Printf("[task %s] queue job %s interrupted by daemon shutdown; preserving attempt %d", taskID, job.ID, job.Attempt)
		return true, queue.Snooze(shutdownRetryDelay)
	case final:
		log.Printf("[task %s] check-status attempt %d: %v", taskID, job.Attempt, err)
	default:
		return true, err
	}
	current, err := w.dispatcher.Store.GetTask(ctx, taskID)
	if err != nil {
		log.Printf("[task %s] check-status attempt %d: %v", taskID, job.Attempt, err)
		return true, nil
	}
	return checkReadRowApplies(current), nil
}

func (w *dispatchTaskWorker) handleFailure(ctx context.Context, job queue.Job, err error) error {
	args, decodeErr := queue.DecodeArgs[queue.DispatchTaskArgs](job)
	if decodeErr != nil {
		return fmt.Errorf("dispatch failed: %v; decode job: %w", err, decodeErr)
	}
	// A duplicate jobs key can be a lost acknowledgement from a dispatch that
	// already materialized the conflict-fix order. Treat that durable active
	// order as success before emitting failure or requeue activity
	// (component-submission-merge).
	if errors.Is(err, store.ErrDispatchJobConflict) {
		if _, active, lookupErr := w.dispatcher.activeImplementationWorkOrder(ctx, args.TaskID, "merge-conflict"); lookupErr != nil {
			return fmt.Errorf("dispatch duplicate recovery: %w", lookupErr)
		} else if active {
			return nil
		}
	}
	if job.Attempt >= job.MaxAttempts {
		if stateErr := w.parkAfterFinalAttempt(ctx, args.TaskID, job, err); stateErr != nil {
			return stateErr
		}
		return err
	}
	payload := core.JSONPayload(map[string]any{
		"attempt":      job.Attempt,
		"max_attempts": job.MaxAttempts,
		"error":        err.Error(),
	})
	if eventErr := w.dispatcher.Store.AppendEvent(ctx, core.Event{
		TaskID: args.TaskID, Kind: "dispatch.failed", Payload: payload,
	}); eventErr != nil {
		log.Printf("[task %s] record dispatch failure: %v", args.TaskID, eventErr)
	}
	task, taskErr := w.dispatcher.Store.GetTask(ctx, args.TaskID)
	if taskErr != nil {
		return fmt.Errorf("dispatch failed: %v; load recovery transition: %w", err, taskErr)
	}
	command := core.TaskDispatchFailRetry
	if task.State == core.TaskRunning {
		// There is no running-state dispatch-failure retry edge. Preserve the
		// existing requeue behavior as an explicit table-gap workaround until
		// the lifecycle table is amended (component-durable-queue).
		command = core.TaskStageBounce
	}
	if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, args.TaskID, taskops.Command{Kind: command, NextStage: w.recoveryStage(ctx, task), ProjectStages: true}); stateErr != nil {
		return fmt.Errorf("dispatch failed: %v; persist retry transition: %w", err, stateErr)
	}
	return err
}

// parkAfterFinalAttempt applies the final-attempt row of the dispatch outcome
// table. A running task takes stage.bounce, then dispatch.fail_final parks the
// task and records dispatch.failed in the same transaction, so a retry after
// any failed step applies each transition and that evidence once
// (component-durable-queue, Handlers; req-task-lifecycle-and-queue AC-6.2).
func (w *dispatchTaskWorker) parkAfterFinalAttempt(ctx context.Context, taskID string, job queue.Job, err error) error {
	task, taskErr := w.dispatcher.Store.GetTask(ctx, taskID)
	if taskErr != nil {
		return fmt.Errorf("dispatch failed: %v; load recovery transition: %w", err, taskErr)
	}
	recoveryStage := w.recoveryStage(ctx, task)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("dispatch failed: %v; before final dispatch transitions: %w", err, ctxErr)
	}
	if task.State == core.TaskRunning {
		// Return the failed stage to queued before applying T13. This preserves
		// the canonical queued -> parked edge and records dispatch.fail_final
		// for operator visibility (component-durable-queue).
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageBounce, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; requeue before final dispatch failure: %w", err, stateErr)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("dispatch failed: %v; before final dispatch failure: %w", err, ctxErr)
	}
	if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, taskID, taskops.Command{
		Kind: core.TaskDispatchFailFinal, RecoveryStage: recoveryStage, ProjectStages: true,
		FailureMessage: err.Error(), Attempt: job.Attempt, MaxAttempts: job.MaxAttempts,
	}); stateErr != nil {
		return fmt.Errorf("dispatch failed: %v; park after final dispatch attempt: %w", err, stateErr)
	}
	return nil
}

func (w *dispatchTaskWorker) recoveryStage(ctx context.Context, task core.Task) core.Stage {
	if task.NextStage != "" {
		return task.NextStage
	}
	if latest, ok, latestErr := w.dispatcher.Store.GetLatestJob(ctx, task.ID); latestErr == nil && ok {
		return latest.Stage
	}
	return ""
}

// Discarded is the dispatch_task discard handler, which the queue retries
// until it returns nil. It parks a queued or running task. A task that a work
// order holds, a blueprint anchor, and a task in any other state are not
// stuck, so a retry after a final transition changes nothing. A job whose
// arguments or task no longer resolve has nothing to finalize
// (req-task-lifecycle-and-queue AC-6.2).
func (w *dispatchTaskWorker) Discarded(ctx context.Context, job queue.Job, reason string) error {
	args, err := queue.DecodeArgs[queue.DispatchTaskArgs](job)
	if err != nil {
		log.Printf("queue job %s: discard: decode job: %v", job.ID, err)
		return nil
	}
	ctx = store.WithActor(ctx, store.SystemActor(fmt.Sprintf("queue:%s", job.ID)))
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	task, err := w.dispatcher.Store.GetTask(ctx, args.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if task.State != core.TaskQueued && task.State != core.TaskRunning || isBlueprintAnchor(task) {
		return nil
	}
	if _, active, activeErr := w.dispatcher.activeWorkOrder(ctx, task.ID, task.NextStage, ""); activeErr != nil {
		return activeErr
	} else if active {
		return nil
	}
	return w.parkAfterFinalAttempt(ctx, args.TaskID, job, errors.New(reason))
}
