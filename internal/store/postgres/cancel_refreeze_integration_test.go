package postgres

import (
	"context"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func TestInterventionActionMigrationUpgradesVersion35SchemaIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = admin.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	schema := "migration_v35_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = migrateControlPlaneToVersion(t.Context(), pool, 35); err != nil {
		t.Fatalf("migrate isolated schema to version 35: %v", err)
	}
	var beforeName, beforeChecksum string
	var beforeVersion int
	if err = pool.QueryRow(t.Context(), "SELECT max(version) FROM conveyor_schema_migrations").Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	if beforeVersion != 35 {
		t.Fatalf("pre-upgrade migration version=%d", beforeVersion)
	}
	if err = pool.QueryRow(t.Context(), "SELECT name,checksum FROM conveyor_schema_migrations WHERE version=1").Scan(&beforeName, &beforeChecksum); err != nil {
		t.Fatal(err)
	}

	workspace := "migration-v35-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	st := &Store{pool: pool, queries: db.New(pool)}
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "repo", Branch: "conveyor/task-" + taskID, State: core.TaskAwaiting, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `
INSERT INTO interventions (task_id, actor_id, actor_role, action, reason_code)
VALUES ($1, 'existing-version-35-row', 'human', 'approve', 'pre-upgrade')`,
		task.ID); err != nil {
		t.Fatal(err)
	}

	if err = migrateControlPlane(t.Context(), pool); err != nil {
		t.Fatalf("upgrade isolated version-35 schema: %v", err)
	}
	var afterName, afterChecksum string
	var afterVersion int
	if err = pool.QueryRow(t.Context(), "SELECT max(version) FROM conveyor_schema_migrations").Scan(&afterVersion); err != nil {
		t.Fatal(err)
	}
	if afterVersion != 39 {
		t.Fatalf("post-upgrade migration version=%d", afterVersion)
	}
	if err = pool.QueryRow(t.Context(), "SELECT name,checksum FROM conveyor_schema_migrations WHERE version=1").Scan(&afterName, &afterChecksum); err != nil {
		t.Fatal(err)
	}
	if afterName != beforeName || afterChecksum != beforeChecksum {
		t.Fatalf("historical migration changed: before=(%q,%q) after=(%q,%q)", beforeName, beforeChecksum, afterName, afterChecksum)
	}
	var existing int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM interventions WHERE task_id=$1 AND actor_id='existing-version-35-row' AND action='approve'", task.ID).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 1 {
		t.Fatalf("existing version-35 row count=%d", existing)
	}
	for _, action := range core.InterventionActions() {
		if _, err = pool.Exec(ctx, `
INSERT INTO interventions (task_id, actor_id, actor_role, action, reason_code)
VALUES ($1, $2, 'human', $3, 'post-upgrade')`,
			task.ID, "canonical-"+string(action), action); err != nil {
			t.Fatalf("insert canonical action %q after upgrade: %v", action, err)
		}
	}
	if _, err = pool.Exec(ctx, `
INSERT INTO interventions (task_id, actor_id, actor_role, action, reason_code)
VALUES ($1, 'unknown', 'human', 'not_canonical', 'post-upgrade')`,
		task.ID); err == nil {
		t.Fatal("upgraded constraint accepted a non-canonical action")
	}
}

func TestMigratedSchemaAcceptsCanonicalInterventionActionsIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "intervention-actions-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "repo", Branch: "conveyor/task-" + taskID, State: core.TaskAwaiting, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, action := range core.InterventionActions() {
		if _, err = st.pool.Exec(ctx, `
INSERT INTO interventions (task_id, actor_id, actor_role, action, reason_code)
VALUES ($1, $2, 'human', $3, 'constraint-coverage')`,
			task.ID, "actor-"+string(action), action); err != nil {
			t.Fatalf("insert canonical action %q: %v", action, err)
		}
	}
	if _, err = st.pool.Exec(ctx, `
INSERT INTO interventions (task_id, actor_id, actor_role, action, reason_code)
VALUES ($1, 'actor-unknown', 'human', 'not_canonical', 'constraint-coverage')`,
		task.ID); err == nil {
		t.Fatal("migrated schema accepted a non-canonical intervention action")
	}
}

func TestTaskCancellationAndRecoveryRefreezeIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "cancel-refreeze-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.Actor{ID: "operator", Role: core.ActorHuman}), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "old", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP}}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	cancelTaskID := core.NewTaskID()
	cancelTask := core.Task{ID: cancelTaskID, Workspace: workspace, Repo: "repo", Branch: "conveyor/task-" + cancelTaskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err = st.CreateTask(ctx, cancelTask); err != nil {
		t.Fatal(err)
	}
	cancelJob := core.Job{ID: cancelTask.ID + "-implement-1", TaskID: cancelTask.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, cancelJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: cancelJob.ID, TaskID: cancelTask.ID, JobID: cancelJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, cancelJob.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	orderIDs := []string{cancelJob.ID}
	for _, state := range []core.WorkOrderState{core.WorkOrderQueued, core.WorkOrderSubmitted, core.WorkOrderTimedOut, core.WorkOrderStale} {
		job := core.Job{ID: cancelTask.ID + "-" + string(state), TaskID: cancelTask.ID, Stage: core.StageReview, State: core.JobPending}
		if err = st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: job.ID, TaskID: cancelTask.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		switch state {
		case core.WorkOrderSubmitted:
			claimed, claimErr := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "submitted-session", ClientToken: "submitted-token", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour})
			if claimErr != nil {
				t.Fatal(claimErr)
			}
			claimed.State = core.WorkOrderSubmitted
			if err = storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitForReview); err != nil {
				t.Fatal(err)
			}
		case core.WorkOrderTimedOut:
			order.State = core.WorkOrderTimedOut
			if err = storetest.For(st).UpdateWorkOrder(ctx, order, core.WorkOrderCmdTimeout); err != nil {
				t.Fatal(err)
			}
		case core.WorkOrderStale:
			order.State = core.WorkOrderStale
			if err = storetest.For(st).UpdateWorkOrder(ctx, order, core.WorkOrderCmdMarkStale); err != nil {
				t.Fatal(err)
			}
		}
		orderIDs = append(orderIDs, job.ID)
	}
	closed, err := st.CancelTask(ctx, core.Intervention{TaskID: cancelTask.ID, JobID: cancelJob.ID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
	if err != nil || closed.State != core.TaskClosed {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	for _, orderID := range orderIDs {
		cancelled, getErr := st.GetWorkOrder(ctx, orderID)
		if getErr != nil || cancelled.State != core.WorkOrderCancelled {
			t.Fatalf("cancelled order %s=%+v err=%v", orderID, cancelled, getErr)
		}
		if orderID == cancelJob.ID && cancelled.SessionID != "session" {
			t.Fatalf("claimed cancellation lost session identity: %+v", cancelled)
		}
	}
	interventions, err := st.ListInterventions(ctx, cancelTask.ID)
	if err != nil || len(interventions) != 1 || interventions[0].Action != core.InterventionCancel {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
	for _, kind := range []string{"intervention.cancel", "task.cancelled"} {
		count, countErr := st.CountEvents(ctx, cancelTask.ID, kind)
		if countErr != nil || count != 1 {
			t.Fatalf("%s events=%d err=%v", kind, count, countErr)
		}
	}
	jobs, err := st.ListJobs(ctx, cancelTask.ID)
	if err != nil || len(jobs) != len(orderIDs) {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	for _, job := range jobs {
		if job.State != core.JobFailed || job.EndedAt.IsZero() {
			t.Fatalf("cancelled job not cleaned up: %+v", job)
		}
	}
	if _, err = storetest.For(st).RenewWorkerClaim(ctx, cancelJob.ID, "worker", "session", time.Minute); !errors.Is(err, store.ErrWorkOrderCancelled) {
		t.Fatalf("renew error=%v", err)
	}
	if _, err = st.CancelTask(ctx, core.Intervention{TaskID: cancelTask.ID, JobID: cancelJob.ID, Action: core.InterventionCancel, ReasonCode: "again"}); !errors.Is(err, store.ErrTaskTerminal) {
		t.Fatalf("repeated cancellation error=%v", err)
	}
	interventions, _ = st.ListInterventions(ctx, cancelTask.ID)
	cancelEvents, _ := st.CountEvents(ctx, cancelTask.ID, "intervention.cancel")
	if len(interventions) != 1 || cancelEvents != 1 {
		t.Fatalf("repeated cancellation partially mutated: interventions=%d events=%d", len(interventions), cancelEvents)
	}

	for _, state := range []core.TaskState{core.TaskQueued, core.TaskAwaiting, core.TaskParked} {
		taskID := core.NewTaskID()
		task := core.Task{ID: taskID, Workspace: workspace, Repo: "repo", Branch: "conveyor/task-" + taskID, State: state, CreatedAt: now}
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatalf("create %s task: %v", state, err)
		}
		got, cancelErr := st.CancelTask(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "state-coverage"})
		if cancelErr != nil || got.State != core.TaskClosed {
			t.Fatalf("cancel %s task=%+v err=%v", state, got, cancelErr)
		}
	}

	prior := config.ExecutionSetup{Name: "default", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "old", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "1h"}}}
	next := config.ExecutionSetup{Name: "default", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "new", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "2h"}}}
	recoverTaskID := core.NewTaskID()
	recoverTask := core.Task{ID: recoverTaskID, Workspace: workspace, Repo: "repo", Branch: "conveyor/task-" + recoverTaskID, State: core.TaskRunning, SetupName: "default", SetupContract: prior, CreatedAt: now}
	if err = st.CreateTask(ctx, recoverTask); err != nil {
		t.Fatal(err)
	}
	recoverJob := core.Job{ID: recoverTask.ID + "-implement-1", TaskID: recoverTask.ID, Stage: core.StageImplement, State: core.JobFailed}
	if err = st.CreateJob(ctx, recoverJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: recoverJob.ID, TaskID: recoverTask.ID, JobID: recoverJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredModel: "old", RequiredHarness: "codex", ExecutionTimeoutText: "1h", QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: now.Add(-time.Minute), LastAttemptOutcome: "child_failure", RetrySuppressed: true, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	change := &store.RecoveryRefreeze{Setup: next, RequiredModel: "new", RequiredHarness: "codex", RequiredEffort: "high", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "codex", Command: []string{"codex", "exec"}, Effort: "high", EffortArgv: []string{"--effort", "high"}}, ExecutionTimeoutText: "2h"}
	recovered, err := storetest.For(st).RecoverWorkOrder(ctx, recoverJob.ID, "recover-refreeze", time.Hour, change)
	if err != nil || recovered.State != core.WorkOrderQueued || recovered.RequiredModel != "new" || recovered.RequiredEffort != "high" {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	persisted, _ := st.GetTask(ctx, recoverTask.ID)
	count, _ := st.CountEvents(ctx, recoverTask.ID, "task.setup.refrozen")
	if persisted.SetupContract.ExecutionSettings.Implementation.Model != "new" || count != 1 {
		t.Fatalf("setup=%+v events=%d", persisted.SetupContract, count)
	}
}
