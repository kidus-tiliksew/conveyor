package postgres

// PostgreSQL review round summaries (req-review-gates-evidence AC-10.1,
// AC-10.2, AC-10.4, AC-10.7; DEC-79; feature-review-round-summary SU-2).

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// RecordReviewSummaryCommand reads the task's history, decides replay,
// conflict, or the next version, and appends at most one
// review.summary_recorded event in one transaction under the task-operation
// advisory locks, so concurrent requests for a task serialize and receive
// consecutive versions.
func (s *Store) RecordReviewSummaryCommand(ctx context.Context, request store.ReviewSummaryRequest) (store.ReviewSummaryResult, error) {
	if _, err := store.RequireActor(ctx); err != nil {
		return store.ReviewSummaryResult{}, err
	}
	var result store.ReviewSummaryResult
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		ws := workspace(ctx)
		if err := lockWorkOrderTaskTx(ctx, tx, ws, request.TaskID); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM tasks WHERE workspace_id=$1 AND id=$2`, ws, request.TaskID).Scan(&exists); err != nil {
			return notFound(err, "task %s", request.TaskID)
		}
		rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(request.TaskID), WorkspaceID: ws})
		if err != nil {
			return err
		}
		events := make([]core.Event, len(rows))
		for i := range rows {
			events[i] = eventFromDB(rows[i])
		}
		plan, err := store.PlanReviewSummary(ws, request, events)
		if err != nil {
			return err
		}
		if plan.Replay != nil {
			result = *plan.Replay
			return nil
		}
		id, err := insertEventWithID(ctx, q, plan.Event)
		if err != nil {
			return err
		}
		var recordedAt time.Time
		if err = tx.QueryRow(ctx, `SELECT at FROM events WHERE workspace_id=$1 AND id=$2`, ws, id).Scan(&recordedAt); err != nil {
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
