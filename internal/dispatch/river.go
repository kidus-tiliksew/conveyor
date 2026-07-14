package dispatch

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type dispatchTaskWorker struct {
	river.WorkerDefaults[queueargs.DispatchTaskArgs]
	dispatcher *Dispatcher
}

func (w *dispatchTaskWorker) Work(ctx context.Context, job *river.Job[queueargs.DispatchTaskArgs]) error {
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:%d", job.ID), Role: core.ActorSystem})
	err := w.dispatcher.runTask(ctx, job.Args.TaskID)
	if err == nil {
		task, getErr := w.dispatcher.Store.GetTask(ctx, job.Args.TaskID)
		if getErr != nil {
			return getErr
		}
		if task.State == core.TaskQueued {
			// The currently running River row owns this task's pipeline. A
			// queued stage transition cannot insert a duplicate while this row
			// is active, so snooze the same row into the next stage.
			return river.JobSnooze(time.Second)
		}
		return nil
	}
	return w.handleFailure(ctx, job, err)
}

func (w *dispatchTaskWorker) handleFailure(ctx context.Context, job *river.Job[queueargs.DispatchTaskArgs], err error) error {
	payload := core.JSONPayload(map[string]any{
		"attempt":      job.Attempt,
		"max_attempts": job.MaxAttempts,
		"error":        err.Error(),
	})
	if eventErr := w.dispatcher.Store.AppendEvent(ctx, core.Event{
		TaskID: job.Args.TaskID, Kind: "dispatch.failed", Payload: payload,
	}); eventErr != nil {
		log.Printf("[task %s] record River failure: %v", job.Args.TaskID, eventErr)
	}
	task, taskErr := w.dispatcher.Store.GetTask(ctx, job.Args.TaskID)
	if taskErr != nil {
		return fmt.Errorf("dispatch failed: %v; load recovery transition: %w", err, taskErr)
	}
	recoveryStage := task.NextStage
	if recoveryStage == "" {
		if latest, ok, latestErr := w.dispatcher.Store.GetLatestJob(ctx, job.Args.TaskID); latestErr == nil && ok {
			recoveryStage = latest.Stage
		}
	}
	if job.Attempt >= job.MaxAttempts {
		if stateErr := w.dispatcher.Store.SetTaskTransition(ctx, job.Args.TaskID, core.TaskParked, "", recoveryStage); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; park after final River attempt: %w", err, stateErr)
		}
	} else if stateErr := w.dispatcher.Store.SetTaskTransition(ctx, job.Args.TaskID, core.TaskQueued, recoveryStage, ""); stateErr != nil {
		return fmt.Errorf("dispatch failed: %v; persist retry transition: %w", err, stateErr)
	}
	return err
}

// NewRiverClient binds the durable queue to the dispatcher. The Store owns
// transactional insertion; this client owns only worker execution (spec
// §3.1, §17.0).
func NewRiverClient(pool *pgxpool.Pool, dispatcher *Dispatcher) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &dispatchTaskWorker{dispatcher: dispatcher})
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		// Dispatcher stage contexts enforce the configured per-stage wall-clock
		// limits. River's one-minute default would cancel long harness runs and
		// handoff artifact collection before those limits (spec §14).
		JobTimeout: -1,
		Queues: map[string]river.QueueConfig{
			queueargs.DispatchQueue(dispatcher.Cfg.Workspace): {MaxWorkers: 1},
		},
		Workers: workers,
	})
}
