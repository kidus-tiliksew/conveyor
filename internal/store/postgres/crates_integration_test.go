package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// crateMigration is the migration that adds crates, crate_requirements, and
// tasks.crate_id (feature-crates CR-1).
const crateMigration = 172

// TestCrateMigrationIntegration upgrades a ledger from the version before the
// crate migration. A task stored before the migration reads with no crate,
// keeps every other field, and can then join a crate through the store; the
// migration replays without touching data (component-crates;
// component-persistence).
func TestCrateMigrationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "crate_migration_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	base := store.WithActor(t.Context(), store.SystemActor())
	if err = migrateControlPlaneToVersion(base, pool, crateMigration-1); err != nil {
		t.Fatalf("migrate to %d: %v", crateMigration-1, err)
	}
	var columns int
	if err = pool.QueryRow(base, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='tasks' AND column_name='crate_id'`, schema).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("version %d already has tasks.crate_id", crateMigration-1)
	}
	workspace := "crate-migration-" + core.NewTaskID()
	if _, err = pool.Exec(base, `INSERT INTO workspaces(id, name) VALUES ($1, $1)`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(base, `INSERT INTO repos(workspace_id, name, url, default_base) VALUES ($1, 'conveyor', 'https://example.test/conveyor', 'main')`, workspace); err != nil {
		t.Fatal(err)
	}
	legacyID := core.NewTaskID()
	if _, err = pool.Exec(base, `INSERT INTO tasks
		(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval,policy_version,plan_state)
		VALUES ($1,$2,'test','Legacy task','Legacy body.','','','conveyor','main',$3,'queued',$4,'',false,true,1,'not_required')`,
		legacyID, workspace, "conveyor/task-"+legacyID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err = migrateControlPlane(base, pool); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(base, workspace)
	st := newStore(pool)
	legacy, err := st.GetTask(ctx, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Crate != nil || legacy.Title != "Legacy task" || legacy.State != core.TaskQueued || legacy.PlanState != core.PlanNotRequired || !legacy.MergeApproval {
		t.Fatalf("legacy task after migration = %+v", legacy)
	}
	crate, err := st.CreateCrate(ctx, store.CrateInput{Name: "After the upgrade", TargetDate: func() *string { d := "2027-02-28"; return &d }()})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := st.SetTaskCrate(ctx, legacyID, crate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if joined.TaskCrateID() != crate.ID {
		t.Fatalf("joined task crate = %+v", joined.Crate)
	}
	// Every hand-maintained task read carries the column (component-persistence).
	listed, err := st.ListTasksFiltered(ctx, store.TaskFilter{Crate: crate.ID})
	if err != nil || len(listed) != 1 || listed[0].TaskCrateID() != crate.ID {
		t.Fatalf("crate filter after migration = %+v err=%v", listed, err)
	}
	if all, err := st.ListTasks(ctx); err != nil || len(all) != 1 || all[0].TaskCrateID() != crate.ID {
		t.Fatalf("task list after migration = %+v err=%v", all, err)
	}
	if byKey, _, err := st.GetTaskByIntakeKey(ctx, "absent"); err != nil || byKey.ID != "" {
		t.Fatalf("intake key read = %+v err=%v", byKey, err)
	}
	if err = migrateControlPlane(base, pool); err != nil {
		t.Fatal(err)
	}
	record, err := st.GetCrate(ctx, crate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.TargetDate == nil || *record.TargetDate != "2027-02-28" || len(record.TaskIDs) != 1 || record.TaskIDs[0] != legacyID {
		t.Fatalf("crate after replay = %+v", record)
	}
	// The foreign key keeps membership inside the crate's workspace.
	if _, err = pool.Exec(base, `UPDATE tasks SET crate_id='crate-000000-000000' WHERE id=$1`, legacyID); err == nil {
		t.Fatal("tasks.crate_id accepted a crate that does not exist")
	}
}
