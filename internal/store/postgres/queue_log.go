package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

type exhaustedDispatch struct {
	taskID      string
	stage       core.Stage
	attempt     int
	maxAttempts int
}

// logDispatchQueue is the store's durable queue on the event log. Enqueues
// run inside the caller's transaction by binding it to the log driver's
// context, so a lifecycle command's rows and its job commit together. The reconciliation reads keep their SQL over tasks and
// fold each candidate's job stream for the queue half of the answer.
type logDispatchQueue struct {
	pool *pgxpool.Pool
	log  *pglog.Store
	now  func() time.Time
}

func newLogDispatchQueue(pool *pgxpool.Pool, log *pglog.Store) *logDispatchQueue {
	return &logDispatchQueue{pool: pool, log: log, now: time.Now}
}

const publicationMaxAttempts = 5

func (q *logDispatchQueue) enqueueDispatchTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) (bool, error) {
	inserted, err := logqueue.Enqueue(pglog.WithTx(ctx, tx), q.log, workspace,
		queue.DispatchTaskArgs{}.Kind(), taskID,
		queue.DispatchTaskArgs{WorkspaceID: workspace, TaskID: taskID},
		queue.DispatchTaskMaxAttempts, q.now().UTC())
	if err != nil {
		return false, fmt.Errorf("enqueue task %s: %w", taskID, err)
	}
	return inserted, nil
}

func (q *logDispatchQueue) enqueueReviewPublicationTx(ctx context.Context, tx pgx.Tx, workspace, reviewWorkOrderID string) error {
	_, err := logqueue.Enqueue(pglog.WithTx(ctx, tx), q.log, workspace,
		queue.ReviewPublicationArgs{}.Kind(), reviewWorkOrderID,
		queue.ReviewPublicationArgs{WorkspaceID: workspace, ReviewWorkOrderID: reviewWorkOrderID},
		publicationMaxAttempts, q.now().UTC())
	return err
}

func (q *logDispatchQueue) enqueueGitHubIssuePublicationTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) error {
	_, err := logqueue.Enqueue(pglog.WithTx(ctx, tx), q.log, workspace,
		queue.GitHubIssuePublicationArgs{}.Kind(), taskID,
		queue.GitHubIssuePublicationArgs{WorkspaceID: workspace, TaskID: taskID},
		publicationMaxAttempts, q.now().UTC())
	return err
}

func (q *logDispatchQueue) queuedTasksWithoutActiveDispatch(ctx context.Context, tx pgx.Tx, workspace string) ([]string, error) {
	rows, err := tx.Query(ctx, `
SELECT t.id
FROM tasks t
WHERE t.workspace_id = $1
  AND t.state = 'queued'
  AND NOT (
      t.parent_task_id IS NULL
      AND t.next_stage = 'implement'
      AND EXISTS (
          SELECT 1 FROM tasks child
          WHERE child.workspace_id=t.workspace_id
            AND child.parent_task_id=t.id
      )
  )
ORDER BY t.created_at, t.id`, workspace)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, taskID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logCtx := pglog.WithTx(ctx, tx)
	kind := queue.DispatchTaskArgs{}.Kind()
	var out []string
	for _, taskID := range candidates {
		job, err := logqueue.Load(logCtx, q.log, workspace, logqueue.StreamFor(kind, taskID))
		if err != nil {
			return nil, err
		}
		if !job.Active() {
			out = append(out, taskID)
		}
	}
	return out, nil
}

func (q *logDispatchQueue) exhaustedDispatches(ctx context.Context, workspace string) ([]exhaustedDispatch, error) {
	rows, err := q.pool.Query(ctx, `
SELECT t.id, t.next_stage
FROM tasks t
WHERE t.workspace_id = $1 AND t.state = 'running'
ORDER BY t.created_at, t.id`, workspace)
	if err != nil {
		return nil, err
	}
	type running struct {
		id    string
		stage core.Stage
	}
	var candidates []running
	for rows.Next() {
		var r running
		if err := rows.Scan(&r.id, &r.stage); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kind := queue.DispatchTaskArgs{}.Kind()
	var out []exhaustedDispatch
	for _, r := range candidates {
		job, err := logqueue.Load(ctx, q.log, workspace, logqueue.StreamFor(kind, r.id))
		if err != nil {
			return nil, err
		}
		if job.State == logqueue.StateDiscarded && job.Attempt >= job.MaxAttempts {
			out = append(out, exhaustedDispatch{taskID: r.id, stage: r.stage, attempt: job.Attempt, maxAttempts: job.MaxAttempts})
		}
	}
	return out, nil
}
