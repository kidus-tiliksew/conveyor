package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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

// errReviewSeatAdditionCollision marks a duplicate ledger key: a concurrent
// request with the same workspace and request_id committed first.
var errReviewSeatAdditionCollision = errors.New("review seat addition request collided")

// AddReviewSeatsCommand adds operator seats to the latest open review round
// (feature-review-seat-minimum RS-5). The request advisory lock and the
// ledger read come first, then lockWorkOrderTaskTx, the lock
// AcceptReviewDecisionCommand takes first, so an addition and a final verdict
// serialize. Members, record, ledger row, and events commit together.
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
	workspaceID := workspace(ctx)
	var result store.ReviewSeatAdditionResult
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:review-seat-addition-request:"+workspaceID+":"+request.RequestID); err != nil {
			return err
		}
		prior, found, err := reviewSeatAdditionTx(ctx, tx, workspaceID, request.RequestID)
		if err != nil {
			return err
		}
		if found {
			result, err = store.ReviewSeatAdditionReplay(prior, request)
			return err
		}
		if err = lockWorkOrderTaskTx(ctx, tx, workspaceID, request.TaskID); err != nil {
			return err
		}
		taskRow, err := q.GetTask(ctx, db.GetTaskParams{ID: request.TaskID, WorkspaceID: workspaceID})
		if err != nil {
			return notFound(err, "task %s", request.TaskID)
		}
		orders, err := taskReviewOrdersTx(ctx, tx, workspaceID, request.TaskID)
		if err != nil {
			return err
		}
		rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(request.TaskID), WorkspaceID: workspaceID})
		if err != nil {
			return err
		}
		events := make([]core.Event, len(rows))
		for i := range rows {
			events[i] = eventFromDB(rows[i])
		}
		records, err := reviewRoundSeatsTx(ctx, tx, workspaceID, request.TaskID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		plan, err := store.PlanReviewSeatAddition(store.ReviewSeatAdditionInputs{Workspace: workspaceID, Task: taskFromDB(taskRow), Orders: orders, Events: events,
			Records: records, Request: request, ActorID: actor.ID, Now: now})
		if err != nil {
			return err
		}
		if err = insertReviewSeatAdditionTx(ctx, tx, workspaceID, plan.Ledger); err != nil {
			return err
		}
		for i, job := range plan.Jobs {
			if _, err = q.InsertJob(ctx, jobInsertParams(job)); err != nil {
				return err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job), At: now}); err != nil {
				return err
			}
			order := plan.Orders[i]
			order.UpdatedAt = now
			if err = insertReviewSeatAdditionOrderTx(ctx, tx, workspaceID, order); err != nil {
				return err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order), At: now}); err != nil {
				return err
			}
		}
		if plan.RecordExists {
			if _, err = tx.Exec(ctx, `UPDATE review_round_seats SET seat_count=$4 WHERE workspace_id=$1 AND task_id=$2 AND review_round=$3`, workspaceID, request.TaskID, plan.Record.ReviewRound, plan.Record.SeatCount); err != nil {
				return err
			}
		} else if err = insertReviewRoundSeatsTx(ctx, tx, workspaceID, plan.Record); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "review.seats_added", Payload: core.JSONPayload(plan.Payload), At: now}); err != nil {
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
	err = s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		prior, found, readErr := reviewSeatAdditionTx(ctx, tx, workspaceID, request.RequestID)
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

func reviewSeatAdditionTx(ctx context.Context, q boundaryQuerier, workspaceID, requestID string) (store.ReviewSeatAddition, bool, error) {
	var row store.ReviewSeatAddition
	err := q.QueryRow(ctx, `SELECT request_id,task_id,review_round,prior_seat_count,seat_count,reason,actor_id,created_at
		FROM review_seat_additions WHERE workspace_id=$1 AND request_id=$2`, workspaceID, requestID).
		Scan(&row.RequestID, &row.TaskID, &row.ReviewRound, &row.PriorSeatCount, &row.SeatCount, &row.Reason, &row.ActorID, &row.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
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
func insertReviewSeatAdditionTx(ctx context.Context, q boundaryQuerier, workspaceID string, row store.ReviewSeatAddition) error {
	_, err := q.Exec(ctx, `INSERT INTO review_seat_additions (workspace_id,request_id,task_id,review_round,prior_seat_count,seat_count,reason,actor_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, workspaceID, row.RequestID, row.TaskID, row.ReviewRound, row.PriorSeatCount, row.SeatCount, row.Reason, row.ActorID, row.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errReviewSeatAdditionCollision
	}
	return err
}

func taskReviewOrdersTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) ([]core.WorkOrder, error) {
	rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review' ORDER BY review_round, review_seat", workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.WorkOrder
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, order)
	}
	return result, rows.Err()
}

// insertReviewSeatAdditionOrderTx inserts one queued, content-free seat order
// with the same column set CreateReviewRoundCommand writes.
func insertReviewSeatAdditionOrderTx(ctx context.Context, tx pgx.Tx, workspaceID string, order core.WorkOrder) error {
	_, err := tx.Exec(ctx, `INSERT INTO work_orders (
		id, workspace_id, task_id, job_id, stage, state, claimant_id,
		session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
		review_round, review_seat, required_model, required_harness, required_harness_config, execution_timeout, model_enforcement,
		reason_code, review_kind, review_scope, baseline_sha, head_sha,
		queue_entered_at, queue_deadline, execution_started_at, execution_deadline,
		last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
		automatic_retry_count, next_retry_at, retry_suppressed,
		redispatch_count, progress, cost_usd, tokens_in, tokens_out,
		usage_reported, self_reported, created_at, updated_at, served_requirement_snapshot, governance_snapshot
	) VALUES ($1,$2,$3,$4,$5,$6,'','','','','','',NULL,$7,$8,$9,$10,$11,$12,'',$13,$14,$15,$16,$17,$18,$19,NULL,NULL,'','',NULL,NULL,0,NULL,false,0,'',0,0,0,false,false,$20,$20,$21,$22)`,
		order.ID, workspaceID, order.TaskID, order.JobID, core.StageReview, order.State,
		order.ReviewRound, order.ReviewSeat, order.RequiredModel, order.RequiredHarness,
		harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText,
		order.ReasonCode, order.ReviewKind, order.ReviewScope, order.BaselineSHA, order.HeadSHA,
		order.QueueEnteredAt, order.QueueDeadline, order.CreatedAt, servedRequirementSnapshotJSON(order.ServedRequirementSnapshot), governanceSnapshotJSON(order.GovernanceSnapshot))
	return err
}
