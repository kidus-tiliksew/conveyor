package postgres

import (
	"context"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestCanonicalStateConstraintsRejectUnsupportedValuesIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	workspace := "state-constraints-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Model: "operator", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP},
		}},
	}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage}); err != nil {
		t.Fatal(err)
	}

	assertCheckViolation := func(query, constraint string, args ...any) {
		t.Helper()
		_, execErr := st.pool.Exec(ctx, query, args...)
		var pgErr *pgconn.PgError
		if !errors.As(execErr, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
			t.Fatalf("constraint %s error = %v", constraint, execErr)
		}
	}
	assertCheckViolation("UPDATE tasks SET state='unsupported' WHERE workspace_id=$1 AND id=$2", "tasks_state_check", workspace, task.ID)
	assertCheckViolation("UPDATE work_orders SET state='unsupported' WHERE workspace_id=$1 AND id=$2", "work_orders_state_check", workspace, job.ID)
}
