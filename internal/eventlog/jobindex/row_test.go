package jobindex_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
)

var base = time.Date(2026, 11, 1, 9, 0, 0, 987654321, time.UTC)

func event(t *testing.T, stream eventlog.StreamID, version eventlog.Version, kind string, payload any) eventlog.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return eventlog.Event{Workspace: "ws", Stream: stream, Version: version, Position: eventlog.Position(version), Kind: kind, Payload: raw, At: base.Add(time.Duration(version) * time.Second)}
}

// everyOutcome is one stream through each event kind the queue appends, over
// two generations.
func everyOutcome(t *testing.T, stream eventlog.StreamID) []eventlog.Event {
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	kinds := []struct {
		kind    string
		payload any
	}{
		{jobindex.KindEnqueued, jobindex.EnqueuedPayload{Kind: "demo", Key: "a", Args: json.RawMessage(`{}`), MaxAttempts: 3}},
		{jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: 1, Worker: "w", ClaimedAt: at(1)}},
		{jobindex.KindFailed, jobindex.OutcomePayload{Attempt: 1, Error: "boom", NextAt: at(2)}},
		{jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: 2, Worker: "w", ClaimedAt: at(2)}},
		{jobindex.KindSnoozed, jobindex.OutcomePayload{Attempt: 2, Until: at(3)}},
		{jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: 2, Worker: "w", ClaimedAt: at(3)}},
		{jobindex.KindRescued, jobindex.OutcomePayload{Attempt: 2, Error: "rescued", NextAt: at(4)}},
		{jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: 3, Worker: "w", ClaimedAt: at(4)}},
		{jobindex.KindCompleted, jobindex.OutcomePayload{Attempt: 3}},
		{jobindex.KindEnqueued, jobindex.EnqueuedPayload{Kind: "demo", Key: "a", Args: json.RawMessage(`{}`), MaxAttempts: 1, ScheduledAt: at(90)}},
		{jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: 1, Worker: "w", ClaimedAt: at(91)}},
		{jobindex.KindDiscarded, jobindex.OutcomePayload{Attempt: 1, Error: "fatal"}},
	}
	out := make([]eventlog.Event, len(kinds))
	for i, k := range kinds {
		out[i] = event(t, stream, eventlog.Version(i+1), k.kind, k.payload)
	}
	return out
}

func TestAdvanceMatchesAFullFoldAfterEveryEvent(t *testing.T) {
	stream := jobindex.StreamFor("demo", "a")
	events := everyOutcome(t, stream)
	row := jobindex.EmptyRow("ws", stream)
	for i := range events {
		next, err := jobindex.Advance(row, events[i:i+1])
		if err != nil {
			t.Fatal(err)
		}
		job, err := jobindex.Fold("ws", stream, events[:i+1])
		if err != nil {
			t.Fatal(err)
		}
		if want := jobindex.RowOf(job); !reflect.DeepEqual(next, want) {
			t.Fatalf("after %s (v%d): advanced row %+v, full fold %+v", events[i].Kind, events[i].Version, next, want)
		}
		row = next
	}
	if row.State != jobindex.StateDiscarded || row.Generation != 2 || row.Version != 12 {
		t.Fatalf("final row %+v", row)
	}
	if row.ClaimedAt.Nanosecond()%1000 != 0 || row.NextAt.Nanosecond()%1000 != 0 {
		t.Fatalf("row times keep sub-microsecond precision: %+v", row)
	}
}

func TestNextFoldsTheWholeStreamWhenTheRowIsMissingOrBehind(t *testing.T) {
	stream := jobindex.StreamFor("demo", "a")
	events := everyOutcome(t, stream)
	full, err := jobindex.Fold("ws", stream, events)
	if err != nil {
		t.Fatal(err)
	}
	want := jobindex.RowOf(full)
	reads := 0
	readAll := func() ([]eventlog.Event, error) { reads++; return events, nil }
	// A row that matches the observed head advances without a read.
	before, _ := jobindex.Fold("ws", stream, events[:11])
	prev := jobindex.RowOf(before)
	if got, err := jobindex.Next("ws", stream, &prev, 11, events[11:], readAll); err != nil || !reflect.DeepEqual(got, want) || reads != 0 {
		t.Fatalf("matching row: %+v err=%v reads=%d", got, err, reads)
	}
	// A missing row on a stream that already had events is rebuilt.
	if got, err := jobindex.Next("ws", stream, nil, 11, events[11:], readAll); err != nil || !reflect.DeepEqual(got, want) || reads != 1 {
		t.Fatalf("missing row: %+v err=%v reads=%d", got, err, reads)
	}
	// A row behind the observed head is rebuilt.
	stale, _ := jobindex.Fold("ws", stream, events[:4])
	behind := jobindex.RowOf(stale)
	if got, err := jobindex.Next("ws", stream, &behind, 11, events[11:], readAll); err != nil || !reflect.DeepEqual(got, want) || reads != 2 {
		t.Fatalf("stale row: %+v err=%v reads=%d", got, err, reads)
	}
	// A fold error is returned, so the driver writes nothing.
	bad := []eventlog.Event{{Workspace: "ws", Stream: stream, Version: 1, Kind: jobindex.KindEnqueued, Payload: json.RawMessage(`{"max_attempts":"x"}`)}}
	if _, err := jobindex.Next("ws", stream, nil, 0, bad, readAll); err == nil {
		t.Fatal("fold error was not returned")
	}
	if _, err := jobindex.Next("ws", stream, nil, 3, bad, func() ([]eventlog.Event, error) { return nil, errors.New("read failed") }); err == nil {
		t.Fatal("read error was not returned")
	}
}

// readCounter counts the events ReadGeneration returns.
type readCounter struct {
	*memlog.Store
	mu       sync.Mutex
	events   int
	earliest eventlog.Version
}

func (r *readCounter) ReadGeneration(ctx context.Context, workspace string, stream eventlog.StreamID, version eventlog.Version) ([]eventlog.Event, error) {
	events, err := r.Store.ReadGeneration(ctx, workspace, stream, version)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events += len(events)
	for _, e := range events {
		if r.earliest == 0 || e.Version < r.earliest {
			r.earliest = e.Version
		}
	}
	return events, err
}

func TestLoadCurrentReadsOnlyTheCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	stream := jobindex.StreamFor("demo", "a")
	appendEvent := func(kind string, payload any) {
		t.Helper()
		raw, _ := json.Marshal(payload)
		if _, err := log.Append(ctx, "ws", stream, eventlog.ExpectAny, []eventlog.NewEvent{{Kind: kind, Payload: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	// Generation 1 is long and finished; generation 2 is still running.
	appendEvent(jobindex.KindEnqueued, jobindex.EnqueuedPayload{Kind: "demo", Key: "a", MaxAttempts: 9})
	for i := 0; i < 30; i++ {
		appendEvent(jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: 1, Worker: "w", ClaimedAt: base})
		appendEvent(jobindex.KindSnoozed, jobindex.OutcomePayload{Attempt: 1, Until: base})
	}
	appendEvent(jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: 1, Worker: "w", ClaimedAt: base})
	appendEvent(jobindex.KindCompleted, jobindex.OutcomePayload{Attempt: 1})
	enqueuedAt := eventlog.Version(64)
	appendEvent(jobindex.KindEnqueued, jobindex.EnqueuedPayload{Kind: "demo", Key: "a", Args: json.RawMessage(`{"n":2}`), MaxAttempts: 4})
	for i := 0; i < 10; i++ {
		appendEvent(jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: i + 1, Worker: "w", ClaimedAt: base})
		appendEvent(jobindex.KindFailed, jobindex.OutcomePayload{Attempt: i + 1, Error: "boom", NextAt: base.Add(time.Minute)})
	}
	row, ok, err := log.JobIndexRow(ctx, "ws", stream)
	if err != nil || !ok {
		t.Fatalf("row ok=%v err=%v", ok, err)
	}
	counter := &readCounter{Store: log}
	job, matched, err := jobindex.LoadCurrent(ctx, counter, row)
	if err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	all, _ := log.Read(ctx, "ws", stream, 0, 0)
	full, err := jobindex.Fold("ws", stream, all)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(job, full) {
		t.Fatalf("current-generation fold %+v, full fold %+v", job, full)
	}
	// Only generation 2 is read: versions 64 through 84.
	if counter.earliest != enqueuedAt || counter.events != len(all)-int(enqueuedAt)+1 {
		t.Fatalf("read %d events from version %d, want the %d events from %d", counter.events, counter.earliest, len(all)-int(enqueuedAt)+1, enqueuedAt)
	}
	// A row behind the head reads only its own generation's prefix.
	if events, err := log.ReadGeneration(ctx, "ws", stream, 62); err != nil || len(events) != 62 || events[0].Version != 1 {
		t.Fatalf("generation 1 read: %d events err=%v", len(events), err)
	}

	// A row whose state or version disagrees with its stream is reported,
	// so the runtime folds the whole stream instead.
	wrongState := row
	wrongState.State = jobindex.StateAvailable
	ahead := row
	ahead.Version += 5
	for name, stale := range map[string]jobindex.Row{"state": wrongState, "version": ahead} {
		if _, matched, err := jobindex.LoadCurrent(ctx, log, stale); err != nil || matched {
			t.Fatalf("a row with a wrong %s matched=%v err=%v", name, matched, err)
		}
	}
}
