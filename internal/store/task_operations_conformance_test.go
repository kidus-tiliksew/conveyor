package store_test

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryTaskOperationsPaginationConformance(t *testing.T) {
	t.Parallel()
	workspace := "task-ops-" + core.NewTaskID()
	st := store.NewMemoryWithConfig(&config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	ctx := store.WithWorkspace(t.Context(), workspace)
	for index, id := range []string{"ops-first", "ops-second"} {
		if err := st.CreateTask(ctx, core.Task{
			ID: id, Workspace: workspace, Title: id, Repo: "conveyor",
			BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskQueued, NextStage: core.StageImplement,
			CreatedAt: time.Date(2026, 8, 7, 10, index, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	storetest.RunTaskOperationsPaginationConformance(t, storetest.TaskOperationsFixture{
		Store: st, Context: ctx, WantTotal: 2,
	})
}
