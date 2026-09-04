package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue/riverqueue"
)

// dispatchQueue is the store's private view of the durable queue: the
// transactional enqueues each lifecycle command needs, and the two
// reconciliation reads. River implements it today; the log-backed queue
// implements it next. Nothing else in the store may know which.
//
// Log-core migration plan, phase 3, task 3.1.
type dispatchQueue interface {
	enqueueDispatchTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) (bool, error)
	enqueueReviewPublicationTx(ctx context.Context, tx pgx.Tx, workspace, reviewWorkOrderID string) error
	enqueueGitHubIssuePublicationTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) error
	// queuedTasksWithoutActiveDispatch lists queued tasks with no dispatch
	// row in an active state, excluding blueprint anchors whose children own
	// delivery. Runs inside the reconciliation transaction.
	queuedTasksWithoutActiveDispatch(ctx context.Context, tx pgx.Tx, workspace string) ([]string, error)
	// exhaustedDispatches lists running tasks whose latest dispatch row was
	// discarded after its final attempt with nothing active behind it.
	exhaustedDispatches(ctx context.Context, workspace string) ([]exhaustedDispatch, error)
}

type exhaustedDispatch struct {
	taskID      string
	stage       core.Stage
	attempt     int
	maxAttempts int
}

// riverDispatchQueue is dispatchQueue on River's tables.
type riverDispatchQueue struct {
	pool     *pgxpool.Pool
	inserter *riverqueue.Inserter
}

func newRiverDispatchQueue(pool *pgxpool.Pool) (*riverDispatchQueue, error) {
	inserter, err := riverqueue.NewInserter(pool)
	if err != nil {
		return nil, err
	}
	return &riverDispatchQueue{pool: pool, inserter: inserter}, nil
}

func (q *riverDispatchQueue) enqueueDispatchTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) (bool, error) {
	inserted, err := q.inserter.InsertDispatchTx(ctx, tx, workspace, taskID)
	if err != nil {
		return false, fmt.Errorf("enqueue task %s: %w", taskID, err)
	}
	return inserted, nil
}

func (q *riverDispatchQueue) enqueueReviewPublicationTx(ctx context.Context, tx pgx.Tx, workspace, reviewWorkOrderID string) error {
	return q.inserter.InsertReviewPublicationTx(ctx, tx, workspace, reviewWorkOrderID)
}

func (q *riverDispatchQueue) enqueueGitHubIssuePublicationTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) error {
	return q.inserter.InsertGitHubIssuePublicationTx(ctx, tx, workspace, taskID)
}

func (q *riverDispatchQueue) queuedTasksWithoutActiveDispatch(ctx context.Context, tx pgx.Tx, workspace string) ([]string, error) {
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
  AND NOT EXISTS (
      SELECT 1 FROM river_job r
      WHERE r.kind = 'dispatch_task'
        AND r.args->>'task_id' = t.id
        AND r.state IN ('available', 'pending', 'running', 'retryable', 'scheduled')
  )
ORDER BY t.created_at, t.id`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			return nil, err
		}
		taskIDs = append(taskIDs, taskID)
	}
	return taskIDs, rows.Err()
}

func (q *riverDispatchQueue) exhaustedDispatches(ctx context.Context, workspace string) ([]exhaustedDispatch, error) {
	rows, err := q.pool.Query(ctx, `
SELECT t.id, t.next_stage, r.attempt, r.max_attempts
FROM tasks t
JOIN LATERAL (
    SELECT attempt, max_attempts
    FROM river_job
    WHERE kind = 'dispatch_task'
      AND args->>'task_id' = t.id
      AND args->>'workspace_id' = t.workspace_id
      AND state = 'discarded'
      AND attempt >= max_attempts
    ORDER BY finalized_at DESC NULLS LAST, id DESC
    LIMIT 1
) r ON true
WHERE t.workspace_id = $1
  AND t.state = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM river_job active
      WHERE active.kind = 'dispatch_task'
        AND active.args->>'task_id' = t.id
        AND active.args->>'workspace_id' = t.workspace_id
        AND active.state IN ('available', 'pending', 'running', 'retryable', 'scheduled')
  )
ORDER BY t.created_at, t.id`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []exhaustedDispatch
	for rows.Next() {
		var candidate exhaustedDispatch
		if err := rows.Scan(&candidate.taskID, &candidate.stage, &candidate.attempt, &candidate.maxAttempts); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}
