package store

import (
	"context"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"sort"
)

// RepositoryInstallKey is workspace-scoped intake identity, not a title search.
func RepositoryInstallKey(workspace, repository string, attempt int) string {
	return fmt.Sprintf("repo-install-%s-%s-%d", workspace, repository, attempt)
}

func ValidateRepositoryInstallTask(t core.Task) error {
	if t.RepositoryInstallAttempt == 0 && t.Source != core.RepositoryRegistrationSource {
		return nil
	}
	if t.RepositoryInstallAttempt < 1 || t.Source != core.RepositoryRegistrationSource || t.IntakeKey != RepositoryInstallKey(t.Workspace, t.Repo, t.RepositoryInstallAttempt) {
		return fmt.Errorf("invalid repository registration provenance or install attempt")
	}
	return nil
}

func (m *memory) ListRepositoryInstallTasks(ctx context.Context, repository string) ([]core.RepositoryInstallTask, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws := workspaceOrDefault(ctx, "")
	result := append([]core.RepositoryInstallTask(nil), m.repositoryInstalls[memoryScopedKey{workspace: ws, id: repository}]...)
	for i := range result {
		result[i].State = m.tasks[result[i].TaskID].State
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Attempt < result[j].Attempt })
	return result, nil
}
