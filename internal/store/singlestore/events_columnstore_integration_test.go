package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Use the operator's actual SQL rather than a second implementation of copy,
// validation or allocator preparation. Comments are removed before splitting so
// explanatory semicolons cannot turn into SQL statements.
func eventsOperatorStatements(name string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "sql", "events-columnstore", name))
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	var statements []string
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements, nil
}

func execEventsOperatorScript(ctx context.Context, db *sql.DB, name string) error {
	statements, err := eventsOperatorStatements(name)
	if err != nil {
		return err
	}
	var database string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		return err
	}
	for _, statement := range statements {
		statement = strings.ReplaceAll(statement, "`your_database`", "`"+strings.ReplaceAll(database, "`", "``")+"`")
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("operator script %s: %w", name, err)
		}
	}
	return nil
}

func readEventsCopyValidation(t *testing.T, ctx context.Context, st *Store) [6]int64 {
	t.Helper()
	statements, err := eventsOperatorStatements("03-validate.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 3 {
		t.Fatalf("expected discrepancy query and two workspace summaries, got %d statements", len(statements))
	}
	var result [6]int64
	if err := st.db.QueryRowContext(ctx, statements[0]).Scan(&result[0], &result[1], &result[2], &result[3], &result[4], &result[5]); err != nil {
		t.Fatal(err)
	}
	if result[0] == result[1] && result[2] == 0 && result[3] == 0 && result[4] == 0 && result[5] == 0 {
		source := queryRows(t, st, ctx, statements[1])
		target := queryRows(t, st, ctx, statements[2])
		if !reflect.DeepEqual(source, target) {
			t.Fatalf("workspace summaries differ: source=%v target=%v", source, target)
		}
	}
	return result
}

func assertEventsCopyValid(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	result := readEventsCopyValidation(t, ctx, st)
	if result[0] != result[1] || result[2] != 0 || result[3] != 0 || result[4] != 0 || result[5] != 0 {
		t.Fatalf("operator validation [source destination missing extra changed duplicate]=%v", result)
	}
}

// case: EVC261031445A96-004
// case: EVC261031445A96-005
// case: EVC261031445A96-007
func TestEventsColumnstoreOperatorCopyIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, ws := range []string{"copy-a", "copy-b"} {
		scoped := store.WithWorkspace(ctx, ws)
		if _, err := st.BootstrapWorkspaceConfig(scoped, &config.Config{Workspace: ws}); err != nil {
			t.Fatal(err)
		}
		parent := aggregateTask(scoped, "parent-"+ws)
		if err := st.CreateTask(scoped, parent); err != nil {
			t.Fatal(err)
		}
		child := aggregateTask(scoped, "child-"+ws)
		child.ParentTaskID, child.OriginSpecVersion = parent.ID, 1
		if err := st.CreateTaskWithDependencies(scoped, child, []string{parent.ID}); err != nil {
			t.Fatal(err)
		}
	}
	// Preserve real task/lineage history as the legacy source for this fixture.
	exec(`INSERT INTO events (` + eventColumns + `) SELECT ` + eventColumns + ` FROM events_columnstore`)
	exec(`DELETE FROM events_columnstore`)
	var base int64
	if err := st.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0)+(1<<40) FROM events`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	for i := range 260 {
		ws := "copy-a"
		if i%2 != 0 {
			ws = "copy-b"
		}
		payload := fmt.Sprintf(`{"row":%d}`, i)
		if i == 0 {
			payload = `{"body":"` + strings.Repeat("x", (1<<20)+17) + `"}`
		}
		exec(`INSERT INTO events (`+eventColumns+`) VALUES(?,? ,?,'test.migrated',?,'human',?,?,?)`, base+int64(i*3), "child-"+ws, fmt.Sprintf("job-%d", i), "operator-"+ws, payload, now.Add(time.Duration(i)*time.Microsecond), ws)
	}
	// Equal IDs in different workspaces and both nullable/empty task forms.
	for i, ws := range []string{"copy-a", "copy-b"} {
		exec(`INSERT INTO events (`+eventColumns+`) VALUES(?,NULL,NULL,'requirement.fixture','fixture','system',?,?,?)`, base+1000, `{"requirement_id":"copy-doc","workspace":"`+ws+`"}`, now, ws)
		exec(`INSERT INTO events (`+eventColumns+`) VALUES(?,'','empty-job','test.empty-task','fixture','system','{}',?,?)`, base+1003+int64(i), now, ws)
	}
	before := queryRows(t, st, ctx, `SELECT `+eventColumns+` FROM events`)
	linksBefore := queryRows(t, st, ctx, `SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id FROM links`)
	if err := execEventsOperatorScript(ctx, st.db, "02-copy.sql"); err != nil {
		t.Fatal(err)
	}
	assertEventsCopyValid(t, ctx, st)
	if copied := queryRows(t, st, ctx, `SELECT `+eventColumns+` FROM events_columnstore`); !reflect.DeepEqual(before, copied) {
		t.Fatal("copy changed an event field or identity")
	}
	// Corrupt copies must fail field-aware validation even with equal counts.
	exec(`UPDATE events_columnstore SET payload_json='{"altered":true}' WHERE workspace_id='copy-a' AND id=?`, base)
	if result := readEventsCopyValidation(t, ctx, st); result[4] != 1 {
		t.Fatalf("altered payload not detected: %v", result)
	}
	exec(`UPDATE events_columnstore d JOIN events s ON s.workspace_id=d.workspace_id AND s.id=d.id SET d.payload_json=s.payload_json WHERE d.workspace_id='copy-a' AND d.id=?`, base)
	exec(`DELETE FROM events_columnstore WHERE workspace_id='copy-a' AND id=?`, base)
	if result := readEventsCopyValidation(t, ctx, st); result[2] != 1 || result[0] != result[1]+1 {
		t.Fatalf("omitted source event not detected: %v", result)
	}
	exec(`INSERT INTO events_columnstore (`+eventColumns+`,event_shard_key) SELECT `+eventColumns+`,task_id FROM events WHERE workspace_id='copy-a' AND id=?`, base)
	assertEventsCopyValid(t, ctx, st)
	if err := execEventsOperatorScript(ctx, st.db, "04-allocator.sql"); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"copy-a", "copy-b"} {
		scoped := store.WithWorkspace(ctx, ws)
		rows, err := documentRows(scoped, st.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events WHERE workspace_id=? AND task_id=? ORDER BY at,id`, ws, "child-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := scanDocumentEvents(rows)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := st.ListEvents(scoped, "child-"+ws)
		if err != nil || !reflect.DeepEqual(expected, actual) {
			t.Fatalf("workspace %s history changed: err=%v", ws, err)
		}
		taskless, err := st.ListRequirementEvents(scoped, "copy-doc")
		if err != nil || len(taskless) != 1 || taskless[0].ID != base+1000 || !strings.Contains(string(taskless[0].Payload), ws) {
			t.Fatalf("workspace %s taskless history=%+v err=%v", ws, taskless, err)
		}
		for round := range 2 {
			_, err := st.RebuildLineage(scoped, core.LineageRebuildRequest{Reason: "operator copy proof", RequestID: fmt.Sprintf("copy-%d", round)})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if rebuilt := queryRows(t, st, ctx, `SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id FROM links`); !reflect.DeepEqual(linksBefore, rebuilt) {
		t.Fatalf("rebuild changed copied lineage: before=%v after=%v", linksBefore, rebuilt)
	}
	seenIDs := map[int64]bool{}
	for round := range 2 {
		app, err := Open(ctx, sharedDatabase.cfg.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		for _, ws := range []string{"copy-a", "copy-b"} {
			scoped := store.WithWorkspace(ctx, ws)
			if err := app.AppendEvent(scoped, core.Event{TaskID: "child-" + ws, Kind: "test.resumed", Payload: core.JSONPayload(map[string]int{"round": round}), At: now.Add(time.Duration(round+1) * time.Hour)}); err != nil {
				app.Close()
				t.Fatal(err)
			}
			written, err := app.ListEvents(scoped, "child-"+ws)
			if err != nil {
				app.Close()
				t.Fatal(err)
			}
			last := written[len(written)-1]
			if last.Kind != "test.resumed" || last.ID <= base+1004 || seenIDs[last.ID] {
				app.Close()
				t.Fatalf("resumed identity collided or did not advance: %+v", last)
			}
			seenIDs[last.ID] = true
		}
		app.Close()
	}
	if retained := queryRows(t, st, ctx, `SELECT `+eventColumns+` FROM events`); !reflect.DeepEqual(before, retained) {
		t.Fatal("application restart or resumed writes changed retained legacy history")
	}
	if historical := queryRows(t, st, ctx, `SELECT `+eventColumns+` FROM events_columnstore WHERE kind NOT IN ('test.resumed','lineage.rebuilt')`); !reflect.DeepEqual(before, historical) {
		t.Fatal("resumed writes changed copied history")
	}
}

// case: EVC261031445A96-001
// case: EVC261031445A96-006
func TestEventsColumnstoreExclusiveAccessIntegration(t *testing.T) {
	st, ctx := taskAggregateFixture(t)
	if err := st.CreateTask(ctx, aggregateTask(ctx, "parent")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO events (workspace_id,task_id,kind,actor_id,actor_role,payload_json,at) VALUES(?,'parent','legacy.only','fixture','system','{}',?)`, documentWorkspace(ctx), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	legacyBefore := queryRows(t, st, ctx, `SELECT `+eventColumns+` FROM events`)
	for _, fault := range []string{"events_columnstore", "links"} {
		linksBefore := queryRows(t, st, ctx, `SELECT * FROM links`)
		eventsBefore, err := st.ListEvents(ctx, "parent")
		if err != nil || len(eventsBefore) != 1 || eventsBefore[0].Kind != "task.created" {
			t.Fatalf("stable history read legacy event: %+v err=%v", eventsBefore, err)
		}
		restore := renameTable(t, ctx, st, fault, fault+"_event_fault")
		if fault == "events_columnstore" {
			if _, err := st.ListEvents(ctx, "parent"); err == nil {
				t.Fatal("missing stable table fell back to legacy history")
			}
		}
		child := aggregateTask(ctx, "failed-"+fault)
		if err := st.CreateTaskWithDependencies(ctx, child, []string{"parent"}); err == nil {
			t.Fatal("unavailable event or lineage table allowed task creation")
		}
		restore()
		if _, err := st.GetTask(ctx, child.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("failed event/lineage write left task: %v", err)
		}
		if events, err := st.ListEvents(ctx, child.ID); err != nil || len(events) != 0 {
			t.Fatalf("failed transition left event: %+v err=%v", events, err)
		}
		if linksAfter := queryRows(t, st, ctx, `SELECT * FROM links`); !reflect.DeepEqual(linksBefore, linksAfter) {
			t.Fatal("failed transition changed lineage")
		}
	}
	if err := st.CreateTaskWithDependencies(ctx, aggregateTask(ctx, "committed"), []string{"parent"}); err != nil {
		t.Fatal(err)
	}
	var dependent, event int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM links WHERE workspace_id=? AND src_type='task' AND src_id='committed' AND dst_type='task' AND dst_id='parent' AND kind='depends_on'`, documentWorkspace(ctx)).Scan(&dependent); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND task_id='committed' AND kind='task.dependency_added'`, documentWorkspace(ctx)).Scan(&event); err != nil || dependent != 1 || event != 1 {
		t.Fatalf("committed transition lineage=%d event=%d err=%v", dependent, event, err)
	}
	if retained := queryRows(t, st, ctx, `SELECT `+eventColumns+` FROM events`); !reflect.DeepEqual(legacyBefore, retained) {
		t.Fatal("application transition wrote to legacy events")
	}
}

// case: EVC261031445A96-002
func TestEventsColumnstoreStartupPrerequisiteIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO events (workspace_id,kind,actor_id,actor_role,payload_json,at) VALUES('startup','legacy.only','fixture','system','{}',?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	legacyBefore := queryRows(t, st, ctx, `SELECT `+eventColumns+` FROM events`)
	ledgerBefore := queryRows(t, st, ctx, `SELECT version,name,checksum,applied_at FROM conveyor_singlestore_migrations`)
	restore := renameTable(t, ctx, st, "events_columnstore", "events_columnstore_operator_retained")
	for range 2 {
		app, err := Open(ctx, sharedDatabase.cfg.FormatDSN())
		if err == nil {
			app.Close()
			t.Fatal("application opened without operator-prepared stable table")
		}
		if !strings.Contains(err.Error(), "events_columnstore") {
			t.Fatalf("prerequisite failure does not identify stable table: %v", err)
		}
	}
	if after := queryRows(t, st, ctx, `SELECT version,name,checksum,applied_at FROM conveyor_singlestore_migrations`); !reflect.DeepEqual(ledgerBefore, after) {
		t.Fatal("restart rewrote recorded migration history")
	}
	for _, name := range []string{"events_columnstore", "events_columnstore_0027", "events_rowstore_0027"} {
		if storage, err := st.eventTableStorage(ctx, name); err != nil || storage != "" {
			t.Fatalf("startup created automatic lifecycle table %s storage=%q err=%v", name, storage, err)
		}
	}
	// A stable name with the complete column set is not enough: accepting a
	// rowstore would silently bypass the reviewed columnstore prerequisite.
	if _, err := st.db.ExecContext(ctx, `CREATE ROWSTORE TABLE events_columnstore (
		id BIGINT, event_shard_key VARCHAR(255), task_id VARCHAR(255),
		job_id LONGTEXT, kind LONGTEXT, actor_id LONGTEXT, actor_role LONGTEXT,
		payload_json JSON, at DATETIME(6), workspace_id VARCHAR(255),
		SHARD KEY (workspace_id))`); err != nil {
		t.Fatal(err)
	}
	rowstorePresent := true
	defer func() {
		if rowstorePresent {
			if _, err := st.db.ExecContext(ctx, `DROP TABLE events_columnstore`); err != nil {
				t.Error(err)
			}
		}
	}()
	if app, err := Open(ctx, sharedDatabase.cfg.FormatDSN()); err == nil {
		app.Close()
		t.Fatal("application accepted a rowstore under the stable columnstore name")
	}
	if storage, err := st.eventTableStorage(ctx, "events_columnstore"); err != nil || storage == "" || storage == "COLUMNSTORE" {
		t.Fatalf("startup altered incompatible operator table: storage=%q err=%v", storage, err)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TABLE events_columnstore`); err != nil {
		t.Fatal(err)
	}
	rowstorePresent = false
	restore()
	for range 2 {
		app, err := Open(ctx, sharedDatabase.cfg.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		app.Close()
	}
	if retained := queryRows(t, st, ctx, `SELECT `+eventColumns+` FROM events`); !reflect.DeepEqual(legacyBefore, retained) {
		t.Fatal("startup changed retained source history")
	}
	var copied int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore`).Scan(&copied); err != nil || copied != 0 {
		t.Fatalf("startup copied legacy events: rows=%d err=%v", copied, err)
	}
}

func TestEventsColumnstoreTasklessKeysIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "taskless-events")
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "taskless-events"}); err != nil {
		t.Fatal(err)
	}
	if err := st.withTx(ctx, func(tx *sql.Tx) error {
		for range 64 {
			if err := insertWorkspaceEvent(ctx, tx, core.Event{Kind: "test.taskless", Payload: json.RawMessage(`{}`)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var count, keys, nullTasks int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT event_shard_key),SUM(task_id IS NULL) FROM events_columnstore WHERE workspace_id=? AND kind='test.taskless'`, "taskless-events").Scan(&count, &keys, &nullTasks); err != nil {
		t.Fatal(err)
	}
	if count != 64 || keys != 64 || nullTasks != 64 {
		t.Fatalf("task-less rows=%d independent keys=%d NULL task IDs=%d", count, keys, nullTasks)
	}
}

// Released 0027 already made events a columnstore, including random taskless
// shard keys. The operator copy must preserve these rather than derive new ones.
func TestReleasedColumnstoreOperatorCopyIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	exec := func(q string) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// Reconstruct only this isolated fixture's released source layout.
	exec("DROP TABLE events")
	create, err := eventsOperatorStatements("01-create.sql")
	if err != nil || len(create) != 1 {
		t.Fatalf("create script: %v, %v", create, err)
	}
	exec(strings.Replace(create[0], "events_columnstore", "events", 1))
	exec("INSERT INTO events (id,event_shard_key,task_id,kind,actor_id,actor_role,payload_json,at,workspace_id) VALUES (123,'random-shard',NULL,'released.test','fixture','system','{}','2026-01-01','released')")
	// Start with the empty destination this runbook requires.
	exec("DELETE FROM events_columnstore")
	exec("INSERT INTO events_columnstore (" + eventColumns + ",event_shard_key) SELECT " + eventColumns + ",event_shard_key FROM events")
	assertEventsCopyValid(t, ctx, st)
	shardCheck := "SELECT COUNT(*) FROM events s JOIN events_columnstore d ON s.workspace_id=d.workspace_id AND s.id=d.id WHERE NOT (BINARY s.event_shard_key <=> BINARY d.event_shard_key)"
	check := func(want int) {
		t.Helper()
		var got int
		if err := st.db.QueryRowContext(ctx, shardCheck).Scan(&got); err != nil || got != want {
			t.Fatalf("changed shards=%d, err=%v, want %d", got, err, want)
		}
	}
	check(0)
	exec("DELETE FROM events_columnstore WHERE workspace_id='released' AND id=123")
	exec("INSERT INTO events_columnstore (" + eventColumns + ",event_shard_key) SELECT " + eventColumns + ",'wrong' FROM events WHERE workspace_id='released' AND id=123")
	check(1)
}
