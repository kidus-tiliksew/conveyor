package singlestore

import (
	"context"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

func (s *Store) ListRequirementFollowUps(ctx context.Context, keys []string) (map[string]store.RequirementDetailTask, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementFollowUps")
	out := make(map[string]store.RequirementDetailTask)
	keys = store.UniqueTaskIDs(keys)
	for start := 0; start < len(keys); start += 100 {
		chunk := keys[start:min(start+100, len(keys))]
		clauses := make([]string, 0, len(chunk))
		args := []any{documentWorkspace(ctx)}
		for _, key := range chunk {
			filter, filterArgs := s.hotReadLookup("tasks_workspace_intake_lookup_idx", "task_intake_key", "BINARY intake_key=BINARY ?", key)
			clauses = append(clauses, "("+filter+")")
			args = append(args, filterArgs...)
		}
		rows, err := documentRows(ctx, s.db, `SELECT intake_key,id,title,state FROM tasks WHERE workspace_id=? AND (`+strings.Join(clauses, " OR ")+`) ORDER BY id`, args...)
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
