package store_test

import (
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"testing"
	"time"
)

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
	if err := store.SetMemoryWorkspaceMemberRole(st, workspace, "usr-viewer", core.WorkspaceRoleViewer); err != nil {
		t.Fatal(err)
	}
	// Retain the backend-specific deactivated-account fixture check.
	if _, err := taskops.New(st).SetAssignee(ctx, taskID, "usr-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, taskID, "usr-inactive"); err == nil {
		t.Fatal("inactive account accepted as assignee")
	}
	if _, err := taskops.New(st).SetAssignee(ctx, taskID, "usr-viewer"); err == nil {
		t.Fatal("viewer accepted as assignee")
	}
}
