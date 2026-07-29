package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestChangeTaskSetupCommandRejectsUnleasedMutation(t *testing.T) {
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemory()
	prior := config.ExecutionSetup{Name: "prior"}
	task := core.Task{
		ID: "setup-command-lease", Workspace: "demo", Repo: "app",
		State: core.TaskRunning, SetupName: prior.Name, SetupContract: prior,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	request := SetupChangeRequest{
		TaskID: task.ID, RequestID: "setup-command-request",
		Reason: "verify capability boundary", Setup: config.ExecutionSetup{Name: "next"},
	}
	if _, err := st.ChangeTaskSetupCommand(ctx, taskops.TaskLease{}, request); err == nil || !strings.Contains(err.Error(), "lease does not authorize") {
		t.Fatalf("unleased setup change err=%v", err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.SetupName != prior.Name {
		t.Fatalf("unleased setup change mutated task=%+v", current)
	}
}
