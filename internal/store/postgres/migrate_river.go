package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// moveRiverJobsToLog runs once, inside the migration transaction, just
// before migration 121 drops River's tables. Every River job that could
// still run is enqueued on its log stream, so a deployment that upgrades
// with work queued or in flight loses nothing: queued work stays queued,
// and an interrupted run is retried from its first attempt. Completed,
// cancelled, and discarded rows are history the log does not need.
func moveRiverJobsToLog(ctx context.Context, tx pgx.Tx) error {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('river_job') IS NOT NULL`).Scan(&present); err != nil {
		return err
	}
	if !present {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT kind, args, max_attempts
FROM river_job
WHERE state IN ('available', 'scheduled', 'retryable', 'running', 'pending')
ORDER BY id`)
	if err != nil {
		return err
	}
	type pending struct {
		kind        string
		args        []byte
		maxAttempts int
	}
	var jobs []pending
	for rows.Next() {
		var job pending
		if err := rows.Scan(&job.kind, &job.args, &job.maxAttempts); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, job)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	logCtx := pglog.WithTx(ctx, tx)
	log := pglog.New(nil)
	now := time.Now().UTC()
	for _, job := range jobs {
		workspace, key, ok := queue.Identity(job.kind, job.args)
		if !ok || workspace == "" {
			// The order clock is periodic and needs no row; anything else
			// without an identity is not a job the log queue runs.
			continue
		}
		if _, err := logqueue.Enqueue(logCtx, log, workspace, job.kind, key, json.RawMessage(job.args), job.maxAttempts, now); err != nil {
			return fmt.Errorf("enqueue %s %s/%s: %w", job.kind, workspace, key, err)
		}
	}
	return nil
}
