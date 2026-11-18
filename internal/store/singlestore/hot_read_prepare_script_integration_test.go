package singlestore

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestHotReadPrepareScriptIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL") == "" {
		t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	db, dsn := hotReadIntegrationDatabase(t, true)
	ctx := t.Context()
	old, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(old.Close)
	assertHotReadGeneratedLookups(t, old, false)
	script := loadHotReadPrepareScript(t, "prepare-singlestore.sql", "legacy-ws", "legacy-task")
	alterCount := sqlScriptAlterCount(script)
	queried, executed, skipped := applySSPrepareScript(t, ctx, db, script)
	if queried == 0 {
		t.Fatal("prepare script served no SELECT/SHOW/EXPLAIN results")
	}
	if executed == 0 {
		t.Fatal("legacy fixture executed no missing ALTER statements")
	}
	if executed+skipped != alterCount {
		t.Fatalf("legacy ALTER executed=%d skipped=%d, want %d targets", executed, skipped, alterCount)
	}
	assertSSPrepareAlterTargets(t, ctx, db, script)
	assertHotReadGeneratedLookups(t, old, false)
	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	assertHotReadGeneratedLookups(t, reopened, true)
	queried, executed, skipped = applySSPrepareScript(t, ctx, db, script)
	if queried == 0 {
		t.Fatal("rerun prepare script served no SELECT/SHOW/EXPLAIN results")
	}
	if executed != 0 {
		t.Fatalf("prepared rerun executed %d ALTER statements, want skip of matching objects", executed)
	}
	if skipped != alterCount {
		t.Fatalf("prepared rerun skipped %d ALTER statements, want all %d", skipped, alterCount)
	}
	assertSSPrepareAlterTargets(t, ctx, db, script)
}

func assertHotReadGeneratedLookups(t *testing.T, st *Store, wantReady bool) {
	t.Helper()
	for _, index := range hotReadIndexes {
		if index.column == "" {
			continue
		}
		if st.hotReadIndexReady(index.name) != wantReady {
			t.Fatalf("lookup %s ready=%v, want %v", index.name, st.hotReadIndexReady(index.name), wantReady)
		}
	}
}

func loadHotReadPrepareScript(t *testing.T, name, workspace, taskID string) string {
	t.Helper()
	body, err := os.ReadFile("../../../docs/sql/hot-reads/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(
		"WORKSPACE_ID", workspace,
		"TASK_ID", taskID,
		"ARTIFACT_ID", "artifact-"+taskID,
		"REQUIREMENT_ID", "requirement-"+taskID,
		"SESSION_ID", "session-"+taskID,
		"DOCUMENT_ID", "document-"+taskID,
		"STATE_VALUE", string(core.TaskRunning),
		"INTAKE_KEY", "intake-"+taskID,
	).Replace(string(body))
}

func applySSPrepareScript(t *testing.T, ctx context.Context, db *sql.DB, script string) (queried, executed, skipped int) {
	t.Helper()
	for _, stmt := range splitSQLScript(script) {
		upper := strings.ToUpper(stmt)
		switch {
		case strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "SHOW") || strings.HasPrefix(upper, "EXPLAIN"):
			rows, err := db.QueryContext(ctx, stmt)
			if err != nil {
				t.Fatalf("script query %q: %v", stmt, err)
			}
			if err = drainSQLRows(rows); err != nil {
				t.Fatalf("script query drain %q: %v", stmt, err)
			}
			queried++
		case strings.HasPrefix(upper, "ALTER"):
			table, kind, name := sqlScriptAlterTarget(stmt)
			if table == "" || kind == "" || name == "" {
				t.Fatalf("unrecognized ALTER %q", stmt)
			}
			if kind == "key" && (table == "events_columnstore" || table == "artifact_links") {
				columnstore, err := (&Store{db: db}).hotReadTableColumnstore(ctx, table)
				if err != nil {
					t.Fatal(err)
				}
				if !columnstore {
					var replacement string
					for _, candidate := range hotReadEngineStatements(t, script, table, true) {
						_, candidateKind, candidateName := sqlScriptAlterTarget(candidate)
						if candidateKind == "key" && candidateName == name {
							replacement = candidate
							break
						}
					}
					if replacement == "" {
						t.Fatalf("missing rowstore script variant for %s.%s", table, name)
					}
					stmt = replacement
				}
			}
			present, err := ssPrepareObjectPresent(ctx, db, table, kind, name)
			if err != nil {
				t.Fatalf("inspect %s %s.%s: %v", kind, table, name, err)
			}
			if present {
				skipped++
				continue
			}
			if _, err = db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("script alter %q: %v", stmt, err)
			}
			executed++
		default:
			t.Fatalf("unhandled script statement %q", stmt)
		}
	}
	return queried, executed, skipped
}

func sqlScriptAlterCount(script string) int {
	n := 0
	for _, stmt := range splitSQLScript(script) {
		if strings.HasPrefix(strings.ToUpper(stmt), "ALTER") {
			n++
		}
	}
	return n
}

func assertSSPrepareAlterTargets(t *testing.T, ctx context.Context, db *sql.DB, script string) {
	t.Helper()
	for _, stmt := range splitSQLScript(script) {
		if !strings.HasPrefix(strings.ToUpper(stmt), "ALTER") {
			continue
		}
		table, kind, name := sqlScriptAlterTarget(stmt)
		present, err := ssPrepareObjectPresent(ctx, db, table, kind, name)
		if err != nil {
			t.Fatalf("postflight %s %s.%s: %v", kind, table, name, err)
		}
		if !present {
			t.Fatalf("postflight missing %s %s.%s", kind, table, name)
		}
	}
}

func sqlScriptAlterTarget(stmt string) (table, kind, name string) {
	fields := strings.Fields(stmt)
	for i := 0; i+1 < len(fields); i++ {
		if strings.EqualFold(fields[i], "TABLE") {
			table = strings.Trim(fields[i+1], "`")
			continue
		}
		if i+2 >= len(fields) {
			continue
		}
		switch strings.ToUpper(fields[i]) + " " + strings.ToUpper(fields[i+1]) {
		case "ADD COLUMN":
			return table, "column", strings.Trim(fields[i+2], "`")
		case "ADD KEY":
			return table, "key", strings.Trim(fields[i+2], "`")
		}
	}
	return table, "", ""
}

func ssPrepareObjectPresent(ctx context.Context, db *sql.DB, table, kind, name string) (bool, error) {
	var n int
	var err error
	switch kind {
	case "column":
		err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, table, name).Scan(&n)
	case "key":
		err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?`, table, name).Scan(&n)
	default:
		return false, nil
	}
	return n > 0, err
}

func splitSQLScript(src string) []string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	var stmts []string
	for _, part := range strings.Split(b.String(), ";") {
		stmt := strings.TrimSpace(part)
		if stmt != "" {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
}

func drainSQLRows(rows *sql.Rows) error {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			return err
		}
	}
	return rows.Err()
}
