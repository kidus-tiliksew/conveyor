package postgres

import (
	"context"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

func (s *Store) ListReviewPolicyReconciliationTasks(ctx context.Context) ([]core.Task, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListReviewPolicyReconciliationTasks")
	rows, err := s.boundary.Query(ctx, `SELECT t.id,t.state,t.setup_contract FROM tasks t WHERE t.workspace_id=$1 AND (t.state NOT IN ('merged','closed') OR EXISTS (SELECT 1 FROM review_policy_statuses p WHERE p.workspace_id=t.workspace_id AND p.task_id=t.id AND p.state<>'published')) ORDER BY t.created_at,t.id`, workspace(ctx))
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
