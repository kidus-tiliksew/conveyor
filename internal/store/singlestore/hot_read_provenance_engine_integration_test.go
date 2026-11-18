package singlestore

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func hotReadShowCreate(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var name, ddl string
	if err := db.QueryRowContext(t.Context(), "SHOW CREATE TABLE `"+table+"`").Scan(&name, &ddl); err != nil {
		t.Fatal(err)
	}
	return ddl
}
func hotReadAssertNoPreparedObjects(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	for _, index := range hotReadIndexes {
		if index.table != table {
			continue
		}
		for kind, name := range map[string]string{"column": index.column, "key": index.name} {
			if name == "" {
				continue
			}
			present, err := ssPrepareObjectPresent(t.Context(), db, table, kind, name)
			if err != nil || present {
				t.Fatalf("%s %s.%s present=%v: %v", kind, table, name, present, err)
			}
		}
	}
}

func hotReadInitialTable(t *testing.T, db *sql.DB, table string, rowstore bool) {
	t.Helper()
	raw, err := migrationFiles.ReadFile("migrations/0001_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(string(raw), ";") {
		statement = strings.TrimSpace(statement)
		if strings.Contains(statement, "CREATE TABLE IF NOT EXISTS `"+table+"` (") || strings.Contains(statement, "CREATE ROWSTORE TABLE IF NOT EXISTS `"+table+"` (") {
			if rowstore {
				statement = strings.Replace(statement, "CREATE TABLE IF NOT EXISTS", "CREATE ROWSTORE TABLE IF NOT EXISTS", 1)
				statement = strings.Replace(statement, " SORT KEY (`workspace_id`),\n", "", 1)
			}
			if _, err := db.ExecContext(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("initial schema does not define %s", table)
}

func TestHotReadActualMixedOriginAndInterruptedBootstrapIntegration(t *testing.T) {
	ctx := t.Context()
	db, dsn := hotReadIntegrationDatabase(t, false)
	hotReadInitialTable(t, db, "tasks", false)
	beforeEvents := hotReadShowCreate(t, db, "events_columnstore")
	hotReadAssertNoPreparedObjects(t, db, "tasks")
	// Capture decisions as startup does, then stop after later DDL has committed.
	partial := &Store{db: db}
	fresh, err := partial.hotReadBootstrapOrigin(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh, map[string]bool{"tasks": false, "artifacts": true, "artifact_links": true}) {
		t.Fatalf("initial origin capture = %v", fresh)
	}
	hotReadInitialTable(t, db, "artifact_links", false)
	for attempt := range 2 {
		s, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		for _, index := range hotReadIndexes {
			if index.column == "" {
				continue
			}
			want := index.table != "tasks"
			if got := s.hotReadIndexReady(index.name); got != want {
				t.Errorf("attempt %d: %s ready=%v, want %v", attempt, index.name, got, want)
			}
		}
		s.Close()
		hotReadAssertNoPreparedObjects(t, db, "tasks")
		if got := hotReadShowCreate(t, db, "events_columnstore"); got != beforeEvents {
			t.Fatalf("attempt %d changed operator events layout", attempt)
		}
		for id, want := range map[int]string{1: "fresh", 2: "legacy", 3: "fresh", 4: "fresh"} {
			var got string
			if err := db.QueryRowContext(ctx, `SELECT origin FROM conveyor_singlestore_bootstrap WHERE id=?`, id).Scan(&got); err != nil || got != want {
				t.Fatalf("attempt %d: origin %d=%q, want %q: %v", attempt, id, got, want, err)
			}
		}
	}
}

// Extract the commented rowstore variant without rewriting its SQL.
func hotReadEngineStatements(t *testing.T, script, table string, rowstore bool) []string {
	t.Helper()
	var result []string
	for _, stmt := range splitSQLScript(script) {
		target, kind, _ := sqlScriptAlterTarget(stmt)
		if target == table && (kind == "column" || (kind == "key" && !rowstore)) {
			result = append(result, stmt)
		}
	}
	if rowstore {
		var b strings.Builder
		inVariant := false
		for _, line := range strings.Split(script, "\n") {
			if strings.HasPrefix(line, "-- Variant B:") {
				inVariant = true
				continue
			}
			if !inVariant {
				continue
			}
			if !strings.HasPrefix(line, "-- ") {
				inVariant = false
				continue
			}
			b.WriteString(strings.TrimPrefix(line, "-- "))
			b.WriteByte('\n')
		}
		for _, stmt := range splitSQLScript(b.String()) {
			target, kind, _ := sqlScriptAlterTarget(stmt)
			if target == table && kind == "key" {
				result = append(result, stmt)
			}
		}
	}
	return result
}

func hotReadApplyAndCheckEngine(t *testing.T, db *sql.DB, table string, rowstore, apply bool) {
	t.Helper()
	ctx := t.Context()
	script := loadHotReadPrepareScript(t, "prepare-singlestore.sql", "ws", "task")
	statements := hotReadEngineStatements(t, script, table, rowstore)
	wantCount := 6
	if table == "artifact_links" {
		wantCount = 8
	}
	if len(statements) != wantCount {
		t.Fatalf("%s rowstore=%v: found %d script statements, want %d", table, rowstore, len(statements), wantCount)
	}
	if apply {
		for _, stmt := range statements {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("script statement %q: %v", stmt, err)
			}
		}
	}
	st := &Store{db: db}
	for _, index := range hotReadIndexes {
		if index.table != table || index.column == "" {
			continue
		}
		if err := st.checkHotReadColumn(ctx, index); err != nil {
			t.Fatalf("%s column: %v", index.name, err)
		}
		rows, err := db.QueryContext(ctx, `SELECT column_name,index_type,non_unique FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=? ORDER BY seq_in_index`, table, index.name)
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for rows.Next() {
			var column, kind string
			var nonUnique int
			if err := rows.Scan(&column, &kind, &nonUnique); err != nil {
				t.Fatal(err)
			}
			if !hotReadIndexTypeMatches(index, !rowstore, kind, nonUnique) {
				t.Errorf("%s unexpected key type %s non_unique=%d", index.name, kind, nonUnique)
			}
			columns = append(columns, column)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !reflect.DeepEqual(columns, index.columns) {
			t.Errorf("%s columns=%v, want %v", index.name, columns, index.columns)
		}
	}
}

func TestHotReadOperatorScriptEngineVariantsIntegration(t *testing.T) {
	for _, rowstore := range []bool{false, true} {
		t.Run(fmt.Sprintf("rowstore=%v", rowstore), func(t *testing.T) {
			db, _ := hotReadIntegrationDatabaseWithEvents(t, false, false)
			engine := ""
			if rowstore {
				engine = "ROWSTORE "
			}
			if _, err := db.ExecContext(t.Context(), "CREATE "+engine+"TABLE events_columnstore (workspace_id VARCHAR(255) NOT NULL, payload_json JSON NOT NULL, SHARD KEY(workspace_id))"); err != nil {
				t.Fatal(err)
			}
			hotReadInitialTable(t, db, "artifact_links", rowstore)
			for _, table := range []string{"events_columnstore", "artifact_links"} {
				hotReadApplyAndCheckEngine(t, db, table, rowstore, true)
			}
		})
	}
}

func TestHotReadRowstoreArtifactLinksStoreReadinessIntegration(t *testing.T) {
	ctx := t.Context()
	db, dsn := hotReadIntegrationDatabase(t, false)
	hotReadInitialTable(t, db, "artifact_links", true)
	hotReadAssertNoPreparedObjects(t, db, "artifact_links")
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range hotReadIndexes {
		if index.table == "artifact_links" && st.hotReadIndexReady(index.name) {
			t.Errorf("unprepared %s is ready", index.name)
		}
	}
	st.Close()
	hotReadAssertNoPreparedObjects(t, db, "artifact_links")
	applySSPrepareScript(t, ctx, db, loadHotReadPrepareScript(t, "prepare-singlestore.sql", "ws", "task"))
	hotReadApplyAndCheckEngine(t, db, "artifact_links", true, false)
	st, err = Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, index := range hotReadIndexes {
		if index.table == "artifact_links" && !st.hotReadIndexReady(index.name) {
			t.Errorf("prepared %s is not ready", index.name)
		}
	}
}
