package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("queue:%s", job.ID), Role: core.ActorSystem})
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
			// The running job owns this task's pipeline. A queued stage
			// transition cannot enqueue a duplicate while this job is active,
			// so snooze the same job into the next stage.
			return queue.Snooze(time.Second)
		}
		return nil
	}
	if w.shutdown.Stopping() && errors.Is(err, context.Canceled) {
		// Shutdown interruption is not a dispatch failure. Snoozing hands the
		// attempt back and keeps the same durable job available for another
		// instance (REQ-6/AC-6.2; design-task-lifecycle).
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
	// already materialized the §21.30 conflict-fix order. Treat that durable
	// active order as success before emitting failure or requeue activity.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if _, active, lookupErr := w.dispatcher.activeImplementationWorkOrder(ctx, args.TaskID, "merge-conflict"); lookupErr != nil {
			return fmt.Errorf("dispatch duplicate recovery: %w", lookupErr)
		} else if active {
			return nil
		}
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
	recoveryStage := task.NextStage
	if recoveryStage == "" {
		if latest, ok, latestErr := w.dispatcher.Store.GetLatestJob(ctx, args.TaskID); latestErr == nil && ok {
			recoveryStage = latest.Stage
		}
	}
	if job.Attempt >= job.MaxAttempts {
		if task.State == core.TaskRunning {
			// Return the failed stage to queued before applying T13. This preserves
			// the canonical queued -> parked edge and records dispatch.fail_final
			// for operator visibility (design-task-lifecycle).
			if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, args.TaskID, taskops.Command{Kind: core.TaskStageBounce, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
				return fmt.Errorf("dispatch failed: %v; requeue before final dispatch failure: %w", err, stateErr)
			}
		}
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, args.TaskID, taskops.Command{Kind: core.TaskDispatchFailFinal, RecoveryStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; park after final dispatch attempt: %w", err, stateErr)
		}
	} else {
		command := core.TaskDispatchFailRetry
		if task.State == core.TaskRunning {
			// There is no running-state dispatch-failure retry edge. Preserve the
			// existing requeue behavior as an explicit table-gap workaround until
			// the lifecycle table is amended (design-task-lifecycle).
			command = core.TaskStageBounce
		}
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, args.TaskID, taskops.Command{Kind: command, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; persist retry transition: %w", err, stateErr)
		}
	}
	return err
}
