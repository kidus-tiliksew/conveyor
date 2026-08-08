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
	created := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	for _, task := range []struct {
		id string
		at time.Time
	}{
		{id: "ops-oldest", at: created.Add(-time.Hour)},
		{id: "ops-zeta", at: created},
		{id: "ops-alpha", at: created},
		{id: "ops-newest", at: created.Add(time.Hour)},
	} {
		if err := st.CreateTask(ctx, core.Task{
			ID: task.id, Workspace: workspace, Title: task.id, Repo: "conveyor",
			BaseBranch: "main", Branch: "conveyor/task-" + task.id,
			State: core.TaskQueued, NextStage: core.StageImplement,
			CreatedAt: task.at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	storetest.RunTaskOperationsPaginationConformance(t, storetest.TaskOperationsFixture{
		Store: st, Context: ctx, WantTotal: 4,
		WantOrder: []string{"ops-newest", "ops-alpha", "ops-zeta", "ops-oldest"},
		Filter:    store.TaskFilter{Repositories: []string{"conveyor"}},
	})
}

func TestMemoryTaskFilterConformance(t *testing.T) {
	t.Parallel()
	workspace := "task-filter-" + core.NewTaskID()
	st := store.NewMemoryWithConfig(&config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	fixture := storetest.TaskFilterFixture{
		Store: st, Context: store.WithWorkspace(t.Context(), workspace),
		Workspace: workspace, Repo: "conveyor", Suffix: core.NewTaskID(),
	}
	storetest.SeedTaskFilterFixture(t, fixture)
	storetest.RunTaskFilterConformance(t, fixture)
}
