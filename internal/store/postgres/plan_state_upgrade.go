package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// planStateMigrationVersion is the migration that adds tasks.plan_state and
// the plan-version origin and base commit (component-plan-state invariant 8).
const planStateMigrationVersion = 161

// fillTaskPlanStates fills each task's plan state once, inside migration
// 161's transaction, by component-plan-state invariant 8's order:
// awaiting_approval at the plan_approval gate, else completed with an approved
// plan version, else needed for spec_approval true at policy version 1 or L2
// and L3 at policy version 0, else not_required. It writes no event, and no
// routing reads the level afterwards.
func fillTaskPlanStates(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE tasks t SET plan_state = CASE
	WHEN t.gate_kind = 'plan_approval' THEN 'awaiting_approval'
	WHEN EXISTS (SELECT 1 FROM task_specs s WHERE s.task_id = t.id AND s.approved) THEN 'completed'
	WHEN (t.policy_version > 0 AND t.spec_approval) OR (t.policy_version = 0 AND t.escalation_level IN ('L2','L3')) THEN 'needed'
	ELSE 'not_required'
END`)
	return err
}

// writeTaskPlanState stores a new task's plan state in its creating
// transaction. InsertTask keeps the column set every migrated schema shares,
// as the start-over columns do, so the plan state is its own statement
// (component-plan-state invariant 1).
func writeTaskPlanState(ctx context.Context, tx pgx.Tx, task core.Task) error {
	_, err := tx.Exec(ctx, `UPDATE tasks SET plan_state=$3 WHERE workspace_id=$1 AND id=$2`, task.Workspace, task.ID, string(task.PlanState))
	return err
}
