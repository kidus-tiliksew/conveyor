package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/logtest"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The job index on PostgreSQL (component-durable-queue, The job index;
// req-quiet-idle-runtime AC-5.1, AC-5.2). These run in the CI PostgreSQL
// store shard, which does not run internal/eventlog/pglog. The runtime's
// start on 100,000 finished jobs runs in the daemon shard
// (internal/dispatch/logqueue_integration_test.go).

// isolatedPool opens a pool on a fresh schema; tracer, when set, sees every
// statement.
func isolatedPool(t *testing.T, prefix string, tracer pgx.QueryTracer) (*pgxpool.Pool, string) {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := prefix + "_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	return schemaPool(t, databaseURL, schema, tracer), schema
}

func schemaPool(t *testing.T, databaseURL, schema string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPglogConformanceIntegration runs the log conformance suite, with its
// job_index_matches_fold case, on pglog. Every case names its own
// workspaces, so the cases share one schema.
func TestPglogConformanceIntegration(t *testing.T) {
	pool, _ := isolatedPool(t, "pglog_conformance", nil)
	if err := pglog.EnsureSchema(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	log := pglog.New(pool)
	logtest.Run(t, func(*testing.T) eventlog.Store { return log })
}

// jobScenario appends job streams in every state, including multiple
// generations, to one workspace.
func jobScenario(t *testing.T, ctx context.Context, log eventlog.Store, ws string) {
	t.Helper()
	base := time.Date(2026, 11, 1, 12, 0, 0, 123456789, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	put := func(key string, expected eventlog.Version, kind string, payload any) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(ctx, ws, jobindex.StreamFor("demo", key), expected, []eventlog.NewEvent{{Kind: kind, ActorID: "test", ActorRole: "system", Payload: raw, At: at(0)}}); err != nil {
			t.Fatal(err)
		}
	}
	enq := func(key string, v eventlog.Version, scheduled time.Time) {
		put(key, v, jobindex.KindEnqueued, jobindex.EnqueuedPayload{Kind: "demo", Key: key, Args: json.RawMessage(`{"key":"` + key + `"}`), MaxAttempts: 3, ScheduledAt: scheduled})
	}
	claim := func(key string, v eventlog.Version, attempt, minute int) {
		put(key, v, jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: attempt, Worker: "w", ClaimedAt: at(minute)})
	}
	enq("available", 0, time.Time{})
	enq("scheduled-enqueue", 0, base.Add(24*time.Hour))
	enq("retried", 0, time.Time{})
	claim("retried", 1, 1, 1)
	put("retried", 2, jobindex.KindFailed, jobindex.OutcomePayload{Attempt: 1, Error: "boom", NextAt: at(2)})
	enq("snoozed", 0, time.Time{})
	claim("snoozed", 1, 1, 1)
	put("snoozed", 2, jobindex.KindSnoozed, jobindex.OutcomePayload{Attempt: 1, Until: at(3)})
	enq("rescued", 0, time.Time{})
	claim("rescued", 1, 1, 1)
	put("rescued", 2, jobindex.KindRescued, jobindex.OutcomePayload{Attempt: 1, Error: "rescued", NextAt: at(4)})
	enq("running", 0, time.Time{})
	claim("running", 1, 1, 5)
	enq("completed", 0, time.Time{})
	claim("completed", 1, 1, 1)
	put("completed", 2, jobindex.KindCompleted, jobindex.OutcomePayload{Attempt: 1})
	enq("discarded", 0, time.Time{})
	claim("discarded", 1, 3, 1)
	put("discarded", 2, jobindex.KindDiscarded, jobindex.OutcomePayload{Attempt: 3, Error: "fatal"})
	enq("generations", 0, time.Time{})
	claim("generations", 1, 1, 1)
	put("generations", 2, jobindex.KindCompleted, jobindex.OutcomePayload{Attempt: 1})
	enq("generations", 3, time.Time{})
	claim("generations", 4, 1, 2)
	put("generations", 5, jobindex.KindFailed, jobindex.OutcomePayload{Attempt: 1, Error: "boom", NextAt: at(3)})
	claim("generations", 6, 2, 3)
	put("generations", 7, jobindex.KindDiscarded, jobindex.OutcomePayload{Attempt: 2, Error: "fatal"})
	enq("generations", 8, time.Time{})
	if _, err := log.Append(ctx, ws, "job/odd:note", eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "note"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, ws, "task/not-a-job", eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "task.created"}}); err != nil {
		t.Fatal(err)
	}
}

// assertIndexMatchesFold checks every stream of the workspaces: a job
// stream's row equals a full fold, and no other stream has a row.
func assertIndexMatchesFold(t *testing.T, ctx context.Context, pool *pgxpool.Pool, log *pglog.Store, workspaces ...string) {
	t.Helper()
	for _, ws := range workspaces {
		rows, err := pool.Query(ctx, `SELECT stream_id FROM event_log_streams WHERE workspace_id = $1 AND version > 0 ORDER BY stream_id`, ws)
		if err != nil {
			t.Fatal(err)
		}
		var streams []string
		for rows.Next() {
			var stream string
			if err := rows.Scan(&stream); err != nil {
				t.Fatal(err)
			}
			streams = append(streams, stream)
		}
		rows.Close()
		jobs := 0
		for _, name := range streams {
			stream := eventlog.StreamID(name)
			row, ok, err := log.JobIndexRow(ctx, ws, stream)
			if err != nil {
				t.Fatal(err)
			}
			if !jobindex.IsJobStream(stream) {
				if ok {
					t.Fatalf("%s %s is not a job stream but has row %+v", ws, stream, row)
				}
				continue
			}
			jobs++
			events, err := log.Read(ctx, ws, stream, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			job, err := jobindex.Fold(ws, stream, events)
			if err != nil {
				t.Fatal(err)
			}
			if want := jobindex.RowOf(job); !ok || !sameIndexRow(row, want) {
				t.Fatalf("%s %s row=%+v (present %v), full fold=%+v", ws, stream, row, ok, want)
			}
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_log_job_index WHERE workspace_id = $1`, ws).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != jobs {
			t.Fatalf("%s has %d index rows for %d job streams", ws, count, jobs)
		}
	}
}

func sameIndexRow(a, b jobindex.Row) bool {
	return a.Workspace == b.Workspace && a.Stream == b.Stream && a.Kind == b.Kind && a.State == b.State &&
		a.Generation == b.Generation && a.Version == b.Version && a.NextAt.Equal(b.NextAt) && a.ClaimedAt.Equal(b.ClaimedAt)
}

// TestMigration155FillsJobIndexIntegration: a schema at version 154 holds
// job streams in every state, with no index row or a stale one, as before
// the upgrade. Migration 155 fills one row per job stream equal to a full
// fold; a rerun of the fill writes the same rows and never moves a newer
// row back.
func TestMigration155FillsJobIndexIntegration(t *testing.T) {
	pool, _ := isolatedPool(t, "migration_v155", nil)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	if err := migrateControlPlaneToVersion(ctx, pool, 154); err != nil {
		t.Fatalf("migrate to 154: %v", err)
	}
	log := pglog.New(pool)
	first, second := "fill-a-"+core.NewTaskID(), "fill-b-"+core.NewTaskID()
	jobScenario(t, ctx, log, first)
	jobScenario(t, ctx, log, second)
	// Before 155 no binary wrote the index: drop every row, and leave one
	// stale row behind as a partial earlier attempt would.
	if _, err := pool.Exec(ctx, `DELETE FROM event_log_job_index`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO event_log_job_index (workspace_id, stream_id, kind, state, generation, version) VALUES ($1, 'job/demo:completed', 'demo', 'available', 1, 1)`, first); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_log`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate through 155: %v", err)
	}
	var applied bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conveyor_schema_migrations WHERE version = 155 AND name = '155_event_log_job_index.sql')`).Scan(&applied); err != nil || !applied {
		t.Fatalf("migration 155 recorded=%v err=%v", applied, err)
	}
	assertIndexMatchesFold(t, ctx, pool, log, first, second)
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_log`).Scan(&after); err != nil || after != events {
		t.Fatalf("event_log rows %d -> %d err=%v: the fill must not write events", events, after, err)
	}
	_, unfinished, err := log.UnfinishedJobs(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, row := range unfinished {
		names = append(names, string(row.Stream)+"="+string(row.State))
	}
	sort.Strings(names)
	want := []string{"job/demo:available=available", "job/demo:generations=available", "job/demo:rescued=scheduled", "job/demo:retried=scheduled", "job/demo:running=running", "job/demo:scheduled-enqueue=scheduled", "job/demo:snoozed=scheduled"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("unfinished after fill=%v, want %v", names, want)
	}

	// A rerun writes the same rows and never moves a newer row back.
	if _, err := pool.Exec(ctx, `UPDATE event_log_job_index SET version = 999, state = 'completed' WHERE workspace_id = $1 AND stream_id = 'job/demo:available'`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := pglog.FillJobIndex(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := log.JobIndexRow(ctx, second, "job/demo:available"); row.Version != 999 {
		t.Fatalf("fill moved a newer row back to %+v", row)
	}
	assertIndexMatchesFold(t, ctx, pool, log, first)
}

// TestJobIndexSharesTheAppendTransactionIntegration: an enqueue inside a
// caller's transaction that rolls back leaves neither event nor row; the
// committed enqueue writes both; a suppressed duplicate enqueue writes
// neither; and every runtime outcome append keeps the row equal to a fold.
func TestJobIndexSharesTheAppendTransactionIntegration(t *testing.T) {
	pool, _ := isolatedPool(t, "job_index_tx", nil)
	ctx := t.Context()
	if err := pglog.EnsureSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	log := pglog.New(pool)
	ws := "tx-" + core.NewTaskID()
	stream := logqueue.StreamFor("demo", "k")
	enqueue := func(ctx context.Context) bool {
		t.Helper()
		inserted, err := logqueue.Enqueue(ctx, log, ws, "demo", "k", map[string]string{"key": "k"}, 3, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return inserted
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !enqueue(pglog.WithTx(ctx, tx)) {
		t.Fatal("enqueue inside the transaction was not inserted")
	}
	if row, ok, err := log.JobIndexRow(pglog.WithTx(ctx, tx), ws, stream); err != nil || !ok || row.State != jobindex.StateAvailable {
		t.Fatalf("row inside the transaction=%+v ok=%v err=%v", row, ok, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if head, _ := log.Head(ctx, ws, stream); head != 0 {
		t.Fatalf("head after rollback=%d", head)
	}
	if _, ok, err := log.JobIndexRow(ctx, ws, stream); err != nil || ok {
		t.Fatalf("row after rollback ok=%v err=%v, want none", ok, err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !enqueue(pglog.WithTx(ctx, tx)) {
		t.Fatal("enqueue was not inserted")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if enqueue(ctx) {
		t.Fatal("a duplicate enqueue of an active job was inserted")
	}
	if row, ok, _ := log.JobIndexRow(ctx, ws, stream); !ok || row.Version != 1 || row.Generation != 1 {
		t.Fatalf("row after a suppressed enqueue=%+v", row)
	}

	// The runtime's claim, retry, and completion keep the row on the fold.
	attempts := 0
	rt := logqueue.NewRuntime(log, logqueue.Options{Workspaces: []string{ws}})
	done := make(chan struct{})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("first attempt fails")
		}
		if attempts == 2 {
			return &queue.SnoozeError{Duration: 10 * time.Millisecond}
		}
		close(done)
		return nil
	}, RetryDelay: func(int) time.Duration { return 10 * time.Millisecond }})
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the job did not complete")
	}
	stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Stop(stop); err != nil {
		t.Fatal(err)
	}
	row, _, _ := log.JobIndexRow(ctx, ws, stream)
	events, _ := log.Read(ctx, ws, stream, 0, 0)
	job, _ := jobindex.Fold(ws, stream, events)
	if !sameIndexRow(row, jobindex.RowOf(job)) || row.State != jobindex.StateCompleted {
		t.Fatalf("row after outcomes=%+v, fold=%+v", row, jobindex.RowOf(job))
	}
}

// TestRepairJobIndexRebuildsStaleRowsIntegration: rows that an older
// binary left missing or behind are rebuilt by RepairJobIndex, which then
// finds nothing to repair.
func TestRepairJobIndexRebuildsStaleRowsIntegration(t *testing.T) {
	pool, _ := isolatedPool(t, "job_index_repair", nil)
	ctx := t.Context()
	if err := pglog.EnsureSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	log := pglog.New(pool)
	ws := "repair-" + core.NewTaskID()
	jobScenario(t, ctx, log, ws)
	if _, err := pool.Exec(ctx, `DELETE FROM event_log_job_index WHERE workspace_id = $1 AND stream_id = 'job/demo:available'`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE event_log_job_index SET version = 1, state = 'available' WHERE workspace_id = $1 AND stream_id = 'job/demo:completed'`, ws); err != nil {
		t.Fatal(err)
	}
	repaired, err := log.RepairJobIndex(ctx, ws)
	if err != nil || repaired != 2 {
		t.Fatalf("repaired=%d err=%v, want 2", repaired, err)
	}
	assertIndexMatchesFold(t, ctx, pool, log, ws)
	if repaired, err := log.RepairJobIndex(ctx, ws); err != nil || repaired != 0 {
		t.Fatalf("second repair=%d err=%v, want 0", repaired, err)
	}
}
