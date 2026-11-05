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
	err = w.dispatcher.runTask(ctx, args.TaskID)
	if err == nil {
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
	if stateErr := w.applyFailure(ctx, args.TaskID, job, err); stateErr != nil {
		return stateErr
	}
	return err
}

// Discarded is the dispatch_task discard handler. Rescue discarded the job
// after its last attempt without a handler outcome, so it applies the
// final-attempt row of the dispatch outcome table: dispatch.failed, a running
// task takes stage.bounce, and dispatch.fail_final parks the task
// (component-durable-queue, Handlers; req-task-lifecycle-and-queue AC-6.2).
// A task that a work order holds, a blueprint anchor, and a task that is
// neither queued nor running are not stuck, so they are left alone, as the
// start-up reconciliation leaves them (component-durable-queue,
// Reconciliation); a repeat finds the task parked and changes nothing.
func (w *dispatchTaskWorker) Discarded(ctx context.Context, job queue.Job, reason string) error {
	args, err := queue.DecodeArgs[queue.DispatchTaskArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.SystemActor(fmt.Sprintf("queue:%s", job.ID)))
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	task, err := w.dispatcher.Store.GetTask(ctx, args.TaskID)
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
	job.Attempt = max(job.Attempt, job.MaxAttempts)
	return w.applyFailure(ctx, args.TaskID, job, errors.New(reason))
}

// applyFailure records dispatch.failed and applies the retry or final
// transition for one failed attempt. It returns only a persistence failure.
func (w *dispatchTaskWorker) applyFailure(ctx context.Context, taskID string, job queue.Job, err error) error {
	payload := core.JSONPayload(map[string]any{
		"attempt":      job.Attempt,
		"max_attempts": job.MaxAttempts,
		"error":        err.Error(),
	})
	if eventErr := w.dispatcher.Store.AppendEvent(ctx, core.Event{
		TaskID: taskID, Kind: "dispatch.failed", Payload: payload,
	}); eventErr != nil {
		log.Printf("[task %s] record dispatch failure: %v", taskID, eventErr)
	}
	task, taskErr := w.dispatcher.Store.GetTask(ctx, taskID)
	if taskErr != nil {
		return fmt.Errorf("dispatch failed: %v; load recovery transition: %w", err, taskErr)
	}
	recoveryStage := task.NextStage
	if recoveryStage == "" {
		if latest, ok, latestErr := w.dispatcher.Store.GetLatestJob(ctx, taskID); latestErr == nil && ok {
			recoveryStage = latest.Stage
		}
	}
	if job.Attempt >= job.MaxAttempts {
		if task.State == core.TaskRunning {
			// Return the failed stage to queued before applying T13. This preserves
			// the canonical queued -> parked edge and records dispatch.fail_final
			// for operator visibility (component-durable-queue).
			if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageBounce, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
				return fmt.Errorf("dispatch failed: %v; requeue before final dispatch failure: %w", err, stateErr)
			}
		}
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, taskID, taskops.Command{Kind: core.TaskDispatchFailFinal, RecoveryStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; park after final dispatch attempt: %w", err, stateErr)
		}
	} else {
		command := core.TaskDispatchFailRetry
		if task.State == core.TaskRunning {
			// There is no running-state dispatch-failure retry edge. Preserve the
			// existing requeue behavior as an explicit table-gap workaround until
			// the lifecycle table is amended (component-durable-queue).
			command = core.TaskStageBounce
		}
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, taskID, taskops.Command{Kind: command, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; persist retry transition: %w", err, stateErr)
		}
	}
	return nil
}
