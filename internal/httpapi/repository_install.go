package httpapi

import (
	"context"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The first API configuration write after conveyor init files its install task.
// Registration never writes to a checkout (req-repository-onboarding REQ-3; DEC-32).
func (s *Server) fileRepositoryInstallTasks(ctx context.Context, repos []config.Repo) error {
	workspace, _ := store.WorkspaceFromContext(ctx)
	for _, repo := range repos {
		if !repo.InstallEnabled() {
			continue
		}
		attempts, err := s.Store.ListRepositoryInstallTasks(ctx, repo.Name)
		if err != nil {
			return err
		}
		next := 1
		reuse := false
		for _, attempt := range attempts {
			if attempt.State != core.TaskClosed {
				reuse = true
			}
			if attempt.Attempt >= next {
				next = attempt.Attempt + 1
			}
		}
		if reuse {
			continue
		}
		body := fmt.Sprintf("Prepare repository `%s` on base branch `%s` for Conveyor. In the task worktree, run `conveyor repo init`, commit, push, and submit for review.", repo.Name, repo.Base)
		_, err = s.createTaskRecord(ctx, createTaskReq{Body: body, Repo: repo.Name, BaseBranch: repo.Base, Source: core.RepositoryRegistrationSource, repositoryInstallAttempt: next}, store.RepositoryInstallKey(workspace, repo.Name, next), core.RepositoryRegistrationSource)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) attachRepositoryInstallTasks(ctx context.Context, repos []config.Repo) error {
	for i := range repos {
		repos[i].InstallTask = nil
		tasks, err := s.Store.ListRepositoryInstallTasks(ctx, repos[i].Name)
		if err != nil {
			return err
		}
		if len(tasks) > 0 {
			task := tasks[len(tasks)-1]
			repos[i].InstallTask = &config.InstallTask{ID: task.TaskID, State: string(task.State)}
		}
	}
	return nil
}
