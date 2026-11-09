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
// 1 or L2 and L3 at policy version 0, else not_required.
func fillTaskPlanStates(ctx context.Context, s *Store) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks t
LEFT JOIN (SELECT workspace_id, task_id FROM task_specs WHERE approved GROUP BY workspace_id, task_id) approved
	ON approved.workspace_id = t.workspace_id AND approved.task_id = t.id
SET t.plan_state = CASE
	WHEN t.gate_kind = 'plan_approval' THEN 'awaiting_approval'
	WHEN approved.task_id IS NOT NULL THEN 'completed'
	WHEN (t.policy_version > 0 AND t.spec_approval) OR (t.policy_version = 0 AND t.escalation_level IN ('L2','L3')) THEN 'needed'
	ELSE 'not_required'
END
WHERE t.plan_state IS NULL`)
	return err
}
