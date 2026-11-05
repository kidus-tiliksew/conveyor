package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

// TestLogQueueShutdownInterruptionPreservesAttemptIntegration: the
// dispatcher's handler runs from a job on the log, a hard stop interrupts
// it, and the job is left scheduled with its attempt handed back rather
// than failed.
func TestLogQueueShutdownInterruptionPreservesAttemptIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(store.WithActor(t.Context(), store.SystemActor()), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "logqueue-interrupt-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Log queue interrupt "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	task := core.Task{
		ID: "logqueue-interrupt-" + suffix, Workspace: workspace, Repo: "repo", Title: "Interrupt dispatch",
		BaseBranch: "main", Branch: "conveyor/logqueue-interrupt-" + suffix,
		State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	stream := logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), task.ID)
	before, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil || before.State != logqueue.StateAvailable {
		t.Fatalf("job before dispatch=%+v err=%v", before, err)
	}

	blocking := &blockingDispatchStore{Store: st, taskID: task.ID, started: make(chan struct{})}
	dispatcher := New(blocking, cfg, nil)
	marker := &ShutdownMarker{}
	runtime := logqueue.NewRuntime(st.Log(), logqueue.Options{
		Workspaces:       []string{workspace},
		RescueStuckAfter: time.Hour, WorkerID: "test-" + suffix, Logf: t.Logf,
	})
	for _, registration := range dispatcher.Registrations(marker) {
		runtime.Register(registration)
	}
	if err = runtime.Start(store.WithActor(context.Background(), store.SystemActor())); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.started:
	case <-ctx.Done():
		t.Fatal("log queue dispatch did not start")
	}
	running, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil || running.State != logqueue.StateRunning || running.Attempt != 1 {
		t.Fatalf("job while running=%+v err=%v", running, err)
	}

	stopCtx, stopCancel := context.WithTimeout(store.WithActor(context.Background(), store.SystemActor()), 5*time.Second)
	defer stopCancel()
	if err = NewMarkedRuntime(runtime, marker).StopAndCancel(stopCtx); err != nil {
		t.Fatal(err)
	}
	after, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != logqueue.StateScheduled || after.Attempt != 0 || after.Generation != 1 {
		t.Fatalf("job after interruption=%+v, want scheduled with attempt 0 on the same generation", after)
	}
	if count, countErr := st.CountEvents(taskCtx, task.ID, "dispatch.failed"); countErr != nil || count != 0 {
		t.Fatalf("dispatch.failed events=%d err=%v, want 0", count, countErr)
	}
	current, err := st.GetTask(taskCtx, task.ID)
	if err != nil || current.State != core.TaskQueued {
		t.Fatalf("task=%+v err=%v, want no failure transition", current, err)
	}
}

// TestDiscardFinalizationRecoversOnPostgresIntegration: rescue of an exhausted
// dispatch job whose handler's first store read fails parks the running task
// with no notice and no restart. The job stays claimed until the task parks, so
// stage.bounce starts no newer generation, and each final step is recorded once
// (req-task-lifecycle-and-queue AC-6.2; component-durable-queue, Handlers).
func TestDiscardFinalizationRecoversOnPostgresIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(store.WithActor(t.Context(), store.SystemActor()), time.Minute)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	suffix := core.NewTaskID()
	workspace := "logqueue-discard-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Log queue discard "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	taskCtx := store.WithWorkspace(ctx, workspace)
	task := core.Task{
		ID: "logqueue-discard-" + suffix, Workspace: workspace, Repo: "repo", Title: "Discard dispatch",
		BaseBranch: "main", Branch: "conveyor/logqueue-discard-" + suffix,
		State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(taskCtx, task); err != nil {
		t.Fatal(err)
	}
	kind := queue.DispatchTaskArgs{}.Kind()
	stream := logqueue.StreamFor(kind, task.ID)
	claimedAt := time.Now().UTC().Add(-2 * time.Hour)
	if _, err = logqueue.Enqueue(ctx, st.Log(), workspace, kind, task.ID, queue.DispatchTaskArgs{WorkspaceID: workspace, TaskID: task.ID}, queue.DispatchTaskMaxAttempts, claimedAt); err != nil {
		t.Fatal(err)
	}
	enqueued, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := json.Marshal(map[string]any{"attempt": queue.DispatchTaskMaxAttempts, "worker": "dead", "claimed_at": claimedAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Log().Append(ctx, workspace, stream, enqueued.Head, []eventlog.NewEvent{{Kind: logqueue.KindClaimed, Payload: claim, At: claimedAt}}); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyStore{Store: st, getTaskID: task.ID}
	flaky.getTask.Store(1)
	runtime := logqueue.NewRuntime(st.Log(), logqueue.Options{
		Workspaces: []string{workspace}, RescueStuckAfter: time.Hour, WorkerID: "test-" + suffix, Logf: t.Logf,
	})
	for _, registration := range New(flaky, cfg, nil).Registrations(&ShutdownMarker{}) {
		runtime.Register(registration)
	}
	if err = runtime.Start(store.WithActor(context.Background(), store.SystemActor())); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = runtime.StopAndCancel(stopCtx)
	}()
	for {
		job, loadErr := logqueue.Load(ctx, st.Log(), workspace, stream)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		current, getErr := st.GetTask(taskCtx, task.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if job.State == logqueue.StateDiscarded && current.State == core.TaskParked {
			if job.Generation != enqueued.Generation {
				t.Fatalf("job=%+v started a newer generation", job)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("not finalized: job=%+v task state=%s", job, current.State)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if flaky.getTask.Load() >= 0 {
		t.Fatal("the failure was not injected")
	}
	if count, countErr := st.CountEvents(taskCtx, task.ID, "dispatch.failed"); countErr != nil || count != 1 {
		t.Fatalf("dispatch.failed=%d err=%v", count, countErr)
	}
	events, err := st.ListEvents(taskCtx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	finals := 0
	for _, event := range events {
		if event.Kind == "task.state_changed" && strings.Contains(string(event.Payload), `"dispatch.fail_final"`) {
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("dispatch.fail_final transitions=%d", finals)
	}
}

// jobIndexPool opens a pool on schema, or on a fresh schema named after
// prefix when schema is empty; tracer, when set, sees every statement.
func jobIndexPool(t *testing.T, databaseURL, prefix, schema string, tracer pgx.QueryTracer) (*pgxpool.Pool, string) {
	t.Helper()
	if schema == "" {
		admin, err := pgxpool.New(t.Context(), databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(admin.Close)
		schema = prefix + "_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
		if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		})
	}
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
	return pool, schema
}

// statement is one traced query: its SQL, arguments, and rows returned.
type statement struct {
	sql  string
	args []any
	rows int64
}

// statementTracer records every statement a pool runs, with the row count
// from its command tag.
type statementTracer struct {
	mu   sync.Mutex
	list []statement
}

type traceStartKey struct{}

func (s *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceStartKey{}, data)
}

func (s *statementTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	start, _ := ctx.Value(traceStartKey{}).(pgx.TraceQueryStartData)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, statement{sql: start.SQL, args: start.Args, rows: data.CommandTag.RowsAffected()})
}

func (s *statementTracer) take() []statement {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.list
	s.list = nil
	return out
}

// startReads classifies the statements that read the log or its index.
type startReads struct {
	repair, repairRows             int64
	unfinished, unfinishedRows     int64
	findGeneration, findRows       int64
	generationReads, eventRows     int64
	tails, tailRows                int64
	firstTailFrom                  int64
	otherEventReads                []string
	streamsRead                    map[string]int64
	eventLogInserts, indexRowReads int64
}

func classify(statements []statement) startReads {
	out := startReads{streamsRead: map[string]int64{}, firstTailFrom: -1}
	streamArg := func(st statement) string {
		if len(st.args) > 1 {
			if stream, ok := st.args[1].(string); ok {
				return stream
			}
		}
		return ""
	}
	for _, st := range statements {
		sql := st.sql
		switch {
		case strings.Contains(sql, "FROM event_log_streams s"):
			out.repair++
			out.repairRows += st.rows
		case strings.Contains(sql, "FROM event_log_job_index\nWHERE workspace_id = $1 AND state IN"):
			out.unfinished++
			out.unfinishedRows += st.rows
		case strings.Contains(sql, "FROM event_log_job_index WHERE workspace_id = $1 AND stream_id = $2"):
			out.indexRowReads++
		case strings.Contains(sql, "ORDER BY version DESC\nLIMIT 1"):
			out.findGeneration++
			out.findRows += st.rows
			out.streamsRead[streamArg(st)] += st.rows
		case strings.Contains(sql, "AND version >= $3 AND version <= $4"):
			out.generationReads++
			out.eventRows += st.rows
			out.streamsRead[streamArg(st)] += st.rows
		case strings.Contains(sql, "FROM event_log\nWHERE workspace_id = $1 AND position > $2"):
			if out.firstTailFrom < 0 && len(st.args) > 1 {
				out.firstTailFrom, _ = st.args[1].(int64)
			}
			out.tails++
			out.tailRows += st.rows
		case strings.Contains(sql, "INSERT INTO event_log ("):
			out.eventLogInserts++
		case strings.Contains(sql, "FROM event_log\n") || strings.Contains(sql, "FROM event_log "):
			out.otherEventReads = append(out.otherEventReads, strings.Join(strings.Fields(sql), " "))
			out.streamsRead[streamArg(st)] += st.rows
		}
	}
	return out
}

// TestRuntimeStartReadsOnlyUnfinishedJobsIntegration: a workspace log holds
// 100,000 finished jobs, each enqueued and completed, seeded with set-based
// SQL and indexed by the migration fill, and four unfinished jobs, one of them on a stream with
// two finished generations. A runtime start reads one repair comparison
// that returns no row, the four unfinished index rows, and only the events
// of those jobs' current generation, and no event_log row of a finished
// job. A second start with a handler then runs all four
// (req-quiet-idle-runtime AC-5.1, AC-5.2).
func TestRuntimeStartReadsOnlyUnfinishedJobsIntegration(t *testing.T) {
	const finished = 100_000
	databaseURL := dispatchIntegrationDatabaseURL(t)
	setup, schema := jobIndexPool(t, databaseURL, "job_index_start", "", nil)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	// The runtime reads only the log's tables, so the driver's schema is
	// enough; the migration fill itself is covered by
	// TestMigration155FillsJobIndexIntegration.
	if err := pglog.EnsureSchema(ctx, setup); err != nil {
		t.Fatal(err)
	}
	ws := "job-index-" + core.NewTaskID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedStarted := time.Now()
	if _, err := setup.Exec(ctx, `
INSERT INTO event_log (workspace_id, position, stream_id, version, kind, actor_id, actor_role, payload, at)
SELECT $1, (g - 1) * 2 + v, 'job/finished:' || g, v,
       CASE v WHEN 1 THEN 'job.enqueued' ELSE 'job.completed' END,
       'queue', 'system',
       CASE v WHEN 1 THEN jsonb_build_object('kind', 'finished', 'key', g::text, 'args', '{}'::jsonb, 'max_attempts', 3)
              ELSE jsonb_build_object('attempt', 1) END,
       $2
FROM generate_series(1, $3::integer) AS g, generate_series(1, 2) AS v`, ws, now.Add(-time.Hour), finished); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `
INSERT INTO event_log_streams (workspace_id, stream_id, version)
SELECT $1, 'job/finished:' || g, 2 FROM generate_series(1, $2::integer) AS g`, ws, finished); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `INSERT INTO event_log_positions (workspace_id, position) VALUES ($1, $2)`, ws, finished*2); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded in %s", time.Since(seedStarted).Round(time.Millisecond))
	fillStarted := time.Now()
	filled, err := pglog.FillJobIndex(ctx, setup)
	t.Logf("filled in %s", time.Since(fillStarted).Round(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if filled < finished {
		t.Fatalf("fill folded %d streams, want at least %d", filled, finished)
	}
	t.Logf("seeded %d finished jobs (%d events) and filled the index in %s", finished, finished*2, time.Since(seedStarted).Round(time.Millisecond))

	// The unfinished jobs are written through the driver, as a daemon's
	// store writes them.
	log := logqueue.Notify(pglog.New(setup), changes.NewHub())
	appendJob := func(key string, expected eventlog.Version, kind string, payload any) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(ctx, ws, jobindex.StreamFor("demo", key), expected, []eventlog.NewEvent{{Kind: kind, ActorID: "test", ActorRole: "system", Payload: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	enqueue := func(key string, expected eventlog.Version) {
		appendJob(key, expected, jobindex.KindEnqueued, jobindex.EnqueuedPayload{Kind: "demo", Key: key, Args: json.RawMessage(`{}`), MaxAttempts: 3})
	}
	claim := func(key string, expected eventlog.Version, at time.Time) {
		appendJob(key, expected, jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: 1, Worker: "old", ClaimedAt: at})
	}
	enqueue("available", 0)
	enqueue("scheduled", 0)
	claim("scheduled", 1, now)
	appendJob("scheduled", 2, jobindex.KindFailed, jobindex.OutcomePayload{Attempt: 1, Error: "boom", NextAt: now.Add(-time.Second)})
	enqueue("running", 0)
	claim("running", 1, now.Add(-time.Hour))
	for generation := 0; generation < 2; generation++ {
		head := eventlog.Version(generation * 3)
		enqueue("again", head)
		claim("again", head+1, now)
		appendJob("again", head+2, jobindex.KindCompleted, jobindex.OutcomePayload{Attempt: 1})
	}
	enqueue("again", 6)
	unfinished := map[string]bool{"job/demo:available": true, "job/demo:scheduled": true, "job/demo:running": true, "job/demo:again": true}
	// Current generations: available 1 event, scheduled 3, running 2, and
	// again 1 (its two finished generations hold 6 more).
	const currentGenerationEvents = 7
	var head int64
	if err := setup.QueryRow(ctx, `SELECT position FROM event_log_positions WHERE workspace_id = $1`, ws).Scan(&head); err != nil {
		t.Fatal(err)
	}

	// First start: no handler, so the load is all that reads.
	tracer := &statementTracer{}
	traced, _ := jobIndexPool(t, databaseURL, "", schema, tracer)
	daemonLog := logqueue.Notify(pglog.New(traced), changes.NewHub())
	loaded := make(chan string, 4)
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, "loaded") {
			loaded <- line
		}
	}
	tracer.take()
	started := time.Now()
	rt := logqueue.NewRuntime(daemonLog, logqueue.Options{Workspaces: []string{ws}, Logf: logf})
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var line string
	select {
	case line = <-loaded:
	case <-time.After(30 * time.Second):
		t.Fatal("runtime did not load the job index")
	}
	loadTime := time.Since(started)
	waitFor := time.Now().Add(10 * time.Second)
	for classify(tracerSnapshot(tracer)).tails == 0 && time.Now().Before(waitFor) {
		time.Sleep(5 * time.Millisecond)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := rt.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	reads := classify(tracer.take())
	t.Logf("start-up on %d finished and 4 unfinished jobs (%s; load %s): repair comparison %d statement, %d rows; unfinished index read %d statement, %d rows; current-generation lookups %d statements, %d rows; current-generation event reads %d statements, %d rows; tails %d statements from position %d, %d rows; other event_log reads %d",
		finished, line, loadTime.Round(time.Millisecond), reads.repair, reads.repairRows, reads.unfinished, reads.unfinishedRows,
		reads.findGeneration, reads.findRows, reads.generationReads, reads.eventRows, reads.tails, reads.firstTailFrom, reads.tailRows, len(reads.otherEventReads))
	if reads.repair != 1 || reads.repairRows != 0 {
		t.Fatalf("repair comparison: %d statements returned %d rows, want 1 statement and no row", reads.repair, reads.repairRows)
	}
	if reads.unfinished != 1 || reads.unfinishedRows != 4 {
		t.Fatalf("unfinished index read: %d statements returned %d rows, want 1 statement and 4 rows", reads.unfinished, reads.unfinishedRows)
	}
	if reads.findGeneration != 4 || reads.findRows != 4 || reads.generationReads != 4 || reads.eventRows != currentGenerationEvents {
		t.Fatalf("current-generation reads: %d lookups (%d rows), %d reads (%d rows), want 4 (4), 4 (%d)", reads.findGeneration, reads.findRows, reads.generationReads, reads.eventRows, currentGenerationEvents)
	}
	if reads.tails == 0 || reads.firstTailFrom != head || reads.tailRows != 0 {
		t.Fatalf("tails: %d from %d returned %d rows, want tails from the head %d returning no row", reads.tails, reads.firstTailFrom, reads.tailRows, head)
	}
	if len(reads.otherEventReads) != 0 {
		t.Fatalf("unexpected event_log reads: %v", reads.otherEventReads)
	}
	for stream := range reads.streamsRead {
		if !unfinished[stream] {
			t.Fatalf("start-up read event_log rows of %q, which is not an unfinished job", stream)
		}
	}

	// Second start: the handler runs every unfinished job; the running one
	// after rescue of its dead claim.
	ran := make(chan string, 8)
	rt = logqueue.NewRuntime(daemonLog, logqueue.Options{Workspaces: []string{ws}, RescueStuckAfter: time.Minute})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(_ context.Context, job queue.Job) error {
		ran <- job.ID
		return nil
	}, RetryDelay: func(int) time.Duration { return 10 * time.Millisecond }})
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rt.StopAndCancel(stop)
	})
	want := map[string]bool{"job/demo:available@1": true, "job/demo:scheduled@1": true, "job/demo:running@1": true, "job/demo:again@3": true}
	deadline := time.After(30 * time.Second)
	for len(want) > 0 {
		select {
		case id := <-ran:
			delete(want, id)
		case <-deadline:
			t.Fatalf("jobs that did not run: %v", want)
		}
	}
	for stream := range classify(tracer.take()).streamsRead {
		if strings.HasPrefix(stream, "job/finished:") {
			t.Fatalf("the running runtime read finished stream %s", stream)
		}
	}
}

func tracerSnapshot(tracer *statementTracer) []statement {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	return append([]statement(nil), tracer.list...)
}
