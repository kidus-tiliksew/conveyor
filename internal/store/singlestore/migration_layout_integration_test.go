package singlestore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestHistoryMigrationLayoutsIntegration(t *testing.T) {
	fresh := integrationStore(t)
	ctx := t.Context()
	name := fmt.Sprintf("conveyor_history_%d_test", time.Now().UnixNano())
	if _, err := sharedDatabase.admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` PARTITIONS 2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := sharedDatabase.admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("drop test database: %v", err)
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
	if err := execEventsOperatorScript(ctx, db, "01-create.sql"); err != nil {
		t.Fatal(err)
	}
	published := &Store{db: db}
	files, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	for i := range files {
		if checksum, ok := publishedChecksums[files[i].version]; ok {
			data, err := os.ReadFile(filepath.Join("testdata", "published_migrations", files[i].name))
			if err != nil {
				t.Fatal(err)
			}
			files[i].sql = string(data)
			files[i].checksum = checksum
		}
	}
	if err := published.migrateFiles(ctx, files); err != nil {
		t.Fatal(err)
	}
	queries := map[string]string{
		"ledger":     `SELECT version,name,checksum,applied_at FROM conveyor_singlestore_migrations`,
		"tables":     `SELECT table_name,table_type,storage_type,create_options FROM information_schema.tables WHERE table_schema=DATABASE()`,
		"columns":    `SELECT table_name,column_name,ordinal_position,column_default,is_nullable,data_type,column_type,character_set_name,collation_name,extra FROM information_schema.columns WHERE table_schema=DATABASE()`,
		"statistics": `SELECT table_name,index_name,non_unique,seq_in_index,column_name,index_type FROM information_schema.statistics WHERE table_schema=DATABASE()`,
	}
	before := make(map[string][]string, len(queries))
	for key, query := range queries {
		before[key] = queryRows(t, published, ctx, query)
	}
	if err := published.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for key, query := range queries {
		if got := queryRows(t, published, ctx, query); !reflect.DeepEqual(got, before[key]) {
			t.Errorf("published database %s changed after startup\nbefore: %v\nafter: %v", key, before[key], got)
		}
	}
	columnstore := map[string]bool{
		"artifact_links": true, "deployment_events": true, "interrupted_review_recoveries": true,
		"interventions": true, "legacy_spec_gate_versions": true, "lineage_repair_exclusions": true,
		"links": true, "monitor_activity": true, "monitor_observations": true, "planning_messages": true,
		"reference_document_versions": true, "task_context_proposals": true, "task_dependency_additions": true,
		"task_dependency_removals": true, "task_setup_changes": true, "transcripts": true,
		"work_order_preemptions": true, "work_order_recoveries": true, "artifact_metadata_repairs": true,
		"documentation_gate_evidence": true, "review_seat_additions": true, "github_webhook_deliveries": true,
		"task_risk_approvals": true, "work_order_claim_identities": true, "governance_snapshots": true,
		"events_columnstore": true,
	}
	rows, err := fresh.db.QueryContext(ctx, `SELECT table_name,storage_type FROM information_schema.tables WHERE table_schema=DATABASE()`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var table, storage string
		if err := rows.Scan(&table, &storage); err != nil {
			t.Fatal(err)
		}
		seen[table] = true
		want := "INMEMORY_ROWSTORE"
		if columnstore[table] {
			want = "COLUMNSTORE"
		}
		if storage != want {
			t.Errorf("%s storage = %s, want %s", table, storage, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for table := range columnstore {
		if !seen[table] {
			t.Errorf("missing table %s", table)
		}
	}
	for table := range columnstore {
		if table == "governance_snapshots" || table == "events_columnstore" {
			continue
		}
		var storage string
		if err := published.db.QueryRowContext(ctx, `SELECT storage_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&storage); err != nil || storage != "INMEMORY_ROWSTORE" {
			t.Errorf("published %s storage = %s, error %v", table, storage, err)
		}
	}
	const keys = `SELECT table_name,index_name,non_unique,seq_in_index,column_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND index_type <> 'CLUSTERED COLUMNSTORE'`
	if a, b := queryRows(t, published, ctx, keys), queryRows(t, fresh, ctx, keys); !reflect.DeepEqual(a, b) {
		t.Errorf("published and fresh keys differ\npublished: %v\nfresh: %v", a, b)
	}
	link := `INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind) VALUES ('history-proof','task','a','task','b','related')`
	if _, err := fresh.db.ExecContext(ctx, link); err != nil {
		t.Fatal(err)
	}
	assertDuplicate := func(query string, args ...any) {
		t.Helper()
		_, err := fresh.db.ExecContext(ctx, query, args...)
		var duplicate *mysql.MySQLError
		if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
			t.Errorf("expected duplicate key 1062, got %v", err)
		}
	}
	assertDuplicate(link)
	const contextInsert = `INSERT INTO verification_contexts (workspace_id,id,task_id,context_id,logical_key,key_hash,body) VALUES ('history-proof',?,'task','context','key',?,'{}')`
	if _, err := fresh.db.ExecContext(ctx, contextInsert, "one", "hash-one"); err != nil {
		t.Fatal(err)
	}
	assertDuplicate(contextInsert, "one", "hash-two")
	assertDuplicate(contextInsert, "two", "hash-one")
}
