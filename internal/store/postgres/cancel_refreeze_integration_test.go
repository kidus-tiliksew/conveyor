package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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
	if err = migrateControlPlaneToVersion(store.WithActor(t.Context(), store.SystemActor()), pool, 35); err != nil {
		t.Fatalf("migrate isolated schema to version 35: %v", err)
	}
	var beforeName, beforeChecksum string
	var beforeVersion int
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), "SELECT max(version) FROM conveyor_schema_migrations").Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	if beforeVersion != 35 {
		t.Fatalf("pre-upgrade migration version=%d", beforeVersion)
	}
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), "SELECT name,checksum FROM conveyor_schema_migrations WHERE version=1").Scan(&beforeName, &beforeChecksum); err != nil {
		t.Fatal(err)
	}

	workspace := "migration-v35-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	st := newStore(pool)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "repo", Branch: "conveyor/task-" + taskID, State: core.TaskAwaiting, CreatedAt: time.Now().UTC()}
	if _, err = pool.Exec(ctx, `INSERT INTO tasks
		(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,parent_task_id,created_at,mode,spec_approval,merge_approval)
		VALUES ($1,$2,'test','migration task','','','L2','repo','main',$3,$4,'',$5,'',true,true)`,
		task.ID, workspace, task.Branch, task.State, task.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `
INSERT INTO interventions (task_id, actor_id, actor_role, action, reason_code)
VALUES ($1, 'existing-version-35-row', 'human', 'approve', 'pre-upgrade')`,
		task.ID); err != nil {
		t.Fatal(err)
	}

	if err = migrateControlPlane(store.WithActor(t.Context(), store.SystemActor()), pool); err != nil {
		t.Fatalf("upgrade isolated version-35 schema: %v", err)
	}
	var afterName, afterChecksum string
	var afterVersion int
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), "SELECT max(version) FROM conveyor_schema_migrations").Scan(&afterVersion); err != nil {
		t.Fatal(err)
	}
	if afterVersion != embeddedMigrationHead(t) {
		t.Fatalf("post-upgrade migration version=%d, want the embedded head %d",
			afterVersion, embeddedMigrationHead(t))
	}
	var leakedTempTables int
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), `SELECT count(*) FROM pg_catalog.pg_class
		WHERE relpersistence='t' AND relname IN (
			'migration_050_invalid_observation_requirements',
			'migration_050_invalid_drift_requirements'
		)`).Scan(&leakedTempTables); err != nil {
		t.Fatal(err)
	}
	if leakedTempTables != 0 {
		t.Fatalf("migration 050 temporary tables remain=%d", leakedTempTables)
	}
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), "SELECT name,checksum FROM conveyor_schema_migrations WHERE version=1").Scan(&afterName, &afterChecksum); err != nil {
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

func TestWorkOrderAttemptMigrationUpgradeKeepsVersion48LedgerIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = admin.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	schema := "migration_v48_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
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
	if err = migrateControlPlaneToVersion(store.WithActor(t.Context(), store.SystemActor()), pool, 48); err != nil {
		t.Fatalf("migrate isolated schema to version 48: %v", err)
	}
	var beforeName, beforeChecksum string
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), "SELECT name,checksum FROM conveyor_schema_migrations WHERE version=48").Scan(&beforeName, &beforeChecksum); err != nil {
		t.Fatal(err)
	}
	if beforeName != "048_planning_exploration.sql" || beforeChecksum == "" {
		t.Fatalf("version 48 ledger=(%q,%q)", beforeName, beforeChecksum)
	}
	var attemptColumns, planningColumns int
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='work_orders'
  AND column_name IN ('attempt_id','last_attempt_id','last_failure_category')`).Scan(&attemptColumns); err != nil {
		t.Fatal(err)
	}
	if attemptColumns != 0 {
		t.Fatalf("version 48 attempt columns=%d", attemptColumns)
	}
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='planning_sessions'
  AND column_name IN ('model','effort','exploration_output_tokens','exploration_tokens_used','primary_repo','pinned_revisions')`).Scan(&planningColumns); err != nil {
		t.Fatal(err)
	}
	if planningColumns != 6 {
		t.Fatalf("version 48 planning columns=%d", planningColumns)
	}

	if err = migrateControlPlane(store.WithActor(t.Context(), store.SystemActor()), pool); err != nil {
		t.Fatalf("upgrade isolated version-48 schema: %v", err)
	}
	var afterName, afterChecksum string
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), "SELECT name,checksum FROM conveyor_schema_migrations WHERE version=48").Scan(&afterName, &afterChecksum); err != nil {
		t.Fatal(err)
	}
	if afterName != beforeName || afterChecksum != beforeChecksum {
		t.Fatalf("version 48 migration changed: before=(%q,%q) after=(%q,%q)", beforeName, beforeChecksum, afterName, afterChecksum)
	}
	var afterVersion int
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), "SELECT max(version) FROM conveyor_schema_migrations").Scan(&afterVersion); err != nil {
		t.Fatal(err)
	}
	if afterVersion != embeddedMigrationHead(t) {
		t.Fatalf("post-upgrade migration version=%d, want the embedded head %d",
			afterVersion, embeddedMigrationHead(t))
	}
	var attemptMigrationName string
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), "SELECT name FROM conveyor_schema_migrations WHERE version=49").Scan(&attemptMigrationName); err != nil {
		t.Fatal(err)
	}
	if attemptMigrationName != "049_work_order_attempts.sql" {
		t.Fatalf("version 49 migration=%q", attemptMigrationName)
	}
	if err = pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='work_orders'
  AND column_name IN ('attempt_id','last_attempt_id','last_failure_category')`).Scan(&attemptColumns); err != nil {
		t.Fatal(err)
	}
	if attemptColumns != 3 {
		t.Fatalf("version 49 attempt columns=%d", attemptColumns)
	}
}

func TestMigratedSchemaAcceptsCanonicalInterventionActionsIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "intervention-actions-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
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

// embeddedMigrationHead is the highest version the binary carries. Deriving it
// keeps "the upgrade path reaches head" honest as migrations are added, rather
// than restating a number that goes stale on the next one.
func embeddedMigrationHead(t *testing.T) int {
	t.Helper()
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	head := 0
	for _, name := range files {
		version, versionErr := migrationVersion(name)
		if versionErr != nil {
			t.Fatalf("migration %s: %v", name, versionErr)
		}
		head = max(head, version)
	}
	if head == 0 {
		t.Fatal("no embedded control-plane migrations found")
	}
	return head
}

// taskOperationLockFixture is a running task on a store whose advisory-lock
// waiters the interleave tests can observe.
func taskOperationLockFixture(t *testing.T) (context.Context, *Store, string) {
	t.Helper()
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	workspace := "task-operation-lock-" + core.NewTaskID()
	ctx, cancel := context.WithTimeout(store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: store.UserActorID("operator"), Role: core.ActorUser}), workspace), time.Minute)
	t.Cleanup(cancel)
	if seeded, seedErr := st.BootstrapWorkspaceConfig(ctx, isolationConfig(workspace)); seedErr != nil || !seeded {
		t.Fatalf("workspace seeded=%t err=%v", seeded, seedErr)
	}
	taskID := core.NewTaskID()
	if err = st.CreateTask(ctx, core.Task{
		ID: taskID, Workspace: workspace, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + taskID,
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return ctx, st, taskID
}

// awaitTaskOperationWaiter returns once PostgreSQL reports a session waiting on
// exactly this task's task-operation lock, read from pg_locks and not inferred
// from elapsed time. It fails when the waiting command finishes first, because
// that command took no such lock.
func awaitTaskOperationWaiter[T any](t *testing.T, ctx context.Context, st *Store, workspace, taskID string, done <-chan T) {
	t.Helper()
	key := "conveyor:task-operation:" + workspace + ":" + taskID
	for {
		select {
		case result := <-done:
			t.Fatalf("command finished without waiting on the task-operation lock: %+v", result)
		case <-ctx.Done():
			t.Fatalf("no waiter on the task-operation lock: %v", ctx.Err())
		default:
		}
		var waiting bool
		if err := st.pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype='advisory' AND NOT granted AND objsubid=1
				AND classid::bigint=((hashtext($1)::bigint>>32)&4294967295)
				AND objid::bigint=(hashtext($1)::bigint&4294967295))`, key).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
	}
}

type taskCommandOutcome struct {
	task core.Task
	err  error
}

// A lifecycle command that read `running` holds the task-operation lock until it
// commits, so a cancel arriving in that window waits and then closes the state
// the command wrote. The closed state is the last write, and no live task
// survives with cancelled orders (DEC-50; component-task-lifecycle invariant 8;
// req-task-lifecycle-and-queue REQ-8).
func TestCancelWaitsForCommandThatReadRunningIntegration(t *testing.T) {
	ctx, st, taskID := taskOperationLockFixture(t)
	workspaceID, _ := store.WorkspaceFromContext(ctx)
	read, release := make(chan core.TaskState, 1), make(chan struct{})
	var releaseOnce sync.Once
	releaseCommand := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseCommand)
	commandCtx := store.WithTaskCommandReadHook(ctx, func(_ string, state core.TaskState) {
		read <- state
		<-release
	})
	commandDone := make(chan taskCommandOutcome, 1)
	go func() {
		outcome, err := taskops.New(st).Perform(commandCtx, taskID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
		commandDone <- taskCommandOutcome{outcome.Task, err}
	}()
	select {
	case state := <-read:
		if state != core.TaskRunning {
			t.Fatalf("command read state %s, want running", state)
		}
	case <-ctx.Done():
		t.Fatalf("command never read the task: %v", ctx.Err())
	}

	cancelDone := make(chan taskCommandOutcome, 1)
	go func() {
		task, err := taskops.New(st).Cancel(ctx, core.Intervention{TaskID: taskID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
		cancelDone <- taskCommandOutcome{task, err}
	}()
	awaitTaskOperationWaiter(t, ctx, st, workspaceID, taskID, cancelDone)
	releaseCommand()

	if outcome := <-commandDone; outcome.err != nil || outcome.task.State != core.TaskAwaiting {
		t.Fatalf("command outcome=%+v", outcome)
	}
	if outcome := <-cancelDone; outcome.err != nil || outcome.task.State != core.TaskClosed {
		t.Fatalf("cancel outcome=%+v", outcome)
	}
	final, err := st.GetTask(ctx, taskID)
	if err != nil || final.State != core.TaskClosed {
		t.Fatalf("final task=%+v err=%v", final, err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, event := range events {
		if event.Kind != "task.state_changed" {
			continue
		}
		var payload struct {
			Command string `json:"command"`
			From    string `json:"from"`
		}
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, payload.Command+" from "+payload.From)
	}
	want := []string{string(core.TaskGateMerge) + " from running", string(core.TaskCancel) + " from " + string(core.TaskAwaiting)}
	if !slices.Equal(commands, want) {
		t.Fatalf("state changes=%v, want %v", commands, want)
	}
}

// The state guard holds even against a writer outside the task-operation lock.
// A command that read `running` finds `closed` at write time, refuses with the
// typed transition error, and commits none of its writes (DEC-50;
// req-task-lifecycle-and-queue AC-1.1, AC-1.2).
func TestCommandThatReadRunningCannotOverwriteClosedIntegration(t *testing.T) {
	for _, test := range []struct {
		name    string
		command taskops.Command
	}{
		{name: "state update", command: taskops.Command{Kind: core.TaskGateMerge}},
		{name: "transition update", command: taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, st, taskID := taskOperationLockFixture(t)
			workspaceID, _ := store.WorkspaceFromContext(ctx)
			var observed core.TaskState
			commandCtx := store.WithTaskCommandReadHook(ctx, func(_ string, state core.TaskState) {
				observed = state
				// The pool's other connection commits while the command's
				// transaction sits between its read and its write.
				if _, err := st.pool.Exec(ctx, `UPDATE tasks SET state='closed',gate_kind='',next_stage='',recovery_stage='' WHERE workspace_id=$1 AND id=$2`, workspaceID, taskID); err != nil {
					t.Errorf("close the task inside the window: %v", err)
				}
			})
			_, err := taskops.New(st).Perform(commandCtx, taskID, test.command)
			if observed != core.TaskRunning {
				t.Fatalf("command read state %q, want running", observed)
			}
			var invalid *core.ErrInvalidTransition
			if !errors.As(err, &invalid) || invalid.From != string(core.TaskClosed) || invalid.Command != string(core.TaskGateMerge) {
				t.Fatalf("command err=%v, want an invalid transition from closed", err)
			}
			final, err := st.GetTask(ctx, taskID)
			if err != nil || final.State != core.TaskClosed {
				t.Fatalf("final task=%+v err=%v", final, err)
			}
			changes, err := st.CountEvents(ctx, taskID, "task.state_changed")
			if err != nil || changes != 0 {
				t.Fatalf("the refused command recorded %d state changes, err=%v", changes, err)
			}
		})
	}
}

// CreateConflictFixCommand holds the same lock as every other lifecycle command.
// hashtextextended of the same key would be a different lock, and this command
// would not wait here (DEC-50; component-persistence invariant 7).
func TestConflictFixTakesTheTaskOperationLockIntegration(t *testing.T) {
	ctx, st, taskID := taskOperationLockFixture(t)
	workspaceID, _ := store.WorkspaceFromContext(ctx)
	holder, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if err = lockTaskOperationTx(ctx, holder, workspaceID, taskID); err != nil {
		t.Fatal(err)
	}
	orderID := taskID + "-implement-conflict-1"
	job := core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, ReasonCode: "merge-conflict"}
	intervention := core.Intervention{TaskID: taskID, Action: core.InterventionRedirect, ReasonCode: "merge-conflict"}
	done := make(chan taskCommandOutcome, 1)
	go func() {
		_, commandErr := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ConflictFixResult, error) {
			return st.CreateConflictFixCommand(ctx, lease, store.ConflictFixRequest{TaskID: taskID, Job: job, WorkOrder: order, Intervention: intervention})
		})
		done <- taskCommandOutcome{err: commandErr}
	}()
	awaitTaskOperationWaiter(t, ctx, st, workspaceID, taskID, done)
	if err = holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Past the lock the command runs its ordinary legality checks; this test
	// only asserts that it waited, so it drains the result.
	<-done
}
