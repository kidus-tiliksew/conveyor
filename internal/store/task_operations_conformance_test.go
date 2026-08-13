package store_test

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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
	if err := store.SetMemoryWorkspaceMember(st, workspace, "usr-filter-assignee", true); err != nil {
		t.Fatal(err)
	}
	fixture := storetest.TaskFilterFixture{
		Store: st, Context: store.WithWorkspace(t.Context(), workspace),
		Workspace: workspace, Repo: "conveyor", Suffix: core.NewTaskID(),
		AssigneeUserID: "usr-filter-assignee",
		Assign: func(t *testing.T, taskID, userID string) {
			t.Helper()
			if _, err := taskops.New(st).SetAssignee(store.WithWorkspace(t.Context(), workspace), taskID, userID); err != nil {
				t.Fatal(err)
			}
		},
	}
	storetest.SeedTaskFilterFixture(t, fixture)
	storetest.RunTaskFilterConformance(t, fixture)
}

func TestMemoryTaskAssigneeMembershipConformance(t *testing.T) {
	t.Parallel()
	workspace := "task-assignee-" + core.NewTaskID()
	st := store.NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}})
	ctx := store.WithWorkspace(t.Context(), workspace)
	taskID := "task-" + core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMemoryWorkspaceMember(st, workspace, "usr-active", true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMemoryWorkspaceMember(st, workspace, "usr-inactive", false); err != nil {
		t.Fatal(err)
	}
	storetest.RunTaskAssigneeMembershipConformance(t, storetest.TaskAssigneeMembershipFixture{
		Store: st, Context: ctx, TaskID: taskID, ActiveUserID: "usr-active", InactiveUserID: "usr-inactive", NonMemberID: "usr-missing",
	})
}
