package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// shadowDispatchQueue keeps River authoritative and mirrors every enqueue
// into the log's job streams in the same transaction, so the observing log
// queue sees the jobs River will run. Reads are River's. A mismatch between
// the two enqueue answers is logged, not fatal: River decides.
//
// Log-core migration plan, phase 3, task 3.3.
type shadowDispatchQueue struct {
	primary dispatchQueue
	log     *pglog.Store
	now     func() time.Time
	logf    func(string, ...any)
}

// EnableQueueShadow wraps the store's queue so enqueues also land on the
// log. Call before serving traffic; idempotent.
func (s *Store) EnableQueueShadow(logf func(string, ...any)) {
	if _, already := s.queue.(*shadowDispatchQueue); already {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s.queue = &shadowDispatchQueue{primary: s.queue, log: s.logDriver(), now: time.Now, logf: logf}
}

func (q *shadowDispatchQueue) mirror(ctx context.Context, tx pgx.Tx, workspace, kind, key string, args any, maxAttempts int, primaryInserted bool) {
	inserted, err := logqueue.Enqueue(pglog.WithTx(ctx, tx), q.log, workspace, kind, key, args, maxAttempts, q.now().UTC())
	if err != nil {
		q.logf("queue shadow: %s %s %s: mirror enqueue: %v", workspace, kind, key, err)
		return
	}
	if inserted != primaryInserted {
		q.logf("queue shadow: %s %s %s: River inserted=%t, log inserted=%t", workspace, kind, key, primaryInserted, inserted)
	}
}

func (q *shadowDispatchQueue) enqueueDispatchTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) (bool, error) {
	inserted, err := q.primary.enqueueDispatchTx(ctx, tx, workspace, taskID)
	if err != nil {
		return false, err
	}
	q.mirror(ctx, tx, workspace, queueargs.DispatchTaskArgs{}.Kind(), taskID,
		queueargs.DispatchTaskArgs{WorkspaceID: workspace, TaskID: taskID}, queueargs.DispatchTaskMaxAttempts, inserted)
	return inserted, nil
}

func (q *shadowDispatchQueue) enqueueReviewPublicationTx(ctx context.Context, tx pgx.Tx, workspace, reviewWorkOrderID string) error {
	if err := q.primary.enqueueReviewPublicationTx(ctx, tx, workspace, reviewWorkOrderID); err != nil {
		return err
	}
	q.mirror(ctx, tx, workspace, queueargs.ReviewPublicationArgs{}.Kind(), reviewWorkOrderID,
		queueargs.ReviewPublicationArgs{WorkspaceID: workspace, ReviewWorkOrderID: reviewWorkOrderID}, publicationMaxAttempts, true)
	return nil
}

func (q *shadowDispatchQueue) enqueueGitHubIssuePublicationTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) error {
	if err := q.primary.enqueueGitHubIssuePublicationTx(ctx, tx, workspace, taskID); err != nil {
		return err
	}
	q.mirror(ctx, tx, workspace, queueargs.GitHubIssuePublicationArgs{}.Kind(), taskID,
		queueargs.GitHubIssuePublicationArgs{WorkspaceID: workspace, TaskID: taskID}, publicationMaxAttempts, true)
	return nil
}

func (q *shadowDispatchQueue) queuedTasksWithoutActiveDispatch(ctx context.Context, tx pgx.Tx, workspace string) ([]string, error) {
	return q.primary.queuedTasksWithoutActiveDispatch(ctx, tx, workspace)
}

func (q *shadowDispatchQueue) exhaustedDispatches(ctx context.Context, workspace string) ([]exhaustedDispatch, error) {
	return q.primary.exhaustedDispatches(ctx, workspace)
}

var _ dispatchQueue = (*shadowDispatchQueue)(nil)
