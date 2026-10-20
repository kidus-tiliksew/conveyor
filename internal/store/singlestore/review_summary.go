package singlestore

// SingleStore review round summaries (req-review-gates-evidence AC-10.1,
// AC-10.2, AC-10.4, AC-10.7; DEC-79; feature-review-round-summary SU-2).

import (
	"context"
	"database/sql"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RecordReviewSummaryCommand reads the task's history on the task
// transaction, decides replay, conflict, or the next version, and appends at
// most one review.summary_recorded event under the task-operation lock.
// Versions count the round's earlier summary events, never event IDs, which
// come from per-aggregator AUTO_INCREMENT ranges.
func (s *Store) RecordReviewSummaryCommand(ctx context.Context, request store.ReviewSummaryRequest) (store.ReviewSummaryResult, error) {
	if _, err := store.RequireActor(ctx); err != nil {
		return store.ReviewSummaryResult{}, err
	}
	var result store.ReviewSummaryResult
	err := s.taskTx(ctx, request.TaskID, func(tx *sql.Tx) error {
		ws := documentWorkspace(ctx)
		if _, err := getTaskRow(ctx, tx, request.TaskID); err != nil {
			return err
		}
		events, err := documentTaskEvents(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}
		plan, err := store.PlanReviewSummary(ws, request, events)
		if err != nil {
			return err
		}
		if plan.Replay != nil {
			result = *plan.Replay
			return nil
		}
		if err = taskEvent(ctx, tx, plan.Event); err != nil {
			return err
		}
		var id int64
		if err = tx.QueryRowContext(ctx, `SELECT LAST_INSERT_ID()`).Scan(&id); err != nil {
			return err
		}
		var recordedAt time.Time
		if err = tx.QueryRowContext(ctx, `SELECT at FROM events WHERE workspace_id=? AND id=?`, ws, id).Scan(&recordedAt); err != nil {
			return err
		}
		result = plan.Result(request.TaskID, id, recordedAt)
		return nil
	})
	if err != nil {
		return store.ReviewSummaryResult{}, err
	}
	return result, nil
}
