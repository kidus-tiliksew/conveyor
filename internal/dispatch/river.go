package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
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
		return nil
	}
	return w.handleFailure(ctx, job, err)
}

func (w *dispatchTaskWorker) handleFailure(ctx context.Context, job *river.Job[queueargs.DispatchTaskArgs], err error) error {
	if errors.Is(err, routing.ErrNoCapacity) {
		payload := core.JSONPayload(map[string]any{
			"retry_in_seconds": int64(routing.RateLimitCooldown.Seconds()),
			"reason":           err.Error(),
		})
		if eventErr := w.dispatcher.Store.AppendEvent(ctx, core.Event{
			TaskID: job.Args.TaskID, Kind: "dispatch.capacity_wait", Payload: payload,
		}); eventErr != nil {
			return fmt.Errorf("record capacity wait: %w", eventErr)
		}
		return river.JobSnooze(routing.RateLimitCooldown)
	}
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
	if job.Attempt >= job.MaxAttempts {
		if stateErr := w.dispatcher.Store.UpdateTaskState(ctx, job.Args.TaskID, core.TaskParked); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; park after final River attempt: %w", err, stateErr)
		}
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
		Queues: map[string]river.QueueConfig{
			queueargs.DispatchQueue(dispatcher.Cfg.Workspace): {MaxWorkers: 1},
		},
		Workers: workers,
	})
}
