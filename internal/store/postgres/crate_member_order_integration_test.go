package postgres

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// crateMemberOrderMigration adds the ordered index for the bounded
// work-order crate member read (component-crates invariant 20).
const crateMemberOrderMigration = 174

// TestCrateMemberOrderMigrationIntegration upgrades a ledger at the version
// before migration 174 whose tasks already belong to crates. The upgrade adds
// tasks_workspace_crate_created_idx on (workspace_id, crate_id, created_at,
// id) for crate members only, changes no task row, and keeps the index of
// migration 172. A start that finds the index without its ledger row, as
// after an interrupted start, records the row and adds nothing twice, and a
// replay changes nothing (component-persistence invariants 32 and 36).
func TestCrateMemberOrderMigrationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "crate_member_order_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
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
	if err = migrateControlPlaneToVersion(base, pool, crateMemberOrderMigration-1); err != nil {
		t.Fatalf("migrate to %d: %v", crateMemberOrderMigration-1, err)
	}
	indexes := func() map[string]string {
		t.Helper()
		rows, err := pool.Query(base, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname=$1 AND tablename='tasks' AND indexname LIKE 'tasks_workspace_crate%'`, schema)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, definition string
			if err := rows.Scan(&name, &definition); err != nil {
				t.Fatal(err)
			}
			out[name] = definition
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	ledgerRows := func() int {
		t.Helper()
		var rows int
		if err := pool.QueryRow(base, `SELECT COUNT(*) FROM conveyor_schema_migrations WHERE version=$1`, crateMemberOrderMigration).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if _, found := indexes()["tasks_workspace_crate_created_idx"]; found {
		t.Fatalf("version %d already has the crate member order index", crateMemberOrderMigration-1)
	}

	// An existing database with tasks in a crate and a task in none.
	workspace := "crate-member-order-" + core.NewTaskID()
	if _, err = pool.Exec(base, `INSERT INTO workspaces(id, name) VALUES ($1, $1)`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(base, `INSERT INTO repos(workspace_id, name, url, default_base) VALUES ($1, 'conveyor', 'https://example.test/conveyor', 'main')`, workspace); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(base, workspace)
	st := newStore(pool)
	crate, err := st.CreateCrate(ctx, store.CrateInput{Name: "Before the index"})
	if err != nil {
		t.Fatal(err)
	}
	start := store.CrateTime(time.Now().Add(-time.Hour))
	insert := func(crateID *string, at time.Time) string {
		t.Helper()
		id := core.NewTaskID()
		if _, err := pool.Exec(base, `INSERT INTO tasks
			(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval,policy_version,plan_state,crate_id)
			VALUES ($1,$2,'test',$3,'Legacy body.','','','conveyor','main',$4,'queued',$5,'',false,true,1,'not_required',$6)`,
			id, workspace, "Legacy "+id, "conveyor/task-"+id, at, crateID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	third := insert(&crate.ID, start.Add(2*time.Second))
	second := insert(&crate.ID, start.Add(time.Second))
	first := insert(&crate.ID, start)
	loose := insert(nil, start)
	snapshot := func() string {
		t.Helper()
		var rows string
		if err := pool.QueryRow(base, `SELECT string_agg(id || ':' || COALESCE(crate_id, '') || ':' || state || ':' || title, ',' ORDER BY id) FROM tasks WHERE workspace_id=$1`, workspace).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := snapshot()

	if err = migrateControlPlane(base, pool); err != nil {
		t.Fatal(err)
	}
	assertUpgraded := func(label string) {
		t.Helper()
		found := indexes()
		definition := found["tasks_workspace_crate_created_idx"]
		if !strings.Contains(definition, "(workspace_id, crate_id, created_at, id)") || !strings.Contains(definition, "WHERE (crate_id IS NOT NULL)") {
			t.Fatalf("%s: crate member order index = %q", label, definition)
		}
		if _, kept := found["tasks_workspace_crate_idx"]; !kept || len(found) != 2 {
			t.Fatalf("%s: crate indexes = %v, want migration 172's index and the new one", label, found)
		}
		if ledgerRows() != 1 {
			t.Fatalf("%s: ledger rows for %d = %d", label, crateMemberOrderMigration, ledgerRows())
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
	assertUpgraded("upgrade")

	// The index exists but the ledger row does not, as after a start that
	// stopped before recording the version.
	if _, err = pool.Exec(base, `DELETE FROM conveyor_schema_migrations WHERE version=$1`, crateMemberOrderMigration); err != nil {
		t.Fatal(err)
	}
	if err = migrateControlPlane(base, pool); err != nil {
		t.Fatalf("restart with the index and no ledger row: %v", err)
	}
	assertUpgraded("restart after the index")
	if err = migrateControlPlane(base, pool); err != nil {
		t.Fatal(err)
	}
	assertUpgraded("replay")
}
