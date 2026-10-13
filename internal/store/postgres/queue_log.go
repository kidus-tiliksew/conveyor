package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

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
	boundary boundaryDB
	log      *pglog.Store
	now      func() time.Time
}

func newLogDispatchQueue(boundary boundaryDB, log *pglog.Store) *logDispatchQueue {
	return &logDispatchQueue{boundary: boundary, log: log, now: time.Now}
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

// enqueueReviewPublicationTx reports whether a new job was inserted; false
// means a job for the same publication is still active on its stream.
func (q *logDispatchQueue) enqueueReviewPublicationTx(ctx context.Context, tx pgx.Tx, workspace, reviewWorkOrderID string) (bool, error) {
	return logqueue.Enqueue(pglog.WithTx(ctx, tx), q.log, workspace,
		queue.ReviewPublicationArgs{}.Kind(), reviewWorkOrderID,
		queue.ReviewPublicationArgs{WorkspaceID: workspace, ReviewWorkOrderID: reviewWorkOrderID},
		publicationMaxAttempts, q.now().UTC())
}

// enqueueGitHubIssuePublicationTx reports whether a new job was inserted.
func (q *logDispatchQueue) enqueueGitHubIssuePublicationTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) (bool, error) {
	return logqueue.Enqueue(pglog.WithTx(ctx, tx), q.log, workspace,
		queue.GitHubIssuePublicationArgs{}.Kind(), taskID,
		queue.GitHubIssuePublicationArgs{WorkspaceID: workspace, TaskID: taskID},
		publicationMaxAttempts, q.now().UTC())
}

// blockingOrderSQL is the SQL form of the durable reconcile hold predicate:
// the task's latest order is at its next_stage and holds it (itself, or for
// review any seat of its round, is queued, claimed, stale, or timed_out), or
// any stored same-stage claim holds executor exclusivity across stage history.
// Read-time stale and timed_out projections are stored as queued or claimed,
// so the stored set covers them. A stored claim must remain visible until the
// order clock persists its expiry (component-durable-queue
// "Reconciliation between tasks and jobs"; DEC-62).
func blockingOrderSQL(task string) string {
	return `(
  EXISTS (
      SELECT 1 FROM work_orders latest
      WHERE latest.workspace_id=` + task + `.workspace_id AND latest.task_id=` + task + `.id
        AND latest.stage=` + task + `.next_stage
        AND NOT EXISTS (
            SELECT 1 FROM work_orders later
            WHERE later.workspace_id=latest.workspace_id AND later.task_id=latest.task_id
              AND (later.created_at > latest.created_at OR (later.created_at = latest.created_at AND later.id > latest.id))
        )
        AND EXISTS (
            SELECT 1 FROM work_orders wo
            WHERE wo.workspace_id=latest.workspace_id AND wo.task_id=latest.task_id
              AND (wo.id=latest.id OR (latest.stage='review' AND wo.stage='review' AND wo.review_round=latest.review_round))
              AND wo.state IN ('queued','claimed','stale','timed_out')
        )
  )
  OR EXISTS (
      SELECT 1 FROM work_orders claimed
      WHERE claimed.workspace_id=` + task + `.workspace_id
        AND claimed.task_id=` + task + `.id
        AND claimed.stage=` + task + `.next_stage
        AND claimed.state='claimed'
  )
)`
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
  AND NOT (`+blockingOrderSQL("t")+`)
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
	rows, err := q.boundary.Query(ctx, `
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
