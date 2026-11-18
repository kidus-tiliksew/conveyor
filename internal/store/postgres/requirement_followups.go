package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

func (s *Store) ListRequirementFollowUps(ctx context.Context, keys []string) (map[string]store.RequirementDetailTask, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementFollowUps")
	out := make(map[string]store.RequirementDetailTask)
	keys = store.UniqueTaskIDs(keys)
	for start := 0; start < len(keys); start += 100 {
		rows, err := s.boundary.Query(ctx, `SELECT intake_key,id,title,state FROM tasks WHERE workspace_id=$1 AND intake_key=ANY($2::text[]) ORDER BY id`, workspace(ctx), keys[start:min(start+100, len(keys))])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, state string
			var task store.RequirementDetailTask
			if err := rows.Scan(&key, &task.ID, &task.Title, &state); err != nil {
				rows.Close()
				return nil, err
			}
			task.State = core.TaskState(state)
			// The smallest task ID in byte order wins for a key, as on every
			// backend, whatever the column collation orders first
			// (component-lineage invariant 30).
			if kept, ok := out[key]; !ok || task.ID < kept.ID {
				out[key] = task
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
