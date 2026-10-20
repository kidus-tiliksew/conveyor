package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// ListReviewRoundSeats returns the selected workspace's review round seat
// records for a task, ordered by round (feature-review-seat-minimum RS-2).
func (s *Store) ListReviewRoundSeats(ctx context.Context, taskID string) ([]store.ReviewRoundSeats, error) {
	return reviewRoundSeatsTx(ctx, s.boundary, workspace(ctx), taskID)
}

func reviewRoundSeatsTx(ctx context.Context, q boundaryQuerier, workspaceID, taskID string) ([]store.ReviewRoundSeats, error) {
	rows, err := q.Query(ctx, `SELECT task_id,review_round,seat_count,seat_count_source,minimum_seat_count,implementation_work_order_id,created_at
		FROM review_round_seats WHERE workspace_id=$1 AND task_id=$2 ORDER BY review_round`, workspaceID, taskID)
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

func reviewRoundSeatsExistTx(ctx context.Context, q boundaryQuerier, workspaceID, taskID string, round int) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM review_round_seats WHERE workspace_id=$1 AND task_id=$2 AND review_round=$3)`, workspaceID, taskID, round).Scan(&exists)
	return exists, err
}

// approvedPlannedSeatsTx mirrors GetApprovedSpecVersion: the newest approved
// plan version's review_seats.
func approvedPlannedSeatsTx(ctx context.Context, q boundaryQuerier, workspaceID, taskID string) (*int, error) {
	var planned *int
	err := q.QueryRow(ctx, `SELECT s.review_seats FROM task_specs s JOIN tasks t ON t.id=s.task_id
		WHERE s.task_id=$1 AND t.workspace_id=$2 AND s.approved ORDER BY s.version DESC LIMIT 1`, taskID, workspaceID).Scan(&planned)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return planned, err
}

// checkReviewRoundSeatsTx revalidates caller-resolved seats against the
// stored setup, approved plan count, and records read in the transaction.
func checkReviewRoundSeatsTx(ctx context.Context, q boundaryQuerier, workspaceID string, task core.Task, seats store.ReviewRoundSeatResolution, orderCount int) error {
	planned, err := approvedPlannedSeatsTx(ctx, q, workspaceID, task.ID)
	if err != nil {
		return err
	}
	records, err := reviewRoundSeatsTx(ctx, q, workspaceID, task.ID)
	if err != nil {
		return err
	}
	return store.CheckReviewRoundSeats(seats, task.SetupContract, planned, records, orderCount)
}

func insertReviewRoundSeatsTx(ctx context.Context, q boundaryQuerier, workspaceID string, record store.ReviewRoundSeats) error {
	if err := record.Validate(); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO review_round_seats (workspace_id,task_id,review_round,seat_count,seat_count_source,minimum_seat_count,implementation_work_order_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, workspaceID, record.TaskID, record.ReviewRound, record.SeatCount, record.SeatCountSource, record.MinimumSeatCount, record.ImplementationWorkOrderID, record.CreatedAt)
	return err
}

// policyHandoffSeatInputsTx loads PlanTaskPolicyChange's seat inputs inside
// the policy-change transaction.
func policyHandoffSeatInputsTx(ctx context.Context, tx pgx.Tx, q *db.Queries, workspaceID, taskID string) (store.PolicyHandoffSeatInputs, error) {
	rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(taskID), WorkspaceID: workspaceID})
	if err != nil {
		return store.PolicyHandoffSeatInputs{}, err
	}
	events := make([]core.Event, len(rows))
	for i := range rows {
		events[i] = eventFromDB(rows[i])
	}
	records, err := reviewRoundSeatsTx(ctx, tx, workspaceID, taskID)
	if err != nil {
		return store.PolicyHandoffSeatInputs{}, err
	}
	planned, err := approvedPlannedSeatsTx(ctx, tx, workspaceID, taskID)
	if err != nil {
		return store.PolicyHandoffSeatInputs{}, err
	}
	return store.PolicyHandoffSeatInputs{Events: events, Records: records, PlannedSeatCount: planned}, nil
}
