package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ListAutomaticPublicationRetries lists failed publications due for their
// bounded automatic re-enqueue (feature-bounded-automatic-recovery AR-5).
func (s *Store) ListAutomaticPublicationRetries(ctx context.Context, now time.Time) ([]store.AutomaticPublicationRetry, error) {
	policy, _, err := s.readClockPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if policy.AutomaticRecoveries == 0 {
		return nil, nil
	}
	rows, err := s.boundary.Query(ctx, `
SELECT 'github_issue', g.task_id, g.task_id, t.state, g.automatic_reenqueue_at
FROM github_lifecycles g JOIN tasks t ON t.workspace_id=g.workspace_id AND t.id=g.task_id
WHERE g.workspace_id=$1 AND g.state='failed'
UNION ALL
SELECT 'review', r.task_id, r.review_work_order_id, t.state, r.automatic_reenqueue_at
FROM review_publications r JOIN tasks t ON t.workspace_id=r.workspace_id AND t.id=r.task_id
WHERE r.workspace_id=$1 AND r.state='failed'
ORDER BY 1, 3`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var retries []store.AutomaticPublicationRetry
	for rows.Next() {
		var retry store.AutomaticPublicationRetry
		var kind, taskState string
		var reenqueuedAt pgtype.Timestamptz
		if err = rows.Scan(&kind, &retry.TaskID, &retry.Key, &taskState, &reenqueuedAt); err != nil {
			return nil, err
		}
		retry.Kind = store.AutomaticPublicationKind(kind)
		if store.AutomaticPublicationDue(policy, core.TaskState(taskState), true, reenqueuedAt.Time, now) {
			retries = append(retries, retry)
		}
	}
	return retries, rows.Err()
}

// AutomaticPublicationRetryCommand re-checks one due publication under its
// row lock and re-enqueues it at most once per recovery window.
func (s *Store) AutomaticPublicationRetryCommand(ctx context.Context, lease taskops.TaskLease, retry store.AutomaticPublicationRetry, now time.Time) (bool, error) {
	if _, err := store.RequireActor(ctx); err != nil {
		return false, err
	}
	if !lease.ValidForCommand(retry.TaskID, string(store.PublicationRetryCommand)) {
		return false, fmt.Errorf("automatic publication retry requires a valid taskops lease")
	}
	acted := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		policy, _, err := readClockPolicyTx(ctx, tx)
		if err != nil {
			return err
		}
		var taskState string
		if err = tx.QueryRow(ctx, `SELECT state FROM tasks WHERE workspace_id=$1 AND id=$2`, workspace(ctx), retry.TaskID).Scan(&taskState); err != nil {
			return notFound(err, "task %s", retry.TaskID)
		}
		var inserted bool
		switch retry.Kind {
		case store.AutomaticPublicationIssue:
			lifecycle, err := scanGitHubLifecycle(tx.QueryRow(ctx, "SELECT "+githubLifecycleColumns+" FROM github_lifecycles WHERE workspace_id=$1 AND task_id=$2 FOR UPDATE", workspace(ctx), retry.Key))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if lifecycle.TaskID != retry.TaskID || !store.AutomaticPublicationDue(policy, core.TaskState(taskState), lifecycle.State == core.GitHubPublicationFailed, lifecycle.AutomaticReenqueueAt, now) {
				return nil
			}
			if _, err = tx.Exec(ctx, `UPDATE github_lifecycles SET state='retrying', automatic_reenqueue_at=$1, updated_at=$1
				WHERE workspace_id=$2 AND task_id=$3 AND state='failed'`, now, workspace(ctx), retry.Key); err != nil {
				return err
			}
			lifecycle.State, lifecycle.AutomaticReenqueueAt, lifecycle.UpdatedAt = core.GitHubPublicationRetrying, now, now
			if err = insertEvent(ctx, q, store.AutomaticGitHubIssueRetryEvent(lifecycle, retry.RequestID(now), now)); err != nil {
				return err
			}
			if inserted, err = s.queue.enqueueGitHubIssuePublicationTx(ctx, tx, workspace(ctx), retry.Key); err != nil {
				return err
			}
		case store.AutomaticPublicationReview:
			publication, err := scanReviewPublication(tx.QueryRow(ctx, "SELECT "+reviewPublicationColumns+" FROM review_publications WHERE workspace_id=$1 AND review_work_order_id=$2 FOR UPDATE", workspace(ctx), retry.Key))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if publication.TaskID != retry.TaskID || !store.AutomaticPublicationDue(policy, core.TaskState(taskState), publication.State == core.ReviewPublicationFailed, publication.AutomaticReenqueueAt, now) {
				return nil
			}
			if _, err = tx.Exec(ctx, `UPDATE review_publications SET state='retrying', automatic_reenqueue_at=$1, updated_at=$1
				WHERE workspace_id=$2 AND review_work_order_id=$3 AND state='failed'`, now, workspace(ctx), retry.Key); err != nil {
				return err
			}
			publication.State, publication.AutomaticReenqueueAt, publication.UpdatedAt = core.ReviewPublicationRetrying, now, now
			if err = insertEvent(ctx, q, store.AutomaticReviewRetryEvent(publication, retry.RequestID(now), now)); err != nil {
				return err
			}
			if inserted, err = s.queue.enqueueReviewPublicationTx(ctx, tx, workspace(ctx), retry.Key); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown automatic publication kind %q", retry.Kind)
		}
		if !inserted {
			return store.ErrAutomaticPublicationJobActive
		}
		acted = true
		return nil
	})
	if errors.Is(err, store.ErrAutomaticPublicationJobActive) {
		return false, nil
	}
	return acted, err
}
