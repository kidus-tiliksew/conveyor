package singlestore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ListReviewRoundSeats returns the selected workspace's review round seat
// records for a task, ordered by round (feature-review-seat-minimum RS-2).
func (s *Store) ListReviewRoundSeats(ctx context.Context, taskID string) ([]store.ReviewRoundSeats, error) {
	return reviewRoundSeatsTx(ctx, s.db, taskID)
}

func reviewRoundSeatsTx(ctx context.Context, db s2log.Executor, taskID string) ([]store.ReviewRoundSeats, error) {
	rows, err := documentRows(ctx, db, `SELECT task_id,review_round,seat_count,seat_count_source,minimum_seat_count,implementation_work_order_id,created_at
		FROM review_round_seats WHERE workspace_id=? AND task_id=? ORDER BY review_round`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []store.ReviewRoundSeats{}
	for rows.Next() {
		var record store.ReviewRoundSeats
		if err = rows.Scan(&record.TaskID, &record.ReviewRound, &record.SeatCount, &record.SeatCountSource, &record.MinimumSeatCount, &record.ImplementationWorkOrderID, &record.CreatedAt); err != nil {
			return nil, err
		}
		record.CreatedAt = record.CreatedAt.UTC()
		records = append(records, record)
	}
	return records, rows.Err()
}

func reviewRoundSeatsExistTx(ctx context.Context, tx *sql.Tx, taskID string, round int) (bool, error) {
	var exists bool
	err := documentRow(ctx, tx, `SELECT EXISTS(SELECT 1 FROM review_round_seats WHERE workspace_id=? AND task_id=? AND review_round=?)`, documentWorkspace(ctx), taskID, round).Scan(&exists)
	return exists, err
}

// approvedPlannedSeatsTx mirrors GetApprovedSpecVersion: the newest approved
// plan version's review_seats.
func approvedPlannedSeatsTx(ctx context.Context, db s2log.Executor, taskID string) (*int, error) {
	var planned sql.NullInt64
	err := documentRow(ctx, db, `SELECT review_seats FROM task_specs WHERE workspace_id=? AND task_id=? AND approved ORDER BY version DESC LIMIT 1`, documentWorkspace(ctx), taskID).Scan(&planned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return seatCountFromNull(planned), nil
}

// checkReviewRoundSeatsTx revalidates caller-resolved seats against the
// stored setup, approved plan count, and records read in the transaction.
func checkReviewRoundSeatsTx(ctx context.Context, tx *sql.Tx, task core.Task, seats store.ReviewRoundSeatResolution, orderCount int) error {
	planned, err := approvedPlannedSeatsTx(ctx, tx, task.ID)
	if err != nil {
		return err
	}
	records, err := reviewRoundSeatsTx(ctx, tx, task.ID)
	if err != nil {
		return err
	}
	return store.CheckReviewRoundSeats(seats, task.SetupContract, planned, records, orderCount)
}

// insertReviewRoundSeatsTx validates in Go because SingleStore does not
// enforce CHECK constraints.
func insertReviewRoundSeatsTx(ctx context.Context, tx *sql.Tx, record store.ReviewRoundSeats) error {
	if err := record.Validate(); err != nil {
		return err
	}
	_, err := documentExec(ctx, tx, `INSERT INTO review_round_seats(workspace_id,task_id,review_round,seat_count,seat_count_source,minimum_seat_count,implementation_work_order_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		documentWorkspace(ctx), record.TaskID, record.ReviewRound, record.SeatCount, record.SeatCountSource, record.MinimumSeatCount, record.ImplementationWorkOrderID, record.CreatedAt)
	return err
}

// policyHandoffSeatInputsTx loads PlanTaskPolicyChange's seat inputs inside
// the policy-change transaction.
func policyHandoffSeatInputsTx(ctx context.Context, tx *sql.Tx, taskID string) (store.PolicyHandoffSeatInputs, error) {
	events, err := documentTaskEvents(ctx, tx, taskID)
	if err != nil {
		return store.PolicyHandoffSeatInputs{}, err
	}
	records, err := reviewRoundSeatsTx(ctx, tx, taskID)
	if err != nil {
		return store.PolicyHandoffSeatInputs{}, err
	}
	planned, err := approvedPlannedSeatsTx(ctx, tx, taskID)
	if err != nil {
		return store.PolicyHandoffSeatInputs{}, err
	}
	return store.PolicyHandoffSeatInputs{Events: events, Records: records, PlannedSeatCount: planned}, nil
}

// seatCountFromNull and nullSeatCount map nullable seat counts.
func seatCountFromNull(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	count := int(value.Int64)
	return &count
}

func nullSeatCount(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
