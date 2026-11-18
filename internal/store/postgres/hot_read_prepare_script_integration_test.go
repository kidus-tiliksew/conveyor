package postgres

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const pgHotReadIndexCatalog = `SELECT c.relname AS index_name, pg_get_indexdef(c.oid) AS definition
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'i'
  AND n.nspname = current_schema()
  AND c.relname IN (
    'tasks_workspace_state_created_idx',
    'tasks_workspace_created_idx',
    'artifacts_workspace_created_idx',
    'artifact_links_artifact_lookup_idx',
    'artifact_links_task_lookup_idx',
    'artifact_links_requirement_lookup_idx',
    'artifact_links_session_lookup_idx',
    'events_planning_session_lookup_idx',
    'events_reference_document_lookup_idx'
  )
ORDER BY c.relname`

func TestHotReadPrepareScriptIntegration(t *testing.T) {
	pool, schema := isolatedPool(t, "hot_read_prepare", nil)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	if err := migrateControlPlaneToVersion(ctx, pool, 170); err != nil {
		t.Fatal(err)
	}
	if n := len(pgHotReadIndexDefinitions(t, ctx, pool)); n != 0 {
		t.Fatalf("v170 fixture already has %d hot-read indexes", n)
	}
	body, err := os.ReadFile("../../../docs/sql/hot-reads/prepare-postgres.sql")
	if err != nil {
		t.Fatal(err)
	}
	ws := "prepare-script-" + core.NewTaskID()
	script := strings.NewReplacer(
		"WORKSPACE_ID", ws,
		"TASK_ID", "task-"+ws,
		"ARTIFACT_ID", "artifact-"+ws,
		"REQUIREMENT_ID", "requirement-"+ws,
		"SESSION_ID", "session-"+ws,
		"DOCUMENT_ID", "document-"+ws,
		"STATE_VALUE", string(core.TaskRunning),
	).Replace(string(body))
	queried, executed := applyPGPrepareScript(t, ctx, pool, script)
	if queried == 0 || executed == 0 {
		t.Fatalf("v170 script queried=%d executed=%d", queried, executed)
	}
	reopened := schemaPool(t, integrationDatabaseURL(t), schema, nil)
	names := pgHotReadIndexDefinitions(t, ctx, reopened)
	if len(names) != 9 {
		t.Fatalf("reopened hot-read indexes = %d, want 9: %v", len(names), names)
	}
	queried, executed = applyPGPrepareScript(t, ctx, reopened, script)
	if queried == 0 || executed == 0 {
		t.Fatalf("prepared rerun queried=%d executed=%d", queried, executed)
	}
	if n := len(pgHotReadIndexDefinitions(t, ctx, reopened)); n != 9 {
		t.Fatalf("rerun hot-read indexes = %d, want 9", n)
	}
}

func applyPGPrepareScript(t *testing.T, ctx context.Context, pool *pgxpool.Pool, script string) (queried, executed int) {
	t.Helper()
	for _, stmt := range splitSQLScript(script) {
		upper := strings.ToUpper(stmt)
		switch {
		case strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "EXPLAIN"):
			rows, err := pool.Query(ctx, stmt)
			if err != nil {
				t.Fatalf("script query %q: %v", stmt, err)
			}
			if err = drainPGRows(rows); err != nil {
				t.Fatalf("script query drain %q: %v", stmt, err)
			}
			queried++
		case strings.HasPrefix(upper, "CREATE"):
			if _, err := pool.Exec(ctx, stmt); err != nil {
				t.Fatalf("script create %q: %v", stmt, err)
			}
			executed++
		default:
			t.Fatalf("unhandled script statement %q", stmt)
		}
	}
	return queried, executed
}

func pgHotReadIndexDefinitions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(ctx, pgHotReadIndexCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var name, def string
		if err = rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		if def == "" {
			t.Fatalf("index %s has empty definition", name)
		}
		names[name] = def
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
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

func drainPGRows(rows pgx.Rows) error {
	defer rows.Close()
	fields := rows.FieldDescriptions()
	for rows.Next() {
		dest := make([]any, len(fields))
		ptrs := make([]any, len(fields))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
	}
	return rows.Err()
}
