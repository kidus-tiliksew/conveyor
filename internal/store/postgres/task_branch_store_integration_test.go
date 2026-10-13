package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestAttachTaskBranchSingleConnectionPoolIntegration(t *testing.T) {
	base := conformanceStore(t)
	poolConfig := *base.pool.Config()
	poolConfig.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), &poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	t.Cleanup(st.Close)

	workspace := "attach-single-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
	if _, err := base.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	taskID := "task-" + core.NewTaskID()
	if err := base.CreateTask(ctx, core.Task{
		ID: taskID, Workspace: workspace, Repo: "conveyor", Title: "single connection",
		BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskQueued,
		NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("attach", func(t *testing.T) {
		deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		got, err := st.AttachTaskBranch(deadline, taskID, "feature/single")
		if err != nil {
			t.Fatal(err)
		}
		if got.Branch != "feature/single" {
			t.Fatalf("branch=%q", got.Branch)
		}
		events, err := st.ListEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		var attached int
		for _, event := range events {
			if event.Kind == "task.branch_attached" {
				attached++
			}
		}
		if attached != 1 {
			t.Fatalf("task.branch_attached events=%d, want 1", attached)
		}
	})

	t.Run("same branch", func(t *testing.T) {
		deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		before, err := st.ListEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.AttachTaskBranch(deadline, taskID, "feature/single")
		if err != nil {
			t.Fatal(err)
		}
		if got.Branch != "feature/single" {
			t.Fatalf("branch=%q", got.Branch)
		}
		after, err := st.ListEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Fatalf("events=%d, want unchanged at %d", len(after), len(before))
		}
	})

	t.Run("unknown task", func(t *testing.T) {
		deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		missingID := "missing-" + core.NewTaskID()
		_, err := st.AttachTaskBranch(deadline, missingID, "feature/missing")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("error=%v, want store.ErrNotFound", err)
		}
		if !strings.Contains(err.Error(), "task "+missingID) {
			t.Fatalf("error=%v, want task ID in not-found error", err)
		}
	})
}
