package singlestore

import (
	"context"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (s *Store) ListRepositoryInstallTasks(ctx context.Context, repository string) ([]core.RepositoryInstallTask, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.task_id,i.attempt,t.state FROM repository_install_tasks i JOIN tasks t ON t.workspace_id=i.workspace_id AND t.id=i.task_id WHERE i.workspace_id=? AND i.repository_name=? ORDER BY i.attempt`, ws, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.RepositoryInstallTask
	for rows.Next() {
		var item core.RepositoryInstallTask
		if err := rows.Scan(&item.TaskID, &item.Attempt, &item.State); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
