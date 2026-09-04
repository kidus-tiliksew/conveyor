package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestBlueprintOriginIdentityMigrationRejectsLiveDuplicatesIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "blueprint_identity_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
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
	if err = migrateControlPlaneToVersion(t.Context(), pool, 41); err != nil {
		t.Fatalf("migrate isolated schema to version 41: %v", err)
	}

	workspace := "blueprint-migration-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	st := newStore(pool)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	parentID := core.NewTaskID()
	if _, err = st.queries.InsertTask(ctx, taskInsertParams(core.Task{
		ID: parentID, Workspace: workspace, Repo: "repo", Branch: "conveyor/task-" + parentID,
		State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	})); err != nil {
		t.Fatal(err)
	}
	childIDs := []string{core.NewTaskID(), core.NewTaskID()}
	for index, childID := range childIDs {
		if _, err = st.queries.InsertTask(ctx, taskInsertParams(core.Task{
			ID: childID, Workspace: workspace, Repo: "repo", Branch: "conveyor/task-" + childID,
			State: core.TaskQueued, ParentTaskID: parentID, OriginSpecVersion: index + 1,
			OriginSubID: "SUB-1", CreatedAt: time.Now().UTC().Add(time.Duration(index) * time.Second),
		})); err != nil {
			t.Fatal(err)
		}
	}

	err = migrateControlPlaneToVersion(t.Context(), pool, 42)
	if err == nil || !strings.Contains(err.Error(), "duplicate live blueprint children") ||
		!strings.Contains(err.Error(), "SUB-1") {
		t.Fatalf("duplicate migration error=%v", err)
	}
	var version, rows int
	if err = pool.QueryRow(ctx, "SELECT max(version) FROM conveyor_schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 41 {
		t.Fatalf("migration version=%d after rejected upgrade, want 41", version)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM tasks
		WHERE workspace_id=$1 AND parent_task_id=$2 AND origin_sub_id='SUB-1'`, workspace, parentID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("duplicate rows=%d after rejected upgrade, want 2 preserved", rows)
	}

	if _, err = pool.Exec(ctx, "UPDATE tasks SET state='merged' WHERE id=$1 AND workspace_id=$2", childIDs[1], workspace); err != nil {
		t.Fatal(err)
	}
	if err = migrateControlPlaneToVersion(t.Context(), pool, 42); err != nil {
		t.Fatalf("upgrade with one historical terminal child: %v", err)
	}
	var indexDefinition string
	if err = pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
		WHERE schemaname=current_schema() AND indexname='tasks_live_blueprint_origin_idx'`).Scan(&indexDefinition); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(indexDefinition, "origin_spec_version") ||
		!strings.Contains(indexDefinition, "(workspace_id, parent_task_id, origin_sub_id)") {
		t.Fatalf("unexpected tightened index: %s", indexDefinition)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM tasks
		WHERE workspace_id=$1 AND parent_task_id=$2 AND origin_sub_id='SUB-1'`, workspace, parentID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("historical rows=%d after successful upgrade, want 2 preserved", rows)
	}
}
