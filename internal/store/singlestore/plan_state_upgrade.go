package singlestore

import "context"

// planStateMigrationVersion is the migration that adds tasks.plan_state and
// the plan-version origin and base commit (component-plan-state invariant 8).
const planStateMigrationVersion = 35

// migrateTaskPlanState adds the plan-state columns behind information_schema
// checks and fills each task's plan state once. Existing plan versions take
// the plan_order origin from the column default and no base commit. The fill
// writes only rows whose plan state is still NULL, so a restart after an
// interrupted startup changes nothing already filled, and every writer since
// sets the column. It writes no event.
func (s *Store) migrateTaskPlanState(ctx context.Context) error {
	if err := s.addNullableColumns(ctx,
		nullableColumn{"tasks", "plan_state", "VARCHAR(32) NULL"},
		nullableColumn{"task_specs", "origin", "VARCHAR(32) NOT NULL DEFAULT 'plan_order'"},
		nullableColumn{"task_specs", "base_commit", "VARCHAR(40) NULL"},
	); err != nil {
		return err
	}
	return fillTaskPlanStates(ctx, s)
}

// fillTaskPlanStates applies component-plan-state invariant 8's order:
// awaiting_approval at the plan_approval gate, else completed with an
// approved plan version, else needed for spec_approval true at policy version
// 1 or L2 and L3 at policy version 0, else not_required. The approved-version
// test is a correlated subquery on the shared workspace shard key, the form
// migration 0019's fill uses.
func fillTaskPlanStates(ctx context.Context, s *Store) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks
SET plan_state = CASE
	WHEN gate_kind = 'plan_approval' THEN 'awaiting_approval'
	WHEN (SELECT COUNT(*) FROM task_specs AS s
		WHERE s.workspace_id = tasks.workspace_id AND s.task_id = tasks.id AND s.approved) > 0 THEN 'completed'
	WHEN (policy_version > 0 AND spec_approval) OR (policy_version = 0 AND escalation_level IN ('L2','L3')) THEN 'needed'
	ELSE 'not_required'
END
WHERE plan_state IS NULL`)
	return err
}
