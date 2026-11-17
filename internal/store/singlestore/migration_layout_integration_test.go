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
	if _, err := db.ExecContext(ctx, legacyEventLogDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO event_log (workspace_id,position,stream_id,version,kind,payload,at) VALUES ('legacy',1,'task/one',1,'created','{}','2020-01-01')`); err != nil {
		t.Fatal(err)
	}
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
		"event rows": `SELECT workspace_id,position,stream_id,version,kind,actor_id,actor_role,payload,at FROM event_log`,
	}
	before := make(map[string][]string, len(queries))
	for key, query := range queries {
		before[key] = queryRows(t, published, ctx, query)
	}
	for range 2 {
		if err := published.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for key, query := range queries {
			if got := queryRows(t, published, ctx, query); !reflect.DeepEqual(got, before[key]) {
				t.Errorf("published database %s changed after startup\nbefore: %v\nafter: %v", key, before[key], got)
			}
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
		"events_columnstore": true, "event_log": true,
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
	for _, table := range []string{"event_log_positions", "event_log_streams", "event_log_job_index"} {
		if !seen[table] {
			t.Errorf("missing rowstore support table %s", table)
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
	for _, index := range []struct {
		name, typ, columns string
		nonUnique          int
	}{
		{"PRIMARY", "HASH", "workspace_id,position", 0},
		{"event_log_stream_version", "HASH", "workspace_id,stream_id,version", 1},
	} {
		var columns string
		var nonUnique int
		err := fresh.db.QueryRowContext(ctx, `SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ','), MIN(non_unique) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='event_log' AND index_name=? AND index_type=?`, index.name, index.typ).Scan(&columns, &nonUnique)
		if err != nil || columns != index.columns || nonUnique != index.nonUnique {
			t.Errorf("event_log index %s: columns=%q non_unique=%d error=%v", index.name, columns, nonUnique, err)
		}
	}
	for _, index := range []struct{ typ, columns string }{
		{"SHARD", "workspace_id"},
		{"CLUSTERED COLUMNSTORE", "workspace_id,position"},
	} {
		var columns string
		err := fresh.db.QueryRowContext(ctx, `SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',') FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='event_log' AND index_type=?`, index.typ).Scan(&columns)
		if err != nil || columns != index.columns {
			t.Errorf("event_log %s: columns=%q error=%v", index.typ, columns, err)
		}
	}
	const keys = `SELECT table_name,index_name,non_unique,seq_in_index,column_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name <> 'event_log' AND index_type NOT IN ('CLUSTERED COLUMNSTORE','SHARD')`
	if a, b := queryRows(t, published, ctx, keys), queryRows(t, fresh, ctx, keys); !reflect.DeepEqual(a, b) {
		t.Errorf("published and fresh keys differ\npublished: %v\nfresh: %v", a, b)
	}
	// SingleStore assigns different generated names to rowstore and columnstore shard keys.
	const shards = `SELECT table_name,non_unique,index_type,seq_in_index,column_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND index_type='SHARD'`
	if a, b := queryRows(t, published, ctx, shards), queryRows(t, fresh, ctx, shards); !reflect.DeepEqual(a, b) {
		t.Errorf("published and fresh shard keys differ\npublished: %v\nfresh: %v", a, b)
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

const legacyEventLogDDL = `CREATE ROWSTORE TABLE event_log (
 workspace_id VARCHAR(255) NOT NULL, position BIGINT NOT NULL,
 stream_id VARCHAR(768) NOT NULL, version BIGINT NOT NULL, kind VARCHAR(255) NOT NULL,
 actor_id VARCHAR(255) NOT NULL DEFAULT '', actor_role VARCHAR(255) NOT NULL DEFAULT '',
 payload JSON NOT NULL, at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, position), UNIQUE KEY event_log_stream_version (workspace_id,stream_id,version),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`

func TestExistingColumnstoreEventLogPreservedIntegration(t *testing.T) {
	integrationStore(t)
	ctx := t.Context()
	name := fmt.Sprintf("conveyor_column_history_%d_test", time.Now().UnixNano())
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
	if _, err := db.ExecContext(ctx, `CREATE TABLE event_log (
 workspace_id VARCHAR(255) NOT NULL, position BIGINT NOT NULL,
 stream_id VARCHAR(768) NOT NULL, version BIGINT NOT NULL, kind VARCHAR(255) NOT NULL,
 actor_id VARCHAR(255) NOT NULL DEFAULT '', actor_role VARCHAR(255) NOT NULL DEFAULT '',
 payload JSON NOT NULL, at DATETIME(6) NOT NULL,
 SORT KEY (workspace_id, position), PRIMARY KEY (workspace_id, position) USING HASH,
 KEY event_log_stream_version (workspace_id,stream_id,version) USING HASH,
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO event_log (workspace_id,position,stream_id,version,kind,payload,at) VALUES ('converted',1,'task/one',1,'created','{}','2020-01-01')`); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	queries := map[string]string{
		"table":   `SELECT storage_type,create_options FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='event_log'`,
		"columns": `SELECT column_name,ordinal_position,column_default,is_nullable,data_type,column_type,character_set_name,collation_name,extra FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='event_log'`,
		"keys":    `SELECT index_name,non_unique,seq_in_index,column_name,index_type FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='event_log'`,
		"rows":    `SELECT workspace_id,position,stream_id,version,kind,actor_id,actor_role,payload,at FROM event_log`,
	}
	before := make(map[string][]string, len(queries))
	for key, query := range queries {
		before[key] = queryRows(t, st, ctx, query)
	}
	for range 2 {
		if err := st.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for key, query := range queries {
			if got := queryRows(t, st, ctx, query); !reflect.DeepEqual(got, before[key]) {
				t.Errorf("converted database %s changed after startup\nbefore: %v\nafter: %v", key, before[key], got)
			}
		}
		if _, ok := queries["ledger"]; !ok {
			queries["ledger"] = `SELECT version,name,checksum,applied_at FROM conveyor_singlestore_migrations`
			before["ledger"] = queryRows(t, st, ctx, queries["ledger"])
		}
	}
}
