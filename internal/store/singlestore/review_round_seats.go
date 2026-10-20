package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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

// errReviewSeatAdditionCollision marks a duplicate ledger key: a concurrent
// request with the same workspace and request_id committed first.
var errReviewSeatAdditionCollision = errors.New("review seat addition request collided")

// AddReviewSeatsCommand adds operator seats to the latest open review round
// (feature-review-seat-minimum RS-5) inside taskTx, the task lock
// AcceptReviewDecisionCommand also runs under, so an addition and a final
// verdict serialize. Members, record, ledger row, and events commit together.
func (s *Store) AddReviewSeatsCommand(ctx context.Context, lease taskops.TaskLease, request store.ReviewSeatAdditionRequest) (store.ReviewSeatAdditionResult, error) {
	if err := request.Validate(); err != nil {
		return store.ReviewSeatAdditionResult{}, err
	}
	actor, err := store.RequireReviewSeatOperator(ctx)
	if err != nil {
		return store.ReviewSeatAdditionResult{}, err
	}
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return store.ReviewSeatAdditionResult{}, fmt.Errorf("review seat addition requires a valid taskops lease")
	}
	var result store.ReviewSeatAdditionResult
	err = s.taskTx(ctx, request.TaskID, func(tx *sql.Tx) error {
		ws := documentWorkspace(ctx)
		if err := lockKey(ctx, tx, "review-seat-addition-request:"+ws+":"+request.RequestID); err != nil {
			return err
		}
		prior, found, err := reviewSeatAdditionTx(ctx, tx, request.RequestID)
		if err != nil {
			return err
		}
		if found {
			result, err = store.ReviewSeatAdditionReplay(prior, request)
			return err
		}
		task, err := getTaskRow(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}
		orders, err := taskReviewOrdersTx(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}
		events, err := documentTaskEvents(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}
		records, err := reviewRoundSeatsTx(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		plan, err := store.PlanReviewSeatAddition(store.ReviewSeatAdditionInputs{Workspace: ws, Task: task, Orders: orders, Events: events,
			Records: records, Request: request, ActorID: actor.ID, Now: now})
		if err != nil {
			return err
		}
		if err = insertReviewSeatAdditionTx(ctx, tx, plan.Ledger); err != nil {
			return err
		}
		for i, job := range plan.Jobs {
			if err = s.insertReviewMemberTx(ctx, tx, job, plan.Orders[i]); err != nil {
				return err
			}
		}
		if plan.RecordExists {
			if _, err = documentExec(ctx, tx, `UPDATE review_round_seats SET seat_count=? WHERE workspace_id=? AND task_id=? AND review_round=?`, plan.Record.SeatCount, ws, request.TaskID, plan.Record.ReviewRound); err != nil {
				return err
			}
		} else if err = insertReviewRoundSeatsTx(ctx, tx, plan.Record); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, Kind: "review.seats_added", Payload: core.JSONPayload(plan.Payload), At: now}); err != nil {
			return err
		}
		result = plan.Result
		return nil
	})
	if !errors.Is(err, errReviewSeatAdditionCollision) {
		return result, err
	}
	// The colliding transaction rolled back with nothing written; the
	// committed row decides between replay and conflict.
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		prior, found, readErr := reviewSeatAdditionTx(ctx, tx, request.RequestID)
		if readErr != nil {
			return readErr
		}
		if !found {
			return fmt.Errorf("review seat addition request %s collided without a committed row", request.RequestID)
		}
		result, readErr = store.ReviewSeatAdditionReplay(prior, request)
		return readErr
	})
	return result, err
}

func reviewSeatAdditionTx(ctx context.Context, db s2log.Executor, requestID string) (store.ReviewSeatAddition, bool, error) {
	var row store.ReviewSeatAddition
	err := documentRow(ctx, db, `SELECT request_id,task_id,review_round,prior_seat_count,seat_count,reason,actor_id,created_at
		FROM review_seat_additions WHERE workspace_id=? AND request_id=?`, documentWorkspace(ctx), requestID).
		Scan(&row.RequestID, &row.TaskID, &row.ReviewRound, &row.PriorSeatCount, &row.SeatCount, &row.Reason, &row.ActorID, &row.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ReviewSeatAddition{}, false, nil
	}
	if err != nil {
		return store.ReviewSeatAddition{}, false, err
	}
	row.CreatedAt = row.CreatedAt.UTC()
	return row, true, nil
}

// insertReviewSeatAdditionTx inserts the ledger row; a duplicate key becomes
// errReviewSeatAdditionCollision so the caller can read the committed row.
func insertReviewSeatAdditionTx(ctx context.Context, tx *sql.Tx, row store.ReviewSeatAddition) error {
	_, err := documentExec(ctx, tx, `INSERT INTO review_seat_additions(workspace_id,request_id,task_id,review_round,prior_seat_count,seat_count,reason,actor_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		documentWorkspace(ctx), row.RequestID, row.TaskID, row.ReviewRound, row.PriorSeatCount, row.SeatCount, row.Reason, row.ActorID, row.CreatedAt)
	var driver *mysql.MySQLError
	if errors.As(err, &driver) && driver.Number == 1062 {
		return errReviewSeatAdditionCollision
	}
	return err
}

func taskReviewOrdersTx(ctx context.Context, tx *sql.Tx, taskID string) ([]core.WorkOrder, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='review' ORDER BY review_round, review_seat`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.WorkOrder{}
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, order)
	}
	return out, rows.Err()
}
