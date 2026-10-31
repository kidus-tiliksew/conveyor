package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Preserve audit history, nullable workspace events, cursor IDs and payloads
// above one MiB across upgrades and each non-transactional rename boundary.
func TestEventsColumnstoreMigrationIntegration(t *testing.T) {
	for _, state := range []string{"upgrade", "partial-copy", "source-renamed", "promoted", "backup-dropped"} {
		t.Run(state, func(t *testing.T) {
			st := integrationStore(t)
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "events-upgrade")
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := st.db.ExecContext(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			if storage, err := st.eventTableStorage(ctx, "events"); err != nil || storage != "COLUMNSTORE" {
				t.Fatalf("fresh events storage=%q err=%v", storage, err)
			}
			if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "events-upgrade", Repos: []config.Repo{{Name: "repo", Base: "main"}}}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			task := core.Task{ID: "events-upgrade-task", Workspace: "events-upgrade", Repo: "repo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}

			// Reconstruct the published pre-0027 table, not a guessed equivalent.
			schema, err := migrationFiles.ReadFile("migrations/0001_schema.sql")
			if err != nil {
				t.Fatal(err)
			}
			start := strings.Index(string(schema), "CREATE ROWSTORE TABLE IF NOT EXISTS `events`")
			if start < 0 {
				t.Fatal("old events DDL missing")
			}
			ddl := strings.SplitN(string(schema)[start:], ";", 2)[0]
			exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=27`)
			t.Cleanup(func() {
				recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				defer cancel()
				if err := st.migrate(recovery); err != nil {
					t.Errorf("restore migrated fixture: %v", err)
				}
			})
			exec(`DROP TABLE events`)
			exec(ddl)
			for i := range 260 {
				payload := fmt.Sprintf(`{"row":%d}`, i)
				if i == 0 {
					payload = `{"body":"` + strings.Repeat("x", (1<<20)+17) + `"}`
				}
				exec(`INSERT INTO events (`+eventColumns+`) VALUES(?,?,NULL,'test.migrated','fixture','system',?,?,?)`, int64(500000+i), task.ID, payload, now.Add(time.Duration(i/2)*time.Microsecond), task.Workspace)
			}
			// NULL task IDs and an equal explicit ID in another workspace must
			// survive the range copy without being collapsed or duplicated.
			exec(`INSERT INTO events (`+eventColumns+`) VALUES(500000,NULL,NULL,'workspace.test','fixture','system','{"workspace":true}',?, 'other-workspace')`, now)
			legacyRows, err := documentRows(ctx, st.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events WHERE workspace_id=? AND task_id=? ORDER BY at,id`, task.Workspace, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := scanDocumentEvents(legacyRows)
			if err != nil {
				t.Fatal(err)
			}
			migration, err := migrationFiles.ReadFile("migrations/0027_events_columnstore.sql")
			if err != nil {
				t.Fatal(err)
			}
			if state != "upgrade" {
				exec(string(migration))
				exec(`INSERT INTO events_columnstore_0027 (` + eventColumns + `,event_shard_key) SELECT ` + eventColumns + `,COALESCE(task_id,CONCAT('event/',CAST(id AS CHAR))) FROM events WHERE id<=500100`)
			}
			if state == "source-renamed" || state == "promoted" || state == "backup-dropped" {
				exec(`INSERT INTO events_columnstore_0027 (` + eventColumns + `,event_shard_key) SELECT ` + eventColumns + `,COALESCE(task_id,CONCAT('event/',CAST(id AS CHAR))) FROM events WHERE id>500100`)
				exec(`ALTER TABLE events RENAME TO events_rowstore_0027`)
			}
			if state == "promoted" || state == "backup-dropped" {
				exec(`ALTER TABLE events_columnstore_0027 RENAME TO events`)
			}
			if state == "backup-dropped" {
				exec(`DROP TABLE events_rowstore_0027`)
			}
			if err := st.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := st.migrate(ctx); err != nil {
				t.Fatalf("ledger replay: %v", err)
			}
			after, err := st.ListEvents(ctx, task.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("migration changed task history: before=%d after=%d err=%v", len(before), len(after), err)
			}
			var count, workspaceEvents int
			if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*),SUM(task_id IS NULL AND workspace_id='other-workspace') FROM events`).Scan(&count, &workspaceEvents); err != nil || count != 261 || workspaceEvents != 1 {
				t.Fatalf("copied rows=%d workspace events=%d err=%v", count, workspaceEvents, err)
			}
			for _, table := range []string{"events_columnstore_0027", "events_rowstore_0027"} {
				if storage, err := st.eventTableStorage(ctx, table); err != nil || storage != "" {
					t.Fatalf("staging table %s remains: storage=%q err=%v", table, storage, err)
				}
			}
			cursorEvents, err := st.ListEventsAfter(ctx, task.ID, 500256)
			if err != nil || !reflect.DeepEqual(after[257:], cursorEvents) {
				t.Fatalf("ID cursor history changed: %+v err=%v", cursorEvents, err)
			}
			window, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Limit: 3, MaxBytes: store.TaskEventWindowMaxBytes, Descending: true})
			if err != nil || !reflect.DeepEqual(after[len(after)-3:], window.Events) {
				t.Fatalf("timeline window changed: events=%d err=%v", len(window.Events), err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "test.after-migration", Payload: json.RawMessage(`{"new":true}`), At: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			newEvents, err := st.ListEventsAfter(ctx, task.ID, 500259)
			if err != nil || len(newEvents) != 1 || newEvents[0].ID <= 500259 || newEvents[0].Kind != "test.after-migration" {
				t.Fatalf("AUTO_INCREMENT cursor did not advance: %+v err=%v", newEvents, err)
			}
		})
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
			if err := insertWorkspaceEvent(ctx, tx, core.Event{Kind: "test.taskless"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var count, keys, nullTasks int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT event_shard_key),SUM(task_id IS NULL) FROM events WHERE workspace_id=? AND kind='test.taskless'`, "taskless-events").Scan(&count, &keys, &nullTasks); err != nil {
		t.Fatal(err)
	}
	if count != 64 || keys != 64 || nullTasks != 64 {
		t.Fatalf("task-less rows=%d independent keys=%d NULL task IDs=%d", count, keys, nullTasks)
	}
}
