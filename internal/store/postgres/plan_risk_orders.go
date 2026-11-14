package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// Plan risk orders on PostgreSQL (feature-risk-based-review RR-13;
// req-risk-based-review AC-4.7 to AC-4.9). Each helper runs in the caller's
// transaction, so a plan version, its plan-state move, and its risk order
// commit together.

// planApprovalRiskRefusalTx applies store.PlanApprovalRiskRefusal to the
// approval of version in the caller's transaction. Under record or off it
// reads no risk order (component-risk-review invariant 13).
func planApprovalRiskRefusalTx(ctx context.Context, tx pgx.Tx, q *db.Queries, taskID string, version int) error {
	row, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return notFound(err, "task %s", taskID)
	}
	task := taskFromDB(row)
	if !task.SetupContract.Risk.Enforcing() {
		return nil
	}
	orders, err := taskRiskOrdersTx(ctx, tx, workspace(ctx), taskID)
	if err != nil {
		return err
	}
	return store.PlanApprovalRiskRefusal(task, orders, version)
}

// applyPlanRiskMoveTx applies store.PlanRiskMoveFor to a plan-state move from
// the state from that the caller's transaction has just written, or to a plan
// version it recorded. It takes
// lockWorkOrderTaskTx, which every caller already holds or may take after the
// task-operation lock, so a claim, a completion, and a cancellation of the
// task's risk orders serialize with the move.
func applyPlanRiskMoveTx(ctx context.Context, tx pgx.Tx, q *db.Queries, taskID string, from core.PlanState, cause core.PlanStateCause, recorded bool, at time.Time) error {
	row, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return notFound(err, "task %s", taskID)
	}
	task := taskFromDB(row)
	move := store.PlanRiskMoveFor(from, task.PlanState, cause, recorded)
	if !move.Create && move.Cancel == "" {
		return nil
	}
	if err = lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
		return err
	}
	latest := 0
	if move.Create {
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(s.version),0) FROM task_specs s JOIN tasks t ON t.id=s.task_id
			WHERE s.task_id=$1 AND t.workspace_id=$2`, taskID, workspace(ctx)).Scan(&latest); err != nil {
			return err
		}
	}
	if move.Cancel != "" {
		orders, listErr := taskRiskOrdersTx(ctx, tx, workspace(ctx), taskID)
		if listErr != nil {
			return listErr
		}
		for _, id := range store.PlanRiskCancelIDs(orders, latest) {
			if _, err = cancelRiskOrdersTx(ctx, tx, q, taskID, id, move.Cancel, at); err != nil {
				return err
			}
		}
	}
	if !move.Create {
		return nil
	}
	counted, err := listRiskRatings(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if request, ok := store.PlanRiskOrderRequest(task, counted, latest, at); ok {
		_, _, err = createRiskOrderTx(ctx, tx, q, task, request)
	}
	return err
}

// insertSpecVersionTx records spec as the task's next plan version with its
// spec.version_created event in the caller's transaction.
func insertSpecVersionTx(ctx context.Context, q *db.Queries, spec core.SpecVersion) (core.SpecVersion, error) {
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	if err := store.PrepareSpecVersionOrigin(&spec); err != nil {
		return core.SpecVersion{}, err
	}
	if _, err := q.GetTask(ctx, db.GetTaskParams{ID: spec.TaskID, WorkspaceID: workspace(ctx)}); err != nil {
		return core.SpecVersion{}, notFound(err, "task %s", spec.TaskID)
	}
	row, err := q.InsertSpecVersion(ctx, db.InsertSpecVersionParams{
		TaskID: spec.TaskID, Content: spec.Content, AcceptanceCount: int32(spec.AcceptanceCount),
		Acceptance: spec.Acceptance, Decomposition: spec.Decomposition, CreatedAt: timestamp(spec.CreatedAt), Agent: spec.Agent, Model: spec.Model,
		ReviewSeats: int4FromSeatCount(spec.ReviewSeats),
		Origin:      string(spec.Origin), BaseCommit: textFromPointer(spec.BaseCommit),
	})
	if err != nil {
		return core.SpecVersion{}, err
	}
	spec = specFromDB(row)
	return spec, insertEvent(ctx, q, core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount})})
}
