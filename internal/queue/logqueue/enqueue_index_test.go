package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
)

type countingIndexLog struct {
	inner        eventlog.Store
	reads        atomic.Int64
	readEvents   atomic.Int64
	indexLookups atomic.Int64
	indexRows    atomic.Int64
	appends      atomic.Int64
}

func (l *countingIndexLog) Append(ctx context.Context, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	l.appends.Add(1)
	return l.inner.Append(ctx, workspace, stream, expected, events)
}

func (l *countingIndexLog) Read(ctx context.Context, workspace string, stream eventlog.StreamID, after eventlog.Version, limit int) ([]eventlog.Event, error) {
	l.reads.Add(1)
	events, err := l.inner.Read(ctx, workspace, stream, after, limit)
	l.readEvents.Add(int64(len(events)))
	return events, err
}

func (l *countingIndexLog) Head(ctx context.Context, workspace string, stream eventlog.StreamID) (eventlog.Version, error) {
	return l.inner.Head(ctx, workspace, stream)
}

func (l *countingIndexLog) Tail(ctx context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	return l.inner.Tail(ctx, workspace, after, limit)
}

func (l *countingIndexLog) UnfinishedJobs(ctx context.Context, workspace string) (eventlog.Position, []jobindex.Row, error) {
	reader, ok := jobindex.ReaderOf(l.inner)
	if !ok {
		return 0, nil, jobindex.ErrUnsupported
	}
	return reader.UnfinishedJobs(ctx, workspace)
}

func (l *countingIndexLog) JobIndexRow(ctx context.Context, workspace string, stream eventlog.StreamID) (jobindex.Row, bool, error) {
	l.indexLookups.Add(1)
	reader, ok := jobindex.ReaderOf(l.inner)
	if !ok {
		return jobindex.Row{}, false, jobindex.ErrUnsupported
	}
	row, found, err := reader.JobIndexRow(ctx, workspace, stream)
	if err == nil && found {
		l.indexRows.Add(1)
	}
	return row, found, err
}

func (l *countingIndexLog) ReadGeneration(ctx context.Context, workspace string, stream eventlog.StreamID, version eventlog.Version) ([]eventlog.Event, error) {
	reader, ok := jobindex.ReaderOf(l.inner)
	if !ok {
		return nil, jobindex.ErrUnsupported
	}
	events, err := reader.ReadGeneration(ctx, workspace, stream, version)
	l.readEvents.Add(int64(len(events)))
	return events, err
}

func (l *countingIndexLog) RepairJobIndex(ctx context.Context, workspace string) (int, error) {
	reader, ok := jobindex.ReaderOf(l.inner)
	if !ok {
		return 0, jobindex.ErrUnsupported
	}
	return reader.RepairJobIndex(ctx, workspace)
}

type storeOnly struct{ eventlog.Store }

type errIndexLog struct {
	*memlog.Store
	err error
}

func (l *errIndexLog) JobIndexRow(context.Context, string, eventlog.StreamID) (jobindex.Row, bool, error) {
	return jobindex.Row{}, false, l.err
}

type staleIndexLog struct {
	*memlog.Store
	row jobindex.Row
}

func (l *staleIndexLog) JobIndexRow(context.Context, string, eventlog.StreamID) (jobindex.Row, bool, error) {
	return l.row, true, nil
}

type conflictIndexLog struct {
	*memlog.Store
	remaining int
}

func (l *conflictIndexLog) Append(ctx context.Context, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	if l.remaining > 0 {
		l.remaining--
		return 0, &eventlog.VersionConflictError{Workspace: workspace, Stream: stream, Expected: expected, Actual: expected + 1}
	}
	return l.Store.Append(ctx, workspace, stream, expected, events)
}

type genReadCounter struct {
	*memlog.Store
	mu       sync.Mutex
	events   int
	earliest eventlog.Version
}

func (r *genReadCounter) ReadGeneration(ctx context.Context, workspace string, stream eventlog.StreamID, version eventlog.Version) ([]eventlog.Event, error) {
	events, err := r.Store.ReadGeneration(ctx, workspace, stream, version)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events += len(events)
	for _, event := range events {
		if r.earliest == 0 || event.Version < r.earliest {
			r.earliest = event.Version
		}
	}
	return events, err
}

func seedJobHistory(t *testing.T, log eventlog.Store, workspace, key string, n int, finish bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	stream := StreamFor("demo", key)
	if n < 1 {
		t.Fatal("history needs at least the enqueue")
	}
	if finish && n < 2 {
		n = 2
	}
	payload, err := json.Marshal(enqueuedPayload{Kind: "demo", Key: key, Args: []byte(`{}`), MaxAttempts: 9})
	if err != nil {
		t.Fatal(err)
	}
	events := []eventlog.NewEvent{{Kind: KindEnqueued, Payload: payload, At: now}}
	remaining := n - 1
	if finish {
		if remaining < 1 {
			t.Fatal("finished history needs a completion")
		}
		remaining--
	}
	attempt := 1
	for remaining > 0 {
		events = append(events, eventlog.NewEvent{Kind: KindClaimed, Payload: mustJSON(claimedPayload{Attempt: attempt, Worker: "w", ClaimedAt: now}), At: now})
		remaining--
		if remaining == 0 {
			break
		}
		events = append(events, eventlog.NewEvent{Kind: KindSnoozed, Payload: mustJSON(outcomePayload{Attempt: attempt, Until: now}), At: now})
		remaining--
		attempt++
	}
	if finish {
		events = append(events, eventlog.NewEvent{Kind: KindCompleted, Payload: mustJSON(outcomePayload{Attempt: 1}), At: now})
	}
	if _, err := log.Append(ctx, workspace, stream, 0, events); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func drainJobNotices(n *changes.Subscription) int {
	count := 0
	for {
		select {
		case <-n.C():
			count++
		default:
			return count
		}
	}
}

func TestEnqueueUsesJobIndexWithoutHistoricalReads(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	for _, n := range []int{1, 100, 6000} {
		for _, finish := range []bool{false, true} {
			t.Run(fmt.Sprintf("events=%d finish=%t", n, finish), func(t *testing.T) {
				indexed := memlog.New()
				folded := memlog.New()
				seedJobHistory(t, indexed, "ws", "k", n, finish)
				seedJobHistory(t, folded, "ws", "k", n, finish)
				hub := changes.NewHub()
				indexLog := &countingIndexLog{inner: Notify(indexed, hub)}
				notices := hub.Subscribe("ws", changes.Job)
				defer notices.Close()
				got, err := Enqueue(ctx, indexLog, "ws", "demo", "k", demoArgs{Key: "k"}, 3, now)
				if err != nil {
					t.Fatal(err)
				}
				want, err := Enqueue(ctx, storeOnly{folded}, "ws", "demo", "k", demoArgs{Key: "k"}, 3, now)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("indexed inserted=%t fold inserted=%t", got, want)
				}
				if finish != got {
					t.Fatalf("inserted=%t, want %t for finish=%t", got, finish, finish)
				}
				if indexLog.reads.Load() != 0 || indexLog.readEvents.Load() != 0 {
					t.Fatalf("ordinary enqueue read %d calls/%d events", indexLog.reads.Load(), indexLog.readEvents.Load())
				}
				if indexLog.indexLookups.Load() != 1 || indexLog.indexRows.Load() != 1 {
					t.Fatalf("index lookups=%d rows=%d, want 1/1", indexLog.indexLookups.Load(), indexLog.indexRows.Load())
				}
				if got && indexLog.appends.Load() != 1 {
					t.Fatalf("appends=%d, want 1", indexLog.appends.Load())
				}
				if !got && indexLog.appends.Load() != 0 {
					t.Fatalf("suppressed enqueue appended %d times", indexLog.appends.Load())
				}
				raised := drainJobNotices(notices)
				if got && raised != 1 {
					t.Fatalf("inserted enqueue notices=%d, want 1", raised)
				}
				if !got && raised != 0 {
					t.Fatalf("suppressed enqueue notices=%d", raised)
				}
			})
		}
	}
}

func TestEnqueueIndexFallbacksAndConflicts(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("missingRowFoldsOnce", func(t *testing.T) {
		log := memlog.New()
		stream := StreamFor("demo", "missing")
		if _, err := log.Append(ctx, "ws", stream, 0, []eventlog.NewEvent{{Kind: KindEnqueued, Payload: mustJSON(enqueuedPayload{Kind: "demo", Key: "missing", Args: []byte(`{}`), MaxAttempts: 3}), At: now}}); err != nil {
			t.Fatal(err)
		}
		appendKind(t, log, "ws", stream, 1, KindClaimed, claimedPayload{Attempt: 1, Worker: "w", ClaimedAt: now})
		appendKind(t, log, "ws", stream, 2, KindCompleted, outcomePayload{Attempt: 1})
		log.DropJobIndexRow("ws", stream)
		counted := &countingIndexLog{inner: log}
		inserted, err := Enqueue(ctx, counted, "ws", "demo", "missing", demoArgs{}, 3, now)
		if err != nil || !inserted {
			t.Fatalf("inserted=%t err=%v", inserted, err)
		}
		if counted.reads.Load() != 1 {
			t.Fatalf("reads=%d, want one fold", counted.reads.Load())
		}
	})

	t.Run("unsupportedReaderFoldsOnce", func(t *testing.T) {
		log := memlog.New()
		seedJobHistory(t, log, "ws", "plain", 8, true)
		inserted, err := Enqueue(ctx, storeOnly{log}, "ws", "demo", "plain", demoArgs{}, 3, now)
		if err != nil || !inserted {
			t.Fatalf("inserted=%t err=%v", inserted, err)
		}
	})

	t.Run("readerError", func(t *testing.T) {
		want := errors.New("index unavailable")
		_, err := Enqueue(ctx, &errIndexLog{Store: memlog.New(), err: want}, "ws", "demo", "err", demoArgs{}, 3, now)
		if !errors.Is(err, want) {
			t.Fatalf("err=%v, want the reader error", err)
		}
	})

	t.Run("staleTerminalIndex", func(t *testing.T) {
		log := memlog.New()
		seedJobHistory(t, log, "ws", "stale", 4, true)
		stream := StreamFor("demo", "stale")
		row, ok, err := log.JobIndexRow(ctx, "ws", stream)
		if err != nil || !ok {
			t.Fatalf("row ok=%v err=%v", ok, err)
		}
		inserted, err := Enqueue(ctx, log, "ws", "demo", "stale", demoArgs{}, 3, now)
		if err != nil || !inserted {
			t.Fatalf("second generation inserted=%t err=%v", inserted, err)
		}
		stale := &staleIndexLog{Store: log, row: row}
		hub := changes.NewHub()
		notifying := Notify(stale, hub)
		notices := hub.Subscribe("ws", changes.Job)
		defer notices.Close()
		inserted, err = Enqueue(ctx, notifying, "ws", "demo", "stale", demoArgs{}, 3, now)
		if err != nil || inserted {
			t.Fatalf("stale terminal inserted=%t err=%v", inserted, err)
		}
		if drainJobNotices(notices) != 0 {
			t.Fatal("stale terminal enqueue raised a notice")
		}
	})

	t.Run("concurrentWinner", func(t *testing.T) {
		log := memlog.New()
		hub := changes.NewHub()
		notifying := Notify(log, hub)
		notices := hub.Subscribe("ws", changes.Job)
		defer notices.Close()
		var inserted atomic.Int64
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := Enqueue(ctx, notifying, "ws", "demo", "race", demoArgs{}, 3, now)
				if err != nil {
					t.Errorf("enqueue: %v", err)
					return
				}
				if ok {
					inserted.Add(1)
				}
			}()
		}
		wg.Wait()
		if inserted.Load() != 1 {
			t.Fatalf("inserted=%d, want 1", inserted.Load())
		}
		if drainJobNotices(notices) != 1 {
			t.Fatal("concurrent enqueue notices")
		}
	})

	t.Run("secondConflictStops", func(t *testing.T) {
		log := memlog.New()
		seedJobHistory(t, log, "ws", "conflict", 4, true)
		hub := changes.NewHub()
		wrapped := &conflictIndexLog{Store: log, remaining: 2}
		notifying := Notify(wrapped, hub)
		notices := hub.Subscribe("ws", changes.Job)
		defer notices.Close()
		inserted, err := Enqueue(ctx, notifying, "ws", "demo", "conflict", demoArgs{}, 3, now)
		if err != nil || inserted {
			t.Fatalf("inserted=%t err=%v", inserted, err)
		}
		if drainJobNotices(notices) != 0 {
			t.Fatal("conflicting enqueue raised a notice")
		}
	})
}

func TestLoadDiscardedReadsOnlyCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	log := memlog.New()
	stream := StreamFor("demo", "discard")
	payload, _ := json.Marshal(enqueuedPayload{Kind: "demo", Key: "discard", Args: []byte(`{}`), MaxAttempts: 2})
	first := []eventlog.NewEvent{
		{Kind: KindEnqueued, Payload: payload, At: now},
	}
	for range 20 {
		first = append(first,
			eventlog.NewEvent{Kind: KindClaimed, Payload: mustJSON(claimedPayload{Attempt: 1, Worker: "w", ClaimedAt: now}), At: now},
			eventlog.NewEvent{Kind: KindSnoozed, Payload: mustJSON(outcomePayload{Attempt: 1, Until: now}), At: now},
		)
	}
	first = append(first, eventlog.NewEvent{Kind: KindCompleted, Payload: mustJSON(outcomePayload{Attempt: 1}), At: now})
	if _, err := log.Append(ctx, "ws", stream, 0, first); err != nil {
		t.Fatal(err)
	}
	currentStart := eventlog.Version(len(first) + 1)
	second, _ := json.Marshal(enqueuedPayload{Kind: "demo", Key: "discard", Args: []byte(`{"n":2}`), MaxAttempts: 2})
	if _, err := log.Append(ctx, "ws", stream, eventlog.Version(len(first)), []eventlog.NewEvent{
		{Kind: KindEnqueued, Payload: second, At: now},
		{Kind: KindClaimed, Payload: mustJSON(claimedPayload{Attempt: 1, Worker: "w", ClaimedAt: now}), At: now},
		{Kind: KindFailed, Payload: mustJSON(outcomePayload{Attempt: 1, Error: "boom"}), At: now},
		{Kind: KindClaimed, Payload: mustJSON(claimedPayload{Attempt: 2, Worker: "w", ClaimedAt: now}), At: now},
		{Kind: KindDiscarded, Payload: mustJSON(outcomePayload{Attempt: 2, Error: "exhausted"}), At: now},
	}); err != nil {
		t.Fatal(err)
	}
	counter := &genReadCounter{Store: log}
	job, discarded, err := LoadDiscarded(ctx, counter, "ws", stream)
	if err != nil || !discarded {
		t.Fatalf("discarded=%t err=%v job=%+v", discarded, err, job)
	}
	if job.Attempt != 2 || job.MaxAttempts != 2 || job.Generation != 2 {
		t.Fatalf("job=%+v", job)
	}
	if counter.earliest != currentStart {
		t.Fatalf("read from version %d, want current generation %d", counter.earliest, currentStart)
	}
}

func TestEnqueueFiveStreamFixtureBoundsReturnedRows(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	log := memlog.New()
	total := 0
	for i := range 5 {
		n := 6006
		if i == 0 {
			n = 6007
		}
		seedJobHistory(t, log, "ws", fmt.Sprintf("s%d", i), n, true)
		total += n
	}
	if total != 30031 {
		t.Fatalf("fixture events=%d, want 30031", total)
	}
	counted := &countingIndexLog{inner: log}
	var inserted int
	for i := range 5 {
		ok, err := Enqueue(ctx, counted, "ws", "demo", fmt.Sprintf("s%d", i), demoArgs{}, 3, now)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			inserted++
		}
	}
	if inserted != 5 {
		t.Fatalf("inserted=%d, want 5", inserted)
	}
	returned := counted.indexRows.Load() + counted.readEvents.Load()
	if counted.reads.Load() != 0 || returned > 31 {
		t.Fatalf("reads=%d returned=%d lookups=%d", counted.reads.Load(), returned, counted.indexLookups.Load())
	}
}

// TestLoadDiscardedReportsTheCurrentDiscardTime proves LoadDiscarded returns
// the recorded time of the current generation's job.discarded event, and the
// same time from the job index, from a stream whose index row is missing, and
// from a log without an index (component-verification-publication invariant
// 26). An earlier generation's discard never stands in for the current one.
func TestLoadDiscardedReportsTheCurrentDiscardTime(t *testing.T) {
	ctx := context.Background()
	firstAt := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Hour)
	seed := func(t *testing.T) (*memlog.Store, eventlog.StreamID) {
		t.Helper()
		log := memlog.New()
		stream := StreamFor("demo", "discard-at")
		var version eventlog.Version
		for _, at := range []time.Time{firstAt, secondAt} {
			payload := mustJSON(enqueuedPayload{Kind: "demo", Key: "discard-at", Args: []byte(`{}`), MaxAttempts: 1})
			next, err := log.Append(ctx, "ws", stream, version, []eventlog.NewEvent{
				{Kind: KindEnqueued, Payload: payload, At: at.Add(-time.Minute)},
				{Kind: KindClaimed, Payload: mustJSON(claimedPayload{Attempt: 1, Worker: "w", ClaimedAt: at}), At: at},
				{Kind: KindDiscarded, Payload: mustJSON(outcomePayload{Attempt: 1, Error: "exhausted"}), At: at},
			})
			if err != nil {
				t.Fatal(err)
			}
			version = next
		}
		return log, stream
	}
	check := func(t *testing.T, log eventlog.Store, stream eventlog.StreamID) {
		t.Helper()
		job, discarded, err := LoadDiscarded(ctx, log, "ws", stream)
		if err != nil || !discarded {
			t.Fatalf("discarded=%t err=%v", discarded, err)
		}
		if !job.DiscardedAt.Equal(secondAt) {
			t.Fatalf("discarded at %s, want the current generation's %s", job.DiscardedAt, secondAt)
		}
	}
	t.Run("index", func(t *testing.T) {
		log, stream := seed(t)
		check(t, log, stream)
	})
	t.Run("missingRow", func(t *testing.T) {
		log, stream := seed(t)
		log.DropJobIndexRow("ws", stream)
		check(t, log, stream)
	})
	t.Run("unsupportedReader", func(t *testing.T) {
		log, stream := seed(t)
		check(t, storeOnly{log}, stream)
	})
	t.Run("reenqueuedClearsTheTime", func(t *testing.T) {
		log, stream := seed(t)
		if inserted, err := Enqueue(ctx, log, "ws", "demo", "discard-at", demoArgs{}, 1, secondAt.Add(time.Minute)); err != nil || !inserted {
			t.Fatalf("inserted=%t err=%v", inserted, err)
		}
		job, discarded, err := LoadDiscarded(ctx, log, "ws", stream)
		if err != nil || discarded || !job.DiscardedAt.IsZero() {
			t.Fatalf("a new generation kept a discard: discarded=%t at=%s err=%v", discarded, job.DiscardedAt, err)
		}
		folded, err := Load(ctx, log, "ws", stream)
		if err != nil || !folded.DiscardedAt.IsZero() {
			t.Fatalf("fold of a new generation kept a discard time %s err=%v", folded.DiscardedAt, err)
		}
	})
}
