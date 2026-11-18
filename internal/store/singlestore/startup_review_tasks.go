package singlestore

import (
	"context"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

func (s *Store) ListReviewPolicyReconciliationTasks(ctx context.Context) ([]core.Task, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListReviewPolicyReconciliationTasks")
	terminalStates := []string{string(core.TaskMerged), string(core.TaskClosed)}
	open, openArgs := s.taskStatePredicate("t.state", terminalStates, true)
	terminal, terminalArgs := s.taskStatePredicate("t.state", terminalStates, false)
	args := []any{documentWorkspace(ctx)}
	args = append(args, openArgs...)
	args = append(args, documentWorkspace(ctx))
	args = append(args, terminalArgs...)
	// The branches are disjoint. Open tasks need reconciliation regardless of
	// status; terminal tasks only repair an outstanding publication.
	rows, err := documentRows(ctx, s.db, `SELECT id,state,setup_contract FROM (
		SELECT t.id,t.state,t.setup_contract,t.created_at FROM tasks t
		WHERE t.workspace_id=? AND `+open+`
		UNION ALL
		SELECT t.id,t.state,t.setup_contract,t.created_at FROM tasks t
		WHERE t.workspace_id=? AND `+terminal+`
		AND EXISTS (SELECT 1 FROM review_policy_statuses p
			WHERE p.workspace_id=t.workspace_id AND p.task_id=t.id AND p.state<>'published')
	) selected ORDER BY created_at,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Task{}
	for rows.Next() {
		var task core.Task
		var state string
		var contract []byte
		if err := rows.Scan(&task.ID, &state, &contract); err != nil {
			return nil, err
		}
		task.State = core.TaskState(state)
		if len(contract) > 0 {
			if err := json.Unmarshal(contract, &task.SetupContract); err != nil {
				return nil, err
			}
		}
		out = append(out, task)
	}
	return out, rows.Err()
}
