package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
	rows, err := documentRows(ctx, s.db, `
SELECT 'github_issue' AS kind, g.task_id, g.task_id AS publication_key, t.state, g.automatic_reenqueue_at
FROM github_lifecycles g JOIN tasks t ON t.workspace_id=g.workspace_id AND t.id=g.task_id
WHERE g.workspace_id=? AND g.state='failed'
UNION ALL
SELECT 'review' AS kind, r.task_id, r.review_work_order_id AS publication_key, t.state, r.automatic_reenqueue_at
FROM review_publications r JOIN tasks t ON t.workspace_id=r.workspace_id AND t.id=r.task_id
WHERE r.workspace_id=? AND r.state='failed'
ORDER BY kind, publication_key`, documentWorkspace(ctx), documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var retries []store.AutomaticPublicationRetry
	for rows.Next() {
		var retry store.AutomaticPublicationRetry
		var kind, taskState string
		var reenqueuedAt sql.NullTime
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

// AutomaticPublicationRetryCommand re-checks one due publication under the
// task lock and its row lock, then re-enqueues it at most once per window.
func (s *Store) AutomaticPublicationRetryCommand(ctx context.Context, lease taskops.TaskLease, retry store.AutomaticPublicationRetry, now time.Time) (bool, error) {
	if _, err := store.RequireActor(ctx); err != nil {
		return false, err
	}
	if !lease.ValidForCommand(retry.TaskID, string(store.PublicationRetryCommand)) {
		return false, fmt.Errorf("automatic publication retry requires a valid taskops lease")
	}
	acted := false
	err := s.taskTx(ctx, retry.TaskID, func(tx *sql.Tx) error {
		policy, _, err := readClockPolicyTx(ctx, tx)
		if err != nil {
			return err
		}
		var taskState string
		if err = documentRow(ctx, tx, `SELECT state FROM tasks WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), retry.TaskID).Scan(&taskState); err != nil {
			return notFound(err, "task %s", retry.TaskID)
		}
		var inserted bool
		switch retry.Kind {
		case store.AutomaticPublicationIssue:
			lifecycle, err := scanGitHubLifecycle(documentRow(ctx, tx, "SELECT "+githubLifecycleColumns+" FROM github_lifecycles WHERE workspace_id=? AND task_id=? FOR UPDATE", documentWorkspace(ctx), retry.Key))
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if lifecycle.TaskID != retry.TaskID || !store.AutomaticPublicationDue(policy, core.TaskState(taskState), lifecycle.State == core.GitHubPublicationFailed, lifecycle.AutomaticReenqueueAt, now) {
				return nil
			}
			if _, err = documentExec(ctx, tx, `UPDATE github_lifecycles SET state='retrying', automatic_reenqueue_at=?, updated_at=?
				WHERE workspace_id=? AND task_id=? AND state='failed'`, now, now, documentWorkspace(ctx), retry.Key); err != nil {
				return err
			}
			lifecycle.State, lifecycle.AutomaticReenqueueAt, lifecycle.UpdatedAt = core.GitHubPublicationRetrying, now, now
			if err = taskEvent(ctx, tx, store.AutomaticGitHubIssueRetryEvent(lifecycle, retry.RequestID(now), now)); err != nil {
				return err
			}
			if inserted, err = s.enqueueGitHubIssuePublicationTx(ctx, tx, retry.Key); err != nil {
				return err
			}
		case store.AutomaticPublicationReview:
			publication, err := scanReviewPublication(documentRow(ctx, tx, "SELECT "+reviewPublicationColumns+" FROM review_publications WHERE workspace_id=? AND review_work_order_id=? FOR UPDATE", documentWorkspace(ctx), retry.Key))
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if publication.TaskID != retry.TaskID || !store.AutomaticPublicationDue(policy, core.TaskState(taskState), publication.State == core.ReviewPublicationFailed, publication.AutomaticReenqueueAt, now) {
				return nil
			}
			if _, err = documentExec(ctx, tx, `UPDATE review_publications SET state='retrying', automatic_reenqueue_at=?, updated_at=?
				WHERE workspace_id=? AND review_work_order_id=? AND state='failed'`, now, now, documentWorkspace(ctx), retry.Key); err != nil {
				return err
			}
			publication.State, publication.AutomaticReenqueueAt, publication.UpdatedAt = core.ReviewPublicationRetrying, now, now
			if err = taskEvent(ctx, tx, store.AutomaticReviewRetryEvent(publication, retry.RequestID(now), now)); err != nil {
				return err
			}
			if inserted, err = s.enqueueReviewPublicationTx(ctx, tx, retry.Key); err != nil {
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
