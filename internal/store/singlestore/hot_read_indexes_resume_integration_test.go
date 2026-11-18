package singlestore

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// A wrong existing computed column or key must fail closed before any other
// missing column or key of the same table is added.
func TestHotReadResumeWrongExistingObjectFailsBeforeDDLIntegration(t *testing.T) {
	cases := map[string]string{
		"wrong computed column": "ALTER TABLE tasks ADD COLUMN task_state_key AS (SHA2('wrong',256)) PERSISTED VARCHAR(64)",
		"wrong key columns":     "ALTER TABLE tasks ADD KEY tasks_workspace_intake_lookup_idx (workspace_id)",
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			db, _ := hotReadIntegrationDatabaseWithEvents(t, false, false)
			hotReadInitialTable(t, db, "tasks", true)
			if _, err := db.ExecContext(ctx, setup); err != nil {
				t.Fatal(err)
			}
			before := hotReadTaskObjects(t, db)
			err := (&Store{db: db}).migrateHotReadIndexes(ctx, map[string]bool{"tasks": true})
			if err == nil || !strings.Contains(err.Error(), "incompatible hot-read") {
				t.Fatalf("migration error = %v, want incompatible hot-read object", err)
			}
			if after := hotReadTaskObjects(t, db); after != before {
				t.Fatalf("DDL ran before validation:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// hotReadTaskObjects lists the tasks columns and index names so that any added
// column or key shows up as a changed snapshot.
func hotReadTaskObjects(t *testing.T, db *sql.DB) string {
	t.Helper()
	var objects []string
	for _, query := range []struct {
		prefix, sql string
	}{
		{"column:", `SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='tasks'`},
		{"key:", `SELECT DISTINCT index_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='tasks'`},
	} {
		rows, err := db.QueryContext(t.Context(), query.sql)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			objects = append(objects, query.prefix+strings.ToLower(name))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(objects)
	return strings.Join(objects, ",")
}
