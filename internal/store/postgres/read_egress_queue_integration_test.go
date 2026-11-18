package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func init() {
	readEgressCases.Register("queue-enqueue-indexed-unfinished", buildQueueEnqueueIndexed)
	readEgressCases.Register("queue-startup-unfinished-jobs", buildQueueStartupUnfinished)
}

const (
	egressQueueKind = "egress-queue"
	// egressQueueStreams job streams share one workspace log. Streams 0..3
	// end with an unfinished generation; stream 0 is the enqueue target.
	egressQueueStreams     = 5
	egressQueueOpenStreams = 4
	egressQueueTargetKey   = "s0"
	// egressQueueMaxHistory bounds the history rows the indexed candidate may
	// read: its decisions and startup come from the job index rows.
	egressQueueMaxHistory = 31
	egressQueueChunk      = 999
	egressQueueTailPage   = 1000
	egressQueueDefaultGen = 2002
)

// egressCountingQueueLog counts the history events a queue read serves. It
// forwards the job index readers of the store's log, so a candidate reads the
// index exactly as the store does, and counts only event-history reads.
type egressCountingQueueLog struct {
	eventlog.Store
	history int
}

func (l *egressCountingQueueLog) Read(ctx context.Context, workspace string, stream eventlog.StreamID, after eventlog.Version, limit int) ([]eventlog.Event, error) {
	events, err := l.Store.Read(ctx, workspace, stream, after, limit)
	l.history += len(events)
	return events, err
}

func (l *egressCountingQueueLog) Tail(ctx context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	events, err := l.Store.Tail(ctx, workspace, after, limit)
	l.history += len(events)
	return events, err
}

func (l *egressCountingQueueLog) ReadGeneration(ctx context.Context, workspace string, stream eventlog.StreamID, version eventlog.Version) ([]eventlog.Event, error) {
	reader, ok := jobindex.ReaderOf(l.Store)
	if !ok {
		return nil, jobindex.ErrUnsupported
	}
	events, err := reader.ReadGeneration(ctx, workspace, stream, version)
	l.history += len(events)
	return events, err
}

func (l *egressCountingQueueLog) UnfinishedJobs(ctx context.Context, workspace string) (eventlog.Position, []jobindex.Row, error) {
	reader, ok := jobindex.ReaderOf(l.Store)
	if !ok {
		return 0, nil, jobindex.ErrUnsupported
	}
	return reader.UnfinishedJobs(ctx, workspace)
}

func (l *egressCountingQueueLog) JobIndexRow(ctx context.Context, workspace string, stream eventlog.StreamID) (jobindex.Row, bool, error) {
	reader, ok := jobindex.ReaderOf(l.Store)
	if !ok {
		return jobindex.Row{}, false, jobindex.ErrUnsupported
	}
	return reader.JobIndexRow(ctx, workspace, stream)
}

func (l *egressCountingQueueLog) RepairJobIndex(ctx context.Context, workspace string) (int, error) {
	reader, ok := jobindex.ReaderOf(l.Store)
	if !ok {
		return 0, jobindex.ErrUnsupported
	}
	return reader.RepairJobIndex(ctx, workspace)
}

// egressNoIndexQueueLog hides the job index, as the storeOnly wrapper in
// logqueue's enqueue_index_test.go does: the queue reads the event history
// the way it did before the index existed.
type egressNoIndexQueueLog struct{ eventlog.Store }

// egressQueueFixture describes the owned workspace one queue case measures.
type egressQueueFixture struct {
	workspace    string
	rows         int
	targetRows   int
	unfinished   int
	counts       map[string]int
	targetStream eventlog.StreamID
}

// egressQueueWorkspace creates a fresh workspace so the case's unfinished-job
// set and row counts come only from its own fixture.
func egressQueueWorkspace(t testing.TB, ctx context.Context, st *Store) (context.Context, string) {
	t.Helper()
	ws := "egress-queue-" + core.NewTaskID()
	ctx = store.WithWorkspace(ctx, ws)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws,
		Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	return ctx, ws
}

func egressQueueGenerations(t testing.TB) int {
	t.Helper()
	n, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_QUEUE_GENERATIONS", egressQueueDefaultGen)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func egressQueueJSON(t testing.TB, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func egressQueueEvent(kind string, payload json.RawMessage, at time.Time) eventlog.NewEvent {
	return eventlog.NewEvent{Kind: kind, ActorID: core.SystemActor("queue").ID, ActorRole: string(core.ActorSystem), Payload: payload, At: at}
}

// seedEgressQueue appends, through the public log API, generations finished
// job generations (enqueued, claimed, completed: three rows each) to each of
// egressQueueStreams streams. The first egressQueueOpenStreams streams also get
// an unfinished generation (one enqueued row).
func seedEgressQueue(t testing.TB, ctx context.Context, st *Store, ws string, generations int) egressQueueFixture {
	t.Helper()
	log := st.Log()
	fx := egressQueueFixture{workspace: ws, counts: map[string]int{}}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seq := 0
	stamp := func() time.Time {
		seq++
		return base.Add(time.Duration(seq) * time.Second)
	}
	completed := egressQueueJSON(t, struct{}{})
	for s := range egressQueueStreams {
		key := fmt.Sprintf("s%d", s)
		stream := logqueue.StreamFor(egressQueueKind, key)
		enqueuedPayload := egressQueueJSON(t, jobindex.EnqueuedPayload{Kind: egressQueueKind, Key: key, Args: json.RawMessage(`{}`), MaxAttempts: 3})
		var events []eventlog.NewEvent
		for range generations {
			at := stamp()
			claimed := egressQueueJSON(t, jobindex.ClaimedPayload{Attempt: 1, Worker: "egress-worker", ClaimedAt: at})
			events = append(events,
				egressQueueEvent(jobindex.KindEnqueued, enqueuedPayload, at),
				egressQueueEvent(jobindex.KindClaimed, claimed, at),
				egressQueueEvent(jobindex.KindCompleted, completed, at))
		}
		if s < egressQueueOpenStreams {
			events = append(events, egressQueueEvent(jobindex.KindEnqueued, enqueuedPayload, stamp()))
			fx.unfinished++
		}
		appendEgressQueue(t, ctx, log, ws, stream, events)
		fx.rows += len(events)
		if key == egressQueueTargetKey {
			fx.targetStream = stream
			fx.targetRows = len(events)
		}
	}
	fx.counts = map[string]int{"streams": egressQueueStreams, "generations_per_stream": generations,
		"rows_total": fx.rows, "target_rows": fx.targetRows, "unfinished_jobs": fx.unfinished}
	return fx
}

// appendEgressQueue writes events to one stream in chunks, each chunk expecting
// the head the previous chunk returned.
func appendEgressQueue(t testing.TB, ctx context.Context, log eventlog.Store, ws string, stream eventlog.StreamID, events []eventlog.NewEvent) {
	t.Helper()
	expected := eventlog.ExpectNew
	for start := 0; start < len(events); start += egressQueueChunk {
		end := min(start+egressQueueChunk, len(events))
		head, err := log.Append(ctx, ws, stream, expected, events[start:end])
		if err != nil {
			t.Fatal(err)
		}
		expected = head
	}
}

// egressQueueDecision is the served answer of one enqueue: the stream and
// whether a new job was inserted. Successes counts the one decision.
func egressQueueDecision(stream eventlog.StreamID, inserted bool) (storetest.ReadEgressOutcome, error) {
	digest, err := storetest.ReadEgressDigest(map[string]any{"stream": stream, "enqueued": inserted})
	return storetest.ReadEgressOutcome{Digest: digest, Successes: 1}, err
}

type egressQueueServedRow struct {
	Stream     eventlog.StreamID `json:"stream"`
	State      jobindex.State    `json:"state"`
	Generation int               `json:"generation"`
	Version    eventlog.Version  `json:"version"`
}

// egressQueueRowsOutcome digests the unfinished rows by their stable fields,
// ordered by stream. Timestamps are left out.
func egressQueueRowsOutcome(rows []jobindex.Row) (storetest.ReadEgressOutcome, error) {
	served := make([]egressQueueServedRow, 0, len(rows))
	for _, row := range rows {
		served = append(served, egressQueueServedRow{Stream: row.Stream, State: row.State, Generation: row.Generation, Version: row.Version})
	}
	sort.Slice(served, func(i, j int) bool { return served[i].Stream < served[j].Stream })
	digest, err := storetest.ReadEgressDigest(served)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(served)}, err
}

// legacyUnfinishedRows is the startup path of a log without a job index: it
// tails the whole workspace from position zero and folds every job stream.
func legacyUnfinishedRows(ctx context.Context, log eventlog.Store, ws string) ([]jobindex.Row, error) {
	streams := map[eventlog.StreamID][]eventlog.Event{}
	var after eventlog.Position
	for {
		page, err := log.Tail(ctx, ws, after, egressQueueTailPage)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			streams[event.Stream] = append(streams[event.Stream], event)
		}
		after = page[len(page)-1].Position
	}
	var rows []jobindex.Row
	for stream, events := range streams {
		job, err := jobindex.Fold(ws, stream, events)
		if err != nil {
			return nil, err
		}
		if jobindex.Unfinished(job.State) {
			rows = append(rows, jobindex.RowOf(job))
		}
	}
	return rows, nil
}

// buildQueueEnqueueIndexed measures one ordinary Enqueue into a stream whose
// generation is unfinished, so the enqueue inserts nothing and no state
// changes between calls. The candidate decides from the job index row; the
// baseline hides the index and folds the target stream's whole history.
// Provenance: the baseline is not a literal HEAD or recorded-revision SQL. It is
// the checked-out tree's log with the job index hidden, so the queue folds
// history as it did before the index existed. The run-level baseline_revision
// label (storetest.ReadEgressEnv) must say so; see the report.
func buildQueueEnqueueIndexed(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	ctx, ws := egressQueueWorkspace(t, ctx, st)
	fx := seedEgressQueue(t, ctx, st, ws, egressQueueGenerations(t))
	args := map[string]string{"key": egressQueueTargetKey}
	enqueue := func(ctx context.Context, log eventlog.Store) (storetest.ReadEgressOutcome, error) {
		inserted, err := logqueue.Enqueue(ctx, log, ws, egressQueueKind, egressQueueTargetKey, args, 3, time.Now().UTC())
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		if inserted {
			return storetest.ReadEgressOutcome{}, fmt.Errorf("enqueue inserted a job on the unfinished target stream")
		}
		return egressQueueDecision(fx.targetStream, inserted)
	}
	return storetest.ReadEgressCase{Name: "queue-enqueue-indexed-unfinished", Fixture: fx.counts,
		BaselineRevision: "candidate-revision:job-index-hidden-fold",
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			ctx = dbmetrics.WithReadLabel(ctx, "LegacyQueueEnqueueFold")
			log := &egressCountingQueueLog{Store: st.Log()}
			outcome, err := enqueue(ctx, egressNoIndexQueueLog{log})
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			if log.history != fx.targetRows {
				return storetest.ReadEgressOutcome{}, fmt.Errorf("baseline folded %d history rows, want the target stream's %d", log.history, fx.targetRows)
			}
			return outcome, nil
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			ctx = dbmetrics.WithReadLabel(ctx, "QueueEnqueueIndexedHead")
			log := &egressCountingQueueLog{Store: st.Log()}
			outcome, err := enqueue(ctx, log)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			if log.history > egressQueueMaxHistory {
				return storetest.ReadEgressOutcome{}, fmt.Errorf("candidate read %d history rows, want at most %d", log.history, egressQueueMaxHistory)
			}
			return outcome, nil
		}}
}

// buildQueueStartupUnfinished measures the startup read of unfinished jobs
// across the whole workspace log: the candidate reads the job index rows, the
// baseline hides the index and folds every event of the workspace.
// Provenance: the baseline is not a literal HEAD or recorded-revision SQL. It is
// the checked-out tree's log with the job index hidden, so the queue folds
// history as it did before the index existed. The run-level baseline_revision
// label (storetest.ReadEgressEnv) must say so; see the report.
func buildQueueStartupUnfinished(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	ctx, ws := egressQueueWorkspace(t, ctx, st)
	fx := seedEgressQueue(t, ctx, st, ws, egressQueueGenerations(t))
	return storetest.ReadEgressCase{Name: "queue-startup-unfinished-jobs", Fixture: fx.counts,
		BaselineRevision: "candidate-revision:job-index-hidden-fold",
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			ctx = dbmetrics.WithReadLabel(ctx, "LegacyQueueStartupFold")
			log := &egressCountingQueueLog{Store: st.Log()}
			rows, err := legacyUnfinishedRows(ctx, egressNoIndexQueueLog{log}, ws)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			if log.history != fx.rows {
				return storetest.ReadEgressOutcome{}, fmt.Errorf("baseline read %d history rows, want the workspace's %d", log.history, fx.rows)
			}
			if len(rows) != fx.unfinished {
				return storetest.ReadEgressOutcome{}, fmt.Errorf("baseline found %d unfinished jobs, want %d", len(rows), fx.unfinished)
			}
			return egressQueueRowsOutcome(rows)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			ctx = dbmetrics.WithReadLabel(ctx, "QueueStartupUnfinishedIndex")
			log := &egressCountingQueueLog{Store: st.Log()}
			_, rows, err := log.UnfinishedJobs(ctx, ws)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			if log.history > egressQueueMaxHistory {
				return storetest.ReadEgressOutcome{}, fmt.Errorf("candidate read %d history rows, want at most %d", log.history, egressQueueMaxHistory)
			}
			if len(rows) != fx.unfinished {
				return storetest.ReadEgressOutcome{}, fmt.Errorf("candidate found %d unfinished jobs, want %d", len(rows), fx.unfinished)
			}
			return egressQueueRowsOutcome(rows)
		}}
}
