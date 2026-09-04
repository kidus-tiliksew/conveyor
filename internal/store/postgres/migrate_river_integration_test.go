package postgres

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// TestMigration121MovesRiverJobsOntoTheLogIntegration: a database that
// still holds River's tables upgrades through 121 with every active job
// re-enqueued on its log stream and the tables gone. Finished jobs are not
// carried over, and a job without an identity is skipped.
func TestMigration121MovesRiverJobsOntoTheLogIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "migration_v121_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(t.Context(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
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
	defer pool.Close()
	if err = migrateControlPlaneToVersion(t.Context(), pool, 120); err != nil {
		t.Fatalf("migrate isolated schema to version 120: %v", err)
	}
	// The shape River's migrator left behind, reduced to the columns the
	// conversion reads.
	if _, err = pool.Exec(t.Context(), `
CREATE TABLE river_job (id bigserial PRIMARY KEY, kind text NOT NULL, args jsonb NOT NULL, state text NOT NULL, max_attempts int NOT NULL);
CREATE TABLE river_leader (name text PRIMARY KEY);
CREATE TABLE river_queue (name text PRIMARY KEY);
CREATE TABLE river_client (id text PRIMARY KEY);
CREATE TABLE river_client_queue (client_id text, name text);
CREATE TABLE river_migration (id bigserial PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	workspace := "migration-v121-" + core.NewTaskID()
	rows := []struct {
		kind, state string
		args        string
		max         int
	}{
		{"dispatch_task", "available", fmt.Sprintf(`{"workspace_id":%q,"task_id":"queued-task"}`, workspace), queue.DispatchTaskMaxAttempts},
		{"dispatch_task", "running", fmt.Sprintf(`{"workspace_id":%q,"task_id":"running-task"}`, workspace), queue.DispatchTaskMaxAttempts},
		{"dispatch_task", "completed", fmt.Sprintf(`{"workspace_id":%q,"task_id":"done-task"}`, workspace), queue.DispatchTaskMaxAttempts},
		{"dispatch_task", "discarded", fmt.Sprintf(`{"workspace_id":%q,"task_id":"dead-task"}`, workspace), queue.DispatchTaskMaxAttempts},
		{"review_publication", "retryable", fmt.Sprintf(`{"workspace_id":%q,"review_work_order_id":"wo-review"}`, workspace), 5},
		{"github_issue_publication", "scheduled", fmt.Sprintf(`{"workspace_id":%q,"task_id":"issue-task"}`, workspace), 5},
		{"order_clock", "available", fmt.Sprintf(`{"workspace_id":%q}`, workspace), 1},
	}
	for _, row := range rows {
		if _, err = pool.Exec(t.Context(), `INSERT INTO river_job (kind, args, state, max_attempts) VALUES ($1, $2::jsonb, $3, $4)`, row.kind, row.args, row.state, row.max); err != nil {
			t.Fatal(err)
		}
	}

	if err = migrateControlPlaneToVersion(t.Context(), pool, 121); err != nil {
		t.Fatalf("migrate to version 121: %v", err)
	}
	var riverLeft bool
	if err = pool.QueryRow(t.Context(), `SELECT to_regclass('river_job') IS NOT NULL`).Scan(&riverLeft); err != nil {
		t.Fatal(err)
	}
	if riverLeft {
		t.Fatal("river_job survived migration 121")
	}
	st := newStore(pool)
	want := map[string]bool{
		"job/dispatch_task:queued-task":           true,
		"job/dispatch_task:running-task":          true,
		"job/dispatch_task:done-task":             false,
		"job/dispatch_task:dead-task":             false,
		"job/review_publication:wo-review":        true,
		"job/github_issue_publication:issue-task": true,
		"job/order_clock:":                        false,
	}
	for stream, active := range want {
		job, err := logqueue.Load(t.Context(), st.Log(), workspace, eventlog.StreamID(stream))
		if err != nil {
			t.Fatal(err)
		}
		if job.Active() != active {
			t.Fatalf("%s active=%t want %t (job=%+v)", stream, job.Active(), active, job)
		}
		if active && (job.State != logqueue.StateAvailable || job.Attempt != 0 || job.ScheduledAt.After(time.Now().Add(time.Minute))) {
			t.Fatalf("%s job=%+v, want available at attempt 0", stream, job)
		}
	}
	queued, err := logqueue.Load(t.Context(), st.Log(), workspace, eventlog.StreamID("job/dispatch_task:queued-task"))
	if err != nil || queued.MaxAttempts != queue.DispatchTaskMaxAttempts || !strings.Contains(string(queued.Args), `"queued-task"`) {
		t.Fatalf("converted job=%+v err=%v", queued, err)
	}
	// Running the migration path again is a no-op: 121 is recorded and the
	// table is gone.
	if err = migrateControlPlaneToVersion(t.Context(), pool, 121); err != nil {
		t.Fatalf("second run: %v", err)
	}
}
