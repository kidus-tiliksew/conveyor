package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Plan risk orders on SingleStore (feature-risk-based-review RR-13;
// req-risk-based-review AC-4.7 to AC-4.9). Each helper runs inside the
// caller's taskTx, which holds the task operation lock, so a plan version, its
// plan-state move, and its risk order commit together.

// planApprovalRiskRefusalTx applies store.PlanApprovalRiskRefusal to the
// approval of version inside taskTx. Under record or off it reads no risk
// order (component-risk-review invariant 13).
func planApprovalRiskRefusalTx(ctx context.Context, tx *sql.Tx, taskID string, version int) error {
	task, err := getTaskRow(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if !task.SetupContract.Risk.Enforcing() {
		return nil
	}
	orders, err := taskRiskOrdersTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	return store.PlanApprovalRiskRefusal(task, orders, version)
}

// applyPlanRiskMoveTx applies store.PlanRiskMoveFor to a plan-state move from
// the state from that the caller's taskTx has just written.
func (s *Store) applyPlanRiskMoveTx(ctx context.Context, tx *sql.Tx, taskID string, from core.PlanState, cause core.PlanStateCause, at time.Time) error {
	task, err := getTaskRow(ctx, tx, taskID)
	if err != nil {
		return err
	}
	move := store.PlanRiskMoveFor(from, task.PlanState, cause)
	if !move.Create && move.Cancel == "" {
		return nil
	}
	latest := 0
	if move.Create {
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM task_specs WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), taskID).Scan(&latest); err != nil {
			return err
		}
	}
	if move.Cancel != "" {
		orders, listErr := taskRiskOrdersTx(ctx, tx, taskID)
		if listErr != nil {
			return listErr
		}
		for _, id := range store.PlanRiskCancelIDs(orders, latest) {
			if _, err = cancelRiskOrdersTx(ctx, tx, taskID, id, move.Cancel, at); err != nil {
				return err
			}
		}
	}
	if !move.Create {
		return nil
	}
	counted, err := lockRiskRatingsTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if request, ok := store.PlanRiskOrderRequest(task, counted, latest, at); ok {
		_, _, err = s.createRiskOrderTx(ctx, tx, task, request)
	}
	return err
}

// insertSpecVersionTx records spec as the task's next plan version with its
// spec.version_created event inside taskTx.
func insertSpecVersionTx(ctx context.Context, tx *sql.Tx, spec core.SpecVersion) (core.SpecVersion, error) {
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	if err := store.PrepareSpecVersionOrigin(&spec); err != nil {
		return core.SpecVersion{}, err
	}
	if _, err := getTaskRow(ctx, tx, spec.TaskID); err != nil {
		return core.SpecVersion{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM task_specs WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), spec.TaskID).Scan(&spec.Version); err != nil {
		return core.SpecVersion{}, err
	}
	if len(spec.Acceptance) == 0 {
		spec.Acceptance = json.RawMessage(`[]`)
	}
	if len(spec.Decomposition) == 0 {
		spec.Decomposition = json.RawMessage(`[]`)
	}
	spec.Approved = false
	spec.ApprovedAt = time.Time{}
	if _, err := writeRow(ctx, tx, rowWrite{table: "task_specs", operation: "INSERT", values: specRowValues(ctx, spec)}); err != nil {
		return core.SpecVersion{}, err
	}
	return spec, taskEvent(ctx, tx, core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount})})
}
