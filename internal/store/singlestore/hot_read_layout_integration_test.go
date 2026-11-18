package singlestore

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// The private database is created specifically for this test: preparing its
// schema must not change the shared integration fixture or another test.
func hotReadIntegrationDatabase(t *testing.T, legacy bool) (*sql.DB, string) {
	return hotReadIntegrationDatabaseWithEvents(t, legacy, true)
}

func hotReadIntegrationDatabaseWithEvents(t *testing.T, legacy, createEvents bool) (*sql.DB, string) {
	t.Helper()
	integrationStore(t)
	ctx := t.Context()
	name := fmt.Sprintf("conveyor_hot_read_%d_test", time.Now().UnixNano())
	if _, err := sharedDatabase.admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` PARTITIONS 2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := sharedDatabase.admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})
	cfg := *sharedDatabase.cfg
	cfg.DBName = name
	connector, err := mysql.NewConnector(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { db.Close() })
	if createEvents {
		if legacy {
			statements, err := eventsOperatorStatements("01-create.sql")
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			for _, line := range strings.Split(statements[0], "\n") {
				if strings.Contains(line, "event_document_key") || strings.Contains(line, "event_requirement_key") || strings.Contains(line, "event_session_key") {
					continue
				}
				if strings.Contains(line, "KEY events_task_idx") {
					line = strings.TrimSuffix(line, ",")
				}
				lines = append(lines, line)
			}
			if _, err := db.ExecContext(ctx, strings.Join(lines, "\n")); err != nil {
				t.Fatal(err)
			}
		} else if err := execEventsOperatorScript(ctx, db, "01-create.sql"); err != nil {
			t.Fatal(err)
		}
	}
	if legacy {
		if _, err := db.ExecContext(ctx, `CREATE ROWSTORE TABLE conveyor_singlestore_bootstrap (id INT NOT NULL,origin VARCHAR(16) NOT NULL,PRIMARY KEY(id),SHARD KEY(id))`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO conveyor_singlestore_bootstrap(id,origin) VALUES (1,'legacy')`); err != nil {
			t.Fatal(err)
		}
	}
	return db, cfg.FormatDSN()
}

func TestHotReadLayoutDatabaseIntegration(t *testing.T) {
	ctx := t.Context()
	db, dsn := hotReadIntegrationDatabase(t, true)
	open := func() *Store {
		t.Helper()
		s, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
	legacy := open()
	for _, index := range hotReadIndexes {
		if index.column != "" && legacy.hotReadIndexReady(index.name) {
			t.Fatalf("legacy lookup %s ready", index.name)
		}
	}
	// Prepare one lookup first; restarting sees it without enabling its peers.
	index := hotReadIndexes[0]
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+index.table+" ADD COLUMN "+index.column+" AS ("+index.expression+") PERSISTED VARCHAR(64)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+index.table+" ADD KEY "+index.name+" ("+strings.Join(index.columns, ",")+") USING HASH"); err != nil {
		t.Fatal(err)
	}
	partial := open()
	if !partial.hotReadIndexReady(index.name) || partial.hotReadIndexReady(hotReadIndexes[1].name) {
		t.Fatal("partial schema was not discovered per lookup")
	}
	if legacy.hotReadIndexReady(index.name) {
		t.Fatal("legacy store changed after preparation")
	}
	// Prepare the remaining operator-owned and legacy objects explicitly.
	applySSPrepareScript(t, ctx, db, loadHotReadPrepareScript(t, "prepare-singlestore.sql", "ws", "task"))
	if partial.hotReadIndexReady(hotReadIndexes[1].name) || legacy.hotReadIndexReady(index.name) {
		t.Fatal("running store changed after preparation")
	}
	prepared := open()
	for _, candidate := range hotReadIndexes {
		if candidate.column != "" && !prepared.hotReadIndexReady(candidate.name) {
			t.Errorf("prepared lookup %s not ready", candidate.name)
		}
	}
	collector := dbmetrics.NewCollector()
	measured := dbmetrics.WithCollector(ctx, collector)
	probe := &Store{db: prepared.db}
	if err := probe.initHotReadLayout(measured); err != nil {
		t.Fatal(err)
	}
	if got := collector.Snapshot().Queries; got > 24 {
		t.Fatalf("discovery used %d queries, want at most 24", got)
	}
	prepared.ReadStats(0)
	readCollector := dbmetrics.NewCollector()
	readCtx := store.WithWorkspace(dbmetrics.WithCollector(ctx, readCollector), "layout-proof")
	if _, err := prepared.ListEvents(readCtx, "absent-task"); err != nil {
		t.Fatal(err)
	}
	if got := readCollector.Snapshot().Queries; got != 1 {
		t.Fatalf("hot read used %d queries, want 1", got)
	}
	stats := prepared.ReadStats(100)
	for _, entry := range stats.Top {
		if strings.Contains(strings.ToLower(entry.Label), "metadata") {
			t.Fatalf("unexpected metadata read: %s", entry.Label)
		}
	}
}

func TestHotReadLayoutIncompatibleDatabaseIntegration(t *testing.T) {
	ctx := t.Context()
	db, dsn := hotReadIntegrationDatabase(t, true)
	legacy, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacy.Close)
	if _, err := db.ExecContext(ctx, `ALTER TABLE events_columnstore ADD COLUMN event_document_key AS (SHA2('wrong',256)) PERSISTED VARCHAR(64)`); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, dsn)
	if opened != nil {
		opened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "incompatible hot-read computed column") {
		t.Fatalf("expected incompatible definition error, got %v", err)
	}
}

func TestHotReadLayoutFreshDatabaseIntegration(t *testing.T) {
	db, dsn := hotReadIntegrationDatabase(t, false)
	before := hotReadShowCreate(t, db, "events_columnstore")
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if after := hotReadShowCreate(t, db, "events_columnstore"); after != before {
		t.Fatalf("startup changed operator-owned events layout:\nbefore: %s\nafter: %s", before, after)
	}
	for _, index := range hotReadIndexes {
		if index.column != "" && !s.hotReadIndexReady(index.name) {
			t.Errorf("fresh lookup %s not ready", index.name)
		}
	}
}

func hotReadColumnCount(t *testing.T, db *sql.DB, table, like string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name LIKE ?`, table, like).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An events table prepared by the old operator script has no hot-read columns.
// Startup must neither add them nor treat those lookups as ready, while the
// Conveyor-created core tables are still prepared on a fresh bootstrap.
func TestHotReadLayoutOldOperatorEventsIntegration(t *testing.T) {
	ctx := t.Context()
	db, dsn := hotReadIntegrationDatabase(t, false)
	for _, statement := range []string{
		`ALTER TABLE events_columnstore DROP KEY events_document_key_idx`,
		`ALTER TABLE events_columnstore DROP KEY events_requirement_key_idx`,
		`ALTER TABLE events_columnstore DROP KEY events_session_key_idx`,
		`ALTER TABLE events_columnstore DROP COLUMN event_document_key`,
		`ALTER TABLE events_columnstore DROP COLUMN event_requirement_key`,
		`ALTER TABLE events_columnstore DROP COLUMN event_session_key`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	before := hotReadShowCreate(t, db, "events_columnstore")
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if after := hotReadShowCreate(t, db, "events_columnstore"); after != before {
		t.Fatalf("startup changed operator-owned events layout:\nbefore: %s\nafter: %s", before, after)
	}
	if n := hotReadColumnCount(t, db, "events_columnstore", "event_document_key") + hotReadColumnCount(t, db, "events_columnstore", "event_requirement_key") + hotReadColumnCount(t, db, "events_columnstore", "event_session_key"); n != 0 {
		t.Fatalf("startup added %d event hot-read columns to an operator-owned table", n)
	}
	for _, index := range hotReadIndexes {
		if index.column == "" {
			continue
		}
		if want := index.table != "events_columnstore"; s.hotReadIndexReady(index.name) != want {
			t.Errorf("lookup %s ready = %v, want %v", index.name, !want, want)
		}
	}
}

// A persisted per-table record decides, not the current schema: a table
// recorded as pre-existing gets no startup DDL while its siblings are prepared.
func TestHotReadLayoutMixedProvenanceIntegration(t *testing.T) {
	ctx := t.Context()
	db, dsn := hotReadIntegrationDatabase(t, false)
	for _, statement := range []string{
		`CREATE ROWSTORE TABLE conveyor_singlestore_bootstrap (id INT NOT NULL,origin VARCHAR(16) NOT NULL,PRIMARY KEY(id),SHARD KEY(id))`,
		`INSERT INTO conveyor_singlestore_bootstrap(id,origin) VALUES (1,'fresh'),(2,'legacy'),(3,'fresh'),(4,'fresh')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if n := hotReadColumnCount(t, db, "tasks", "task_state_key") + hotReadColumnCount(t, db, "tasks", "task_intake_key"); n != 0 {
		t.Fatalf("legacy-recorded tasks table received %d hot-read columns", n)
	}
	for _, index := range hotReadIndexes {
		if index.column == "" {
			continue
		}
		if want := index.table != "tasks"; s.hotReadIndexReady(index.name) != want {
			t.Errorf("lookup %s ready = %v, want %v", index.name, !want, want)
		}
	}
}
