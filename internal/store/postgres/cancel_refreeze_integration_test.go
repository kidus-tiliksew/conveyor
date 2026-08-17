package postgres

import (
	"io/fs"
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

	if err = migrateControlPlane(t.Context(), pool); err != nil {
		t.Fatalf("upgrade isolated version-35 schema: %v", err)
	}
	var afterName, afterChecksum string
	var afterVersion int
	if err = pool.QueryRow(t.Context(), "SELECT max(version) FROM conveyor_schema_migrations").Scan(&afterVersion); err != nil {
		t.Fatal(err)
	}
	if afterVersion != embeddedMigrationHead(t) {
		t.Fatalf("post-upgrade migration version=%d, want the embedded head %d",
			afterVersion, embeddedMigrationHead(t))
	}
	var leakedTempTables int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_catalog.pg_class
		WHERE relpersistence='t' AND relname IN (
			'migration_050_invalid_observation_requirements',
			'migration_050_invalid_drift_requirements'
		)`).Scan(&leakedTempTables); err != nil {
		t.Fatal(err)
	}
	if leakedTempTables != 0 {
		t.Fatalf("migration 050 temporary tables remain=%d", leakedTempTables)
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
	if err = migrateControlPlaneToVersion(t.Context(), pool, 48); err != nil {
		t.Fatalf("migrate isolated schema to version 48: %v", err)
	}
	var beforeName, beforeChecksum string
	if err = pool.QueryRow(t.Context(), "SELECT name,checksum FROM conveyor_schema_migrations WHERE version=48").Scan(&beforeName, &beforeChecksum); err != nil {
		t.Fatal(err)
	}
	if beforeName != "048_planning_exploration.sql" || beforeChecksum == "" {
		t.Fatalf("version 48 ledger=(%q,%q)", beforeName, beforeChecksum)
	}
	var attemptColumns, planningColumns int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='work_orders'
  AND column_name IN ('attempt_id','last_attempt_id','last_failure_category')`).Scan(&attemptColumns); err != nil {
		t.Fatal(err)
	}
	if attemptColumns != 0 {
		t.Fatalf("version 48 attempt columns=%d", attemptColumns)
	}
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='planning_sessions'
  AND column_name IN ('model','effort','exploration_output_tokens','exploration_tokens_used','primary_repo','pinned_revisions')`).Scan(&planningColumns); err != nil {
		t.Fatal(err)
	}
	if planningColumns != 6 {
		t.Fatalf("version 48 planning columns=%d", planningColumns)
	}

	if err = migrateControlPlane(t.Context(), pool); err != nil {
		t.Fatalf("upgrade isolated version-48 schema: %v", err)
	}
	var afterName, afterChecksum string
	if err = pool.QueryRow(t.Context(), "SELECT name,checksum FROM conveyor_schema_migrations WHERE version=48").Scan(&afterName, &afterChecksum); err != nil {
		t.Fatal(err)
	}
	if afterName != beforeName || afterChecksum != beforeChecksum {
		t.Fatalf("version 48 migration changed: before=(%q,%q) after=(%q,%q)", beforeName, beforeChecksum, afterName, afterChecksum)
	}
	var afterVersion int
	if err = pool.QueryRow(t.Context(), "SELECT max(version) FROM conveyor_schema_migrations").Scan(&afterVersion); err != nil {
		t.Fatal(err)
	}
	if afterVersion != embeddedMigrationHead(t) {
		t.Fatalf("post-upgrade migration version=%d, want the embedded head %d",
			afterVersion, embeddedMigrationHead(t))
	}
	var attemptMigrationName string
	if err = pool.QueryRow(t.Context(), "SELECT name FROM conveyor_schema_migrations WHERE version=49").Scan(&attemptMigrationName); err != nil {
		t.Fatal(err)
	}
	if attemptMigrationName != "049_work_order_attempts.sql" {
		t.Fatalf("version 49 migration=%q", attemptMigrationName)
	}
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns
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
