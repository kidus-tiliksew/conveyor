package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestRecoveryRejectsSupersededOrderAndAllowsLatestIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "recovery-supersession-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", Branch: "conveyor/task-" + core.NewTaskID(), BaseBranch: "main", State: core.TaskRunning, CreatedAt: now.Add(-time.Hour)}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	successorJob := core.Job{ID: task.ID + "-spec-2", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	targetJob := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	if err = st.CreateJob(ctx, successorJob); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, targetJob); err != nil {
		t.Fatal(err)
	}
	successor := core.WorkOrder{ID: successorJob.ID, TaskID: task.ID, JobID: successorJob.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, LastAttemptOutcome: core.WorkOrderOutcomeChildFailure, RetrySuppressed: true, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	target := core.WorkOrder{ID: targetJob.ID, TaskID: task.ID, JobID: targetJob.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, CreatedAt: now.Add(-time.Hour), QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: now.Add(-time.Minute)}
	if err = storetest.For(st).CreateWorkOrder(ctx, successor); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).RecoverWorkOrder(ctx, target.ID, "recover-superseded", time.Hour); err == nil || !strings.Contains(err.Error(), successor.ID) {
		t.Fatalf("superseded recovery err=%v", err)
	}
	if recovered, recoverErr := storetest.For(st).RecoverWorkOrder(ctx, successor.ID, "recover-latest", time.Hour); recoverErr != nil || recovered.State != core.WorkOrderQueued || !recovered.Claimable {
		t.Fatalf("latest recovered=%+v err=%v", recovered, recoverErr)
	}
}

func TestWorkOrderZombieBackfillMigrationRetiresPassedStageAndIsRerunSafeIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 98)
	workspace := "zombie-backfill-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: "260815-4804be", Workspace: workspace, Repo: "conveyor", Branch: "conveyor/task-260815-4804be-" + core.NewTaskID(), BaseBranch: "main", State: core.TaskAwaiting, RecoveryStage: core.StageImplement, CreatedAt: now}
	job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if err := migrateControlPlaneToVersion(t.Context(), st.pool, 101); err != nil {
		t.Fatalf("apply migration 101: %v", err)
	}
	var state core.WorkOrderState
	var retirementEvents int
	if err := st.pool.QueryRow(t.Context(), `SELECT state FROM work_orders WHERE workspace_id=$1 AND id=$2`, workspace, order.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE workspace_id=$1 AND task_id=$2 AND job_id=$3 AND kind='work_order.retired'`, workspace, task.ID, job.ID).Scan(&retirementEvents); err != nil {
		t.Fatal(err)
	}
	if state != core.WorkOrderCancelled || retirementEvents != 1 {
		t.Fatalf("backfill state=%s retirement_events=%d", state, retirementEvents)
	}
	raw, err := migrationFiles.ReadFile("migrations/101_work_order_zombie_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql, err := renderMigration(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(t.Context(), string(sql)); err != nil {
		t.Fatalf("rerun migration 101 projection: %v", err)
	}
	if err = st.pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE workspace_id=$1 AND task_id=$2 AND job_id=$3 AND kind='work_order.retired'`, workspace, task.ID, job.ID).Scan(&retirementEvents); err != nil || retirementEvents != 1 {
		t.Fatalf("retirement events after rerun=%d err=%v", retirementEvents, err)
	}
}
