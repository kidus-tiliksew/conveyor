package store

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestPageOperatorAttentionTotalIsNotCandidateCount(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	st := NewMemory()
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	awaiting := core.Task{ID: "awaiting", Workspace: "demo", Repo: "conveyor", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}
	running := core.Task{ID: "running", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	for _, task := range []core.Task{awaiting, running} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	page, err := PageOperatorAttention(ctx, st, []core.Task{awaiting, running}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Tasks) != 1 || page.Tasks[0].ID != awaiting.ID {
		t.Fatalf("page=%+v", page)
	}
}
