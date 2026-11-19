package singlestore

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestCrateMemberKeyMigrationIntegration replays migration 0047 and its key
// step over a database at the previous ledger version whose tasks already
// belong to crates. The step adds tasks_workspace_crate_created_idx on
// (workspace_id, crate_id, created_at, id) and changes no task row. A start
// that finds the key without its ledger row, as after an interrupted start,
// records the row and adds nothing twice. A key of that name with other
// columns refuses the start, records no ledger row, and is left as it is
// (component-persistence invariants 32 and 36; component-crates invariant
// 20).
func TestCrateMemberKeyMigrationIntegration(t *testing.T) {
	st := integrationStore(t)
	base := store.WithActor(t.Context(), store.SystemActor())
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(base, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	keyColumns := func() []string {
		t.Helper()
		keys, err := st.hotReadTableKeys(base, "tasks")
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for _, part := range keys[crateMemberKey.name] {
			columns = append(columns, strings.ToLower(part.column))
		}
		return columns
	}
	ledgerRows := func() int {
		t.Helper()
		var rows int
		if err := st.db.QueryRowContext(base, `SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?`, crateMemberKeyMigrationVersion).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	dropKey := func() {
		t.Helper()
		if len(keyColumns()) > 0 {
			exec(`ALTER TABLE tasks DROP KEY ` + crateMemberKey.name)
		}
	}
	if !slices.Equal(keyColumns(), crateMemberKey.columns) || ledgerRows() != 1 {
		t.Fatalf("a fresh schema has key columns %v and %d ledger rows", keyColumns(), ledgerRows())
	}
	// The cleanup is registered first, so a failed step still restores the
	// key and its ledger row for later tests.
	t.Cleanup(func() {
		ctx := store.WithActor(context.Background(), store.SystemActor())
		keys, err := st.hotReadTableKeys(ctx, "tasks")
		if err == nil && checkCrateMemberKey(keys[crateMemberKey.name]) != nil && len(keys[crateMemberKey.name]) > 0 {
			_, err = st.db.ExecContext(ctx, `ALTER TABLE tasks DROP KEY `+crateMemberKey.name)
		}
		if err == nil {
			_, err = st.db.ExecContext(ctx, `DELETE FROM conveyor_singlestore_migrations WHERE version=?`, crateMemberKeyMigrationVersion)
		}
		if err == nil {
			err = st.migrate(ctx)
		}
		if err != nil {
			t.Errorf("restore migration 0047: %v", err)
		}
	})

	// The previous ledger version: no key, and tasks already in a crate.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=?`, crateMemberKeyMigrationVersion)
	dropKey()
	workspace := "crate-member-key-" + core.NewTaskID()
	exec(`INSERT INTO workspaces(id, name) VALUES(?, ?)`, workspace, workspace)
	ctx := store.WithWorkspace(base, workspace)
	crate, err := st.CreateCrate(ctx, store.CrateInput{Name: "Before the key"})
	if err != nil {
		t.Fatal(err)
	}
	start := store.CrateTime(time.Now().Add(-time.Hour))
	insert := func(crateID any, at time.Time) string {
		t.Helper()
		id := core.NewTaskID()
		exec(`INSERT INTO tasks
			(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval,policy_version,plan_state,crate_id)
			VALUES(?,?,'test',?,'Legacy body.','','','conveyor','main',?,'queued',?,'',false,true,1,'not_required',?)`,
			id, workspace, "Legacy "+id, "conveyor/task-"+id, at, crateID)
		return id
	}
	third := insert(crate.ID, start.Add(2*time.Second))
	second := insert(crate.ID, start.Add(time.Second))
	first := insert(crate.ID, start)
	loose := insert(nil, start)
	snapshot := func() string {
		t.Helper()
		rows, err := st.db.QueryContext(base, `SELECT id,COALESCE(crate_id,''),state,title FROM tasks WHERE workspace_id=? ORDER BY id`, workspace)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, crateID, state, title string
			if err := rows.Scan(&id, &crateID, &state, &title); err != nil {
				t.Fatal(err)
			}
			out = append(out, id+":"+crateID+":"+state+":"+title)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, ",")
	}
	before := snapshot()
	assertUpgraded := func(label string) {
		t.Helper()
		if got := keyColumns(); !slices.Equal(got, crateMemberKey.columns) || ledgerRows() != 1 {
			t.Fatalf("%s: key columns %v, ledger rows %d", label, got, ledgerRows())
		}
		keys, err := st.hotReadTableKeys(base, "tasks")
		if err != nil {
			t.Fatal(err)
		}
		if err := checkCrateMemberKey(keys[crateMemberKey.name]); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if after := snapshot(); after != before {
			t.Fatalf("%s: task rows changed:\nbefore %s\nafter  %s", label, before, after)
		}
		members, more, err := st.ListCrateMembersLimited(ctx, crate.ID, second, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(members) != 1 || members[0].ID != first || !more {
			t.Fatalf("%s: bounded member read = %+v more=%t, want %s and more", label, members, more, first)
		}
		members, more, err = st.ListCrateMembersLimited(ctx, crate.ID, "", 3)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, member := range members {
			ids = append(ids, member.ID)
		}
		if !slices.Equal(ids, []string{first, second, third}) || more || slices.Contains(ids, loose) {
			t.Fatalf("%s: members = %v more=%t", label, ids, more)
		}
	}
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertUpgraded("upgrade")

	// The key exists but the ledger row does not, as after a start that
	// stopped after the implicit DDL commit.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=?`, crateMemberKeyMigrationVersion)
	if err := st.migrate(base); err != nil {
		t.Fatalf("restart with the key and no ledger row: %v", err)
	}
	assertUpgraded("restart after the key")
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertUpgraded("replay")

	// A key of that name with other columns refuses the start.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=?`, crateMemberKeyMigrationVersion)
	dropKey()
	exec(`ALTER TABLE tasks ADD KEY ` + crateMemberKey.name + ` (workspace_id,crate_id)`)
	err = st.migrate(base)
	if err == nil || !strings.Contains(err.Error(), "incompatible crate member key") {
		t.Fatalf("start over a mismatched key = %v, want an incompatible key refusal", err)
	}
	if got := keyColumns(); !slices.Equal(got, []string{"workspace_id", "crate_id"}) || ledgerRows() != 0 {
		t.Fatalf("after the refusal: key columns %v, ledger rows %d", got, ledgerRows())
	}
}
