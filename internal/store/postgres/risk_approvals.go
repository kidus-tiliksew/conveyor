package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// Human approvals and author waivers (req-risk-based-review AC-8.3 to AC-8.8;
// component-risk-review invariants 9 and 10; feature-risk-based-review RR-7).
// A write takes the task's operation lock, validates the task's state and
// reviewed head, and inserts one row and its activity event in the same
// transaction. The primary key holds one row per task, head, round, kind,
// and person, so a concurrent request by the same person inserts nothing.

func (s *Store) RecordTaskApproval(ctx context.Context, approval core.TaskApproval) (core.TaskApproval, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordTaskApproval")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.TaskApproval{}, false, err
	}
	var result core.TaskApproval
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), approval.TaskID); err != nil {
			return err
		}
		row, err := q.GetTask(ctx, db.GetTaskParams{ID: approval.TaskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", approval.TaskID)
		}
		prepared, err := store.PrepareTaskApproval(taskFromDB(row), approval, time.Now())
		if err != nil {
			return err
		}
		result, created, err = insertTaskApprovalTx(ctx, tx, q, prepared)
		return err
	})
	if err != nil {
		return core.TaskApproval{}, false, err
	}
	return result, created, nil
}

func (s *Store) ListTaskApprovals(ctx context.Context, taskID string) ([]core.TaskApproval, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskApprovals")
	if _, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)}); err != nil {
		return nil, notFound(err, "task %s", taskID)
	}
	rows, err := s.boundary.Query(ctx, `SELECT payload_json FROM task_risk_approvals WHERE workspace_id=$1 AND task_id=$2 ORDER BY seq`, workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	approvals := []core.TaskApproval{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var approval core.TaskApproval
		if err := json.Unmarshal(raw, &approval); err != nil {
			return nil, err
		}
		approvals = append(approvals, approval)
	}
	return approvals, rows.Err()
}

// insertTaskApprovalTx inserts one prepared approval and its activity event
// inside the caller's transaction, which holds the task lock. A key already
// recorded, by a replay or a concurrent writer, inserts nothing and returns
// the recorded row and false.
func insertTaskApprovalTx(ctx context.Context, tx pgx.Tx, q *db.Queries, prepared core.TaskApproval) (core.TaskApproval, bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO task_risk_approvals(workspace_id,task_id,head_sha,review_round,kind,user_id,payload_json,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (workspace_id,task_id,head_sha,review_round,kind,user_id) DO NOTHING`,
		workspace(ctx), prepared.TaskID, prepared.HeadSHA, prepared.ReviewRound, string(prepared.Kind), prepared.UserID, core.JSONPayload(prepared), prepared.CreatedAt)
	if err != nil {
		return core.TaskApproval{}, false, err
	}
	if tag.RowsAffected() == 0 {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT payload_json FROM task_risk_approvals WHERE workspace_id=$1 AND task_id=$2 AND head_sha=$3 AND review_round=$4 AND kind=$5 AND user_id=$6`,
			workspace(ctx), prepared.TaskID, prepared.HeadSHA, prepared.ReviewRound, string(prepared.Kind), prepared.UserID).Scan(&raw); err != nil {
			return core.TaskApproval{}, false, err
		}
		var recorded core.TaskApproval
		if err := json.Unmarshal(raw, &recorded); err != nil {
			return core.TaskApproval{}, false, err
		}
		return recorded, false, nil
	}
	return prepared, true, insertEvent(ctx, q, store.TaskApprovalRecordedEvent(prepared))
}
