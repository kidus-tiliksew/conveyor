package taskops_test

import (
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"testing"
)

func TestTaskStartOverRequiresCompoundCommand(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{ID: "restart-command", Workspace: "demo", State: core.TaskRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartOverTaskCommand(ctx, taskops.TaskLease{}, core.TaskStartOverRequest{TaskID: task.ID, RequestID: "r", Reason: "try again"}); err == nil {
		t.Fatal("zero lease admitted")
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStartOver}); err == nil {
		t.Fatal("generic command bypassed compound operation")
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.State != task.State {
		t.Fatal("refusal changed state")
	}
}
