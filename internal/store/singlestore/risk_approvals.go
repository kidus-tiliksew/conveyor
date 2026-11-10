package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// Human approvals and author waivers (req-risk-based-review AC-8.3 to AC-8.8;
// component-risk-review invariants 9 and 10; feature-risk-based-review RR-7).
// A write runs in taskTx, under the task's operation lock, validates the
// task's state and reviewed head, and inserts one row and its activity event.
// INSERT IGNORE against the primary key keeps one row per task, head, round,
// kind, and person even when a concurrent writer's row is not yet visible to
// this transaction's read, so a replay or race by the same person records one
// row and one event.

func (s *Store) RecordTaskApproval(ctx context.Context, approval core.TaskApproval) (core.TaskApproval, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordTaskApproval")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.TaskApproval{}, false, err
	}
	var result core.TaskApproval
	created := false
	err := s.taskTx(ctx, approval.TaskID, func(tx *sql.Tx) error {
		task, err := getTaskRow(ctx, tx, approval.TaskID)
		if err != nil {
			return err
		}
		prepared, err := store.PrepareTaskApproval(task, approval, time.Now())
		if err != nil {
			return err
		}
		res, err := documentExec(ctx, tx, `INSERT IGNORE INTO task_risk_approvals(workspace_id,task_id,head_sha,review_round,kind,user_id,payload_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			documentWorkspace(ctx), prepared.TaskID, prepared.HeadSHA, prepared.ReviewRound, string(prepared.Kind), prepared.UserID, core.JSONPayload(prepared), prepared.CreatedAt)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			// The person's record exists; answer it and write nothing. A
			// concurrent writer's row may not be readable yet, so the
			// prepared record stands in for it.
			result = prepared
			rows, err := listTaskApprovals(ctx, tx, prepared.TaskID)
			if err != nil {
				return err
			}
			for _, recorded := range rows {
				if store.TaskApprovalKeyOf(recorded) == store.TaskApprovalKeyOf(prepared) {
					result = recorded
				}
			}
			return nil
		}
		result, created = prepared, true
		return taskEvent(ctx, tx, store.TaskApprovalRecordedEvent(prepared))
	})
	if err != nil {
		return core.TaskApproval{}, false, err
	}
	return result, created, nil
}

func (s *Store) ListTaskApprovals(ctx context.Context, taskID string) ([]core.TaskApproval, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskApprovals")
	if _, err := getTaskRow(ctx, s.db, taskID); err != nil {
		return nil, err
	}
	return listTaskApprovals(ctx, s.db, taskID)
}

func listTaskApprovals(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, taskID string) ([]core.TaskApproval, error) {
	rows, err := db.QueryContext(ctx, `SELECT payload_json FROM task_risk_approvals WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), taskID)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(approvals, func(a, b core.TaskApproval) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return approvals, nil
}
