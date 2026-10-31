package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RecordWorkOrderClaimFailure upserts the order's claim-failure record in one
// statement outside the failed claim transaction (component-work-orders,
// Claim admission; component-persistence, Schema domains).
func (s *Store) RecordWorkOrderClaimFailure(ctx context.Context, workOrderID, errorClass string, at time.Time) (core.WorkOrderClaimFailure, error) {
	if err := store.ValidateWorkOrderClaimFailure(workOrderID, errorClass, at); err != nil {
		return core.WorkOrderClaimFailure{}, err
	}
	record := core.WorkOrderClaimFailure{WorkOrderID: workOrderID}
	err := s.boundary.QueryRow(ctx, `
		INSERT INTO work_order_claim_failures (workspace_id, work_order_id, consecutive_failures, first_failed_at, last_failed_at, error_class)
		SELECT w.workspace_id, w.id, 1, $3, $3, $4
		FROM work_orders w
		WHERE w.workspace_id=$1 AND w.id=$2
		ON CONFLICT (workspace_id, work_order_id) DO UPDATE
		SET consecutive_failures=LEAST(work_order_claim_failures.consecutive_failures + 1, $5),
		    last_failed_at=EXCLUDED.last_failed_at, error_class=EXCLUDED.error_class
		RETURNING consecutive_failures, first_failed_at, last_failed_at, error_class`,
		workspace(ctx), workOrderID, at.UTC(), errorClass, core.WorkOrderClaimFailureCountCap).
		Scan(&record.ConsecutiveFailures, &record.FirstFailedAt, &record.LastFailedAt, &record.ErrorClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrderClaimFailure{}, fmt.Errorf("%w: work order %s", store.ErrNotFound, workOrderID)
	}
	if err != nil {
		return core.WorkOrderClaimFailure{}, err
	}
	record.FirstFailedAt, record.LastFailedAt = record.FirstFailedAt.UTC(), record.LastFailedAt.UTC()
	return record, nil
}

// ListWorkOrderClaimFailures reads the given orders' records in one
// workspace-scoped query through the primary key.
func (s *Store) ListWorkOrderClaimFailures(ctx context.Context, workOrderIDs []string) ([]core.WorkOrderClaimFailure, error) {
	out := []core.WorkOrderClaimFailure{}
	if len(workOrderIDs) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT work_order_id, consecutive_failures, first_failed_at, last_failed_at, error_class
		FROM work_order_claim_failures
		WHERE workspace_id=$1 AND work_order_id = ANY($2)
		ORDER BY work_order_id`, workspace(ctx), workOrderIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var record core.WorkOrderClaimFailure
		if err = rows.Scan(&record.WorkOrderID, &record.ConsecutiveFailures, &record.FirstFailedAt, &record.LastFailedAt, &record.ErrorClass); err != nil {
			return nil, err
		}
		record.FirstFailedAt, record.LastFailedAt = record.FirstFailedAt.UTC(), record.LastFailedAt.UTC()
		out = append(out, record)
	}
	return out, rows.Err()
}
