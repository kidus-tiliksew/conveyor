package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// Risk policy status publication rows (req-risk-based-review AC-10.1,
// AC-10.2; component-git-delivery invariant 37). Every write takes the task's
// operation lock, so one row per task and head changes in one transaction at
// a time. A queue writes the row, its event, and the head's job together.

const reviewPolicyStatusMaxAttempts = publicationMaxAttempts

func (s *Store) QueueReviewPolicyStatus(ctx context.Context, taskID, head, desired string) (core.ReviewPolicyStatus, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "QueueReviewPolicyStatus")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	head = strings.TrimSpace(head)
	var result core.ReviewPolicyStatus
	wrote := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
			return err
		}
		row, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", taskID)
		}
		existing, found, err := getReviewPolicyStatus(ctx, tx, taskID, head)
		if err != nil {
			return err
		}
		var current *core.ReviewPolicyStatus
		if found {
			current = &existing
		}
		next, write, err := store.PlanReviewPolicyStatusQueue(taskFromDB(row), current, head, desired, time.Now())
		if err != nil || !write {
			result = next
			return err
		}
		if found {
			err = updateReviewPolicyStatusRow(ctx, tx, existing.Revision, next)
		} else {
			err = insertReviewPolicyStatusRow(ctx, tx, next)
		}
		if err != nil {
			return err
		}
		if err := insertEvent(ctx, q, store.ReviewPolicyStatusEvent(next)); err != nil {
			return err
		}
		args := queue.ReviewPolicyStatusArgs{WorkspaceID: workspace(ctx), TaskID: taskID, HeadSHA: head}
		if _, err := logqueue.Enqueue(logTx(ctx, tx), s.log, workspace(ctx), args.Kind(), args.UniqueKey(), args, reviewPolicyStatusMaxAttempts, time.Now().UTC()); err != nil {
			return fmt.Errorf("enqueue policy status of task %s head %s: %w", taskID, head, err)
		}
		result, wrote = next, true
		return nil
	})
	if err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	return result, wrote, nil
}

func (s *Store) GetReviewPolicyStatus(ctx context.Context, taskID, head string) (core.ReviewPolicyStatus, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetReviewPolicyStatus")
	if _, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)}); err != nil {
		return core.ReviewPolicyStatus{}, false, notFound(err, "task %s", taskID)
	}
	return getReviewPolicyStatus(ctx, s.boundary, taskID, head)
}

func (s *Store) ListReviewPolicyStatuses(ctx context.Context, taskID string) ([]core.ReviewPolicyStatus, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListReviewPolicyStatuses")
	if _, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)}); err != nil {
		return nil, notFound(err, "task %s", taskID)
	}
	return queryReviewPolicyStatuses(ctx, s.boundary, `SELECT payload_json FROM review_policy_statuses WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at, head_sha`, workspace(ctx), taskID)
}

func (s *Store) ListOpenReviewPolicyStatuses(ctx context.Context) ([]core.ReviewPolicyStatus, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListOpenReviewPolicyStatuses")
	return queryReviewPolicyStatuses(ctx, s.boundary, `SELECT payload_json FROM review_policy_statuses WHERE workspace_id=$1 AND state <> 'published' ORDER BY created_at, task_id, head_sha`, workspace(ctx))
}

func (s *Store) UpdateReviewPolicyStatus(ctx context.Context, status core.ReviewPolicyStatus) (core.ReviewPolicyStatus, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "UpdateReviewPolicyStatus")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	var result core.ReviewPolicyStatus
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), status.TaskID); err != nil {
			return err
		}
		current, found, err := getReviewPolicyStatus(ctx, tx, status.TaskID, status.HeadSHA)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: policy status of task %s head %s", store.ErrNotFound, status.TaskID, status.HeadSHA)
		}
		next, err := store.PrepareReviewPolicyStatusUpdate(current, status, time.Now())
		if err != nil {
			return err
		}
		if err := updateReviewPolicyStatusRow(ctx, tx, current.Revision, next); err != nil {
			return err
		}
		result = next
		return insertEvent(ctx, q, store.ReviewPolicyStatusEvent(next))
	})
	if err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	return result, nil
}

type reviewPolicyStatusQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func getReviewPolicyStatus(ctx context.Context, q reviewPolicyStatusQuerier, taskID, head string) (core.ReviewPolicyStatus, bool, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT payload_json FROM review_policy_statuses WHERE workspace_id=$1 AND task_id=$2 AND head_sha=$3`, workspace(ctx), taskID, head).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.ReviewPolicyStatus{}, false, nil
	}
	if err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	var row core.ReviewPolicyStatus
	if err := json.Unmarshal(raw, &row); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	return row, true, nil
}

func queryReviewPolicyStatuses(ctx context.Context, q reviewPolicyStatusQuerier, query string, args ...any) ([]core.ReviewPolicyStatus, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.ReviewPolicyStatus{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var row core.ReviewPolicyStatus
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func insertReviewPolicyStatusRow(ctx context.Context, tx pgx.Tx, row core.ReviewPolicyStatus) error {
	_, err := tx.Exec(ctx, `INSERT INTO review_policy_statuses(workspace_id,task_id,head_sha,state,desired,revision,payload_json,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		workspace(ctx), row.TaskID, row.HeadSHA, string(row.State), row.Desired, row.Revision, core.JSONPayload(row), row.CreatedAt, row.UpdatedAt)
	return err
}

// updateReviewPolicyStatusRow writes next over the row at revision. The task
// lock serializes writers, so a guard miss is a broken invariant, not a race
// to retry.
func updateReviewPolicyStatusRow(ctx context.Context, tx pgx.Tx, revision int, next core.ReviewPolicyStatus) error {
	tag, err := tx.Exec(ctx, `UPDATE review_policy_statuses SET state=$4, desired=$5, revision=$6, payload_json=$7, updated_at=$8
WHERE workspace_id=$1 AND task_id=$2 AND head_sha=$3 AND revision=$9`,
		workspace(ctx), next.TaskID, next.HeadSHA, string(next.State), next.Desired, next.Revision, core.JSONPayload(next), next.UpdatedAt, revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return store.ErrReviewPolicyStatusStale
	}
	return nil
}

// reopenReviewPolicyStatusesTx reopens every published or failed row of an
// enforcing task inside the caller's transaction, which holds the task lock,
// and enqueues each reopened head's job, so the handler re-reads the head's
// requirements before GitHub keeps an obsolete success.
func (s *Store) reopenReviewPolicyStatusesTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task) error {
	if !task.SetupContract.Risk.Enforcing() {
		return nil
	}
	rows, err := queryReviewPolicyStatuses(ctx, tx, `SELECT payload_json FROM review_policy_statuses WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at, head_sha`, workspace(ctx), task.ID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		next, ok := store.PlanReviewPolicyStatusReopen(row, time.Now())
		if !ok {
			continue
		}
		if err := updateReviewPolicyStatusRow(ctx, tx, row.Revision, next); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, store.ReviewPolicyStatusEvent(next)); err != nil {
			return err
		}
		args := queue.ReviewPolicyStatusArgs{WorkspaceID: workspace(ctx), TaskID: next.TaskID, HeadSHA: next.HeadSHA}
		if _, err := logqueue.Enqueue(logTx(ctx, tx), s.log, workspace(ctx), args.Kind(), args.UniqueKey(), args, reviewPolicyStatusMaxAttempts, time.Now().UTC()); err != nil {
			return fmt.Errorf("enqueue policy status of task %s head %s: %w", next.TaskID, next.HeadSHA, err)
		}
	}
	return nil
}
