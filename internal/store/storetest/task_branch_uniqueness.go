package storetest

import (
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runTaskBranchUniqueness(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	open := newAggregateTask(t, x)

	collision := core.Task{
		ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "collision",
		BaseBranch: "main", Branch: open.Branch, State: core.TaskQueued, NextStage: core.StageImplement,
	}
	err := st.CreateTask(ctx, collision)
	if err == nil || !errors.Is(err, store.ErrTaskBranchConflict) || !strings.Contains(err.Error(), open.ID) {
		t.Fatalf("open-task collision: %v", err)
	}

	otherRepo := collision
	otherRepo.ID = core.NewTaskID()
	otherRepo.Repo = "app"
	requireOK(t, st.CreateTask(ctx, otherRepo))

	cancelled, err := taskops.New(st).Cancel(ctx, core.Intervention{
		TaskID: open.ID, Action: core.InterventionCancel, ReasonCode: "operator_cancel", Comment: "release branch",
	})
	requireOK(t, err)
	if cancelled.State != core.TaskClosed {
		t.Fatalf("cancelled state=%s", cancelled.State)
	}

	reuse := core.Task{
		ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "reuse",
		BaseBranch: "main", Branch: open.Branch, State: core.TaskQueued, NextStage: core.StageImplement,
	}
	requireOK(t, st.CreateTask(ctx, reuse))

	before, err := st.ListEvents(ctx, reuse.ID)
	requireOK(t, err)
	same, err := st.AttachTaskBranch(ctx, reuse.ID, reuse.Branch)
	requireOK(t, err)
	if same.Branch != reuse.Branch {
		t.Fatalf("same-name attach changed branch to %s", same.Branch)
	}
	after, err := st.ListEvents(ctx, reuse.ID)
	requireOK(t, err)
	if len(after) != len(before) {
		t.Fatal("same-name attach appended an event")
	}

	remapped, err := st.AttachTaskBranch(ctx, reuse.ID, "feature/demo")
	requireOK(t, err)
	if remapped.Branch != "feature/demo" {
		t.Fatalf("remap branch=%s", remapped.Branch)
	}
	events, err := st.ListEvents(ctx, reuse.ID)
	requireOK(t, err)
	if len(events) != len(after)+1 || events[len(events)-1].Kind != "task.branch_attached" {
		t.Fatal("remap lacks task.branch_attached")
	}

	sibling := newAggregateTask(t, x)
	_, err = st.AttachTaskBranch(ctx, sibling.ID, "feature/demo")
	if err == nil || !errors.Is(err, store.ErrTaskBranchConflict) || !strings.Contains(err.Error(), reuse.ID) {
		t.Fatalf("attach collision: %v", err)
	}

	_, err = st.AttachTaskBranch(ctx, cancelled.ID, "feature/other")
	if err == nil || !errors.Is(err, store.ErrTaskTerminal) {
		t.Fatalf("terminal attach: %v", err)
	}

	current, err := st.GetTask(ctx, reuse.ID)
	requireOK(t, err)
	if current.Branch != "feature/demo" {
		t.Fatalf("persisted branch=%s", current.Branch)
	}
}
