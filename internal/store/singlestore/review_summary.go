package singlestore

// SingleStore review round summaries (req-review-gates-evidence AC-10.1,
// AC-10.2, AC-10.4, AC-10.7; DEC-79; feature-review-round-summary SU-2).

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// reviewSummaryClaimKey names the claim row that records a round's latest
// summary event and version (component-persistence, SingleStore claimed
// records).
func reviewSummaryClaimKey(workspace, taskID string, round int) string {
	return "review-summary:" + workspace + ":" + taskID + ":" + strconv.Itoa(round)
}

// RecordReviewSummaryCommand reads the task's history on the task
// transaction, decides replay, conflict, or the next version, and appends at
// most one review.summary_recorded event under the task-operation lock.
// Versions count the round's earlier summary events, never event IDs, which
// come from per-aggregator AUTO_INCREMENT ranges. The task lock alone does
// not make the predecessor's event visible: a commit spanning partitions can
// release the lock row first. The round's claim row therefore names the
// latest summary event and its version, the command waits until that event
// is visible before it reads the history, and the next version also counts
// from the recorded one.
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
		key := reviewSummaryClaimKey(ws, request.TaskID, request.ReviewRound)
		latest, claimed, err := awaitClaimedRecord(ctx, tx, key, ws, request.TaskID)
		if err != nil {
			return err
		}
		recorded := 0
		if claimed {
			if recorded, err = strconv.Atoi(latest.Note); err != nil {
				return fmt.Errorf("review summary claim %s records no version", key)
			}
		}
		events, err := documentTaskEvents(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}
		plan, err := store.PlanReviewSummaryAfter(ws, request, events, recorded)
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
		appended, err := lastInsertedRecord(ctx, tx, claimedEventsTable, ws, true)
		if err != nil {
			return err
		}
		var recordedAt time.Time
		if err = tx.QueryRowContext(ctx, `SELECT at FROM events_columnstore WHERE workspace_id=? AND id=?`, ws, appended.ID).Scan(&recordedAt); err != nil {
			return err
		}
		result = plan.Result(request.TaskID, appended.ID, recordedAt)
		appended.Note = strconv.Itoa(result.Version)
		return recordClaimedRecord(ctx, tx, key, appended)
	})
	if err != nil {
		return store.ReviewSummaryResult{}, err
	}
	return result, nil
}
