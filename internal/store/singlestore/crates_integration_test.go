package singlestore

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestCrateMigrationIntegration replays migration 0045 and its column hook
// over a task stored at the previous ledger version, when tasks had no
// crate_id column (feature-crates CR-1; component-crates). The column
// arrives NULL with no backfill, so the task reads with no crate and keeps
// every other field. A restart after the column add, before the ledger row,
// adds nothing twice, and the task can then join a crate through the store.
func TestCrateMigrationIntegration(t *testing.T) {
	st := integrationStore(t)
	base := store.WithActor(t.Context(), store.SystemActor())
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(base, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	crateColumn := func() bool {
		t.Helper()
		exists, err := st.columnExists(base, "tasks", "crate_id")
		if err != nil {
			t.Fatal(err)
		}
		return exists
	}
	ledgerRows := func() int {
		t.Helper()
		var rows int
		if err := st.db.QueryRowContext(base, `SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?`, crateMigrationVersion).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if !crateColumn() || ledgerRows() != 1 {
		t.Fatal("a fresh schema lacks tasks.crate_id or its ledger row")
	}

	// The previous ledger version: neither table nor column exists. The
	// cleanup is registered first, so a failed drop still restores the schema.
	t.Cleanup(func() {
		if err := st.migrate(store.WithActor(context.Background(), store.SystemActor())); err != nil {
			t.Errorf("restore migration 0045: %v", err)
		}
	})
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=?`, crateMigrationVersion)
	// Migration 0047's key covers crate_id. Drop the key and its ledger row
	// first, so the replay adds the column and then the key again.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=?`, crateMemberKeyMigrationVersion)
	exec(`ALTER TABLE tasks DROP KEY ` + crateMemberKey.name)
	exec(`ALTER TABLE tasks DROP COLUMN crate_id`)
	exec(`DROP TABLE IF EXISTS crate_requirements`)
	exec(`DROP TABLE IF EXISTS crates`)
	workspace := "crate-migration-" + core.NewTaskID()
	exec(`INSERT INTO workspaces(id, name) VALUES(?, ?)`, workspace, workspace)
	legacyID := core.NewTaskID()
	exec(`INSERT INTO tasks
		(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval,policy_version,plan_state)
		VALUES(?,?,'test','Legacy task','Legacy body.','','','conveyor','main',?,'queued',?,'',false,true,1,'not_required')`,
		legacyID, workspace, "conveyor/task-"+legacyID, time.Now().UTC())

	ctx := store.WithWorkspace(base, workspace)
	assertLegacy := func(label string) {
		t.Helper()
		if !crateColumn() || ledgerRows() != 1 {
			t.Fatalf("%s: crate column present=%t ledger rows=%d", label, crateColumn(), ledgerRows())
		}
		task, err := st.GetTask(ctx, legacyID)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if task.Title != "Legacy task" || task.State != core.TaskQueued || task.PlanState != core.PlanNotRequired || !task.MergeApproval {
			t.Fatalf("%s: legacy task = %+v", label, task)
		}
	}
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertLegacy("upgrade")
	if task, err := st.GetTask(ctx, legacyID); err != nil || task.Crate != nil {
		t.Fatalf("legacy task crate = %+v err=%v, want none", task.Crate, err)
	}
	// A restart after the column add but before the ledger row adds nothing.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=?`, crateMigrationVersion)
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertLegacy("restart after the column add")
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertLegacy("replay")

	date := "2027-02-28"
	crate, err := st.CreateCrate(ctx, store.CrateInput{Name: "After the upgrade", TargetDate: &date})
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
	listed, err := st.ListTasksFiltered(ctx, store.TaskFilter{Crate: crate.ID})
	if err != nil || len(listed) != 1 || listed[0].TaskCrateID() != crate.ID {
		t.Fatalf("crate filter after migration = %+v err=%v", listed, err)
	}
	record, err := st.GetCrate(ctx, crate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.TargetDate == nil || *record.TargetDate != date || len(record.TaskIDs) != 1 || record.TaskIDs[0] != legacyID {
		t.Fatalf("crate after migration = %+v", record)
	}
}
