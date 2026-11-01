package logtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
)

// testJobIndexMatchesFold drives one job stream through every outcome a
// runtime appends, plus a re-enqueue, a batch, a version conflict, a fold
// error, a job stream whose first event is not an enqueue, and a non-job
// stream. After every append it asserts that each job stream's index row
// equals the row of a full fold, and that UnfinishedJobs returns the
// workspace's position and exactly the unfinished rows
// (component-durable-queue, The job index; req-quiet-idle-runtime AC-5.1).
func testJobIndexMatchesFold(t *testing.T, s eventlog.Store) {
	reader, ok := jobindex.ReaderOf(s)
	if !ok {
		t.Fatalf("%T does not implement jobindex.Reader", s)
	}
	ctx := context.Background()
	w := ws(t)
	main := jobindex.StreamFor("demo", "a")
	other := jobindex.StreamFor("other", "b")
	odd := eventlog.StreamID("job/odd:c")
	task := eventlog.StreamID("task/t1")
	// Nanosecond times prove the index keeps the microseconds a driver stores.
	base := time.Date(2026, 11, 1, 10, 0, 0, 123456789, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	enqueued := func(key string, scheduled time.Time) eventlog.NewEvent {
		return jobEvent(t, jobindex.KindEnqueued, jobindex.EnqueuedPayload{Kind: "demo", Key: key, Args: json.RawMessage(`{"n":1}`), MaxAttempts: 3, ScheduledAt: scheduled}, base)
	}
	claimed := func(attempt, minute int) eventlog.NewEvent {
		return jobEvent(t, jobindex.KindClaimed, jobindex.ClaimedPayload{Attempt: attempt, Worker: "w1", ClaimedAt: at(minute)}, at(minute))
	}
	outcome := func(kind string, payload jobindex.OutcomePayload, minute int) eventlog.NewEvent {
		return jobEvent(t, kind, payload, at(minute))
	}
	streams := []eventlog.StreamID{main, other, odd}
	appendAndCheck := func(step string, stream eventlog.StreamID, expected eventlog.Version, events ...eventlog.NewEvent) {
		t.Helper()
		if _, err := s.Append(ctx, w, stream, expected, events); err != nil {
			t.Fatalf("%s: append: %v", step, err)
		}
		checkJobIndex(t, s, reader, w, step, streams)
	}

	appendAndCheck("enqueue", main, eventlog.ExpectNew, enqueued("a", time.Time{}))
	appendAndCheck("claim", main, 1, claimed(1, 1))
	appendAndCheck("fail with retry", main, 2, outcome(jobindex.KindFailed, jobindex.OutcomePayload{Attempt: 1, Error: "boom", NextAt: at(2)}, 1))
	appendAndCheck("claim retry", main, 3, claimed(2, 2))
	appendAndCheck("snooze", main, 4, outcome(jobindex.KindSnoozed, jobindex.OutcomePayload{Attempt: 2, Until: at(3)}, 2))
	appendAndCheck("claim after snooze", main, 5, claimed(2, 3))
	appendAndCheck("rescue", main, 6, outcome(jobindex.KindRescued, jobindex.OutcomePayload{Attempt: 2, Error: "rescued", NextAt: at(5)}, 4))
	appendAndCheck("claim after rescue", main, 7, claimed(3, 5))
	appendAndCheck("complete", main, 8, outcome(jobindex.KindCompleted, jobindex.OutcomePayload{Attempt: 3}, 6))
	appendAndCheck("other stream enqueue", other, eventlog.ExpectNew, enqueued("b", time.Time{}))
	appendAndCheck("re-enqueue after completion", main, 9, enqueued("a", time.Time{}))
	appendAndCheck("claim and discard in one batch", main, 10, claimed(1, 7), outcome(jobindex.KindDiscarded, jobindex.OutcomePayload{Attempt: 1, Error: "fatal"}, 7))
	appendAndCheck("scheduled enqueue", main, 12, enqueued("a", base.Add(time.Hour)))
	appendAndCheck("job stream whose first event is not an enqueue", odd, eventlog.ExpectNew, ev("note"))
	appendAndCheck("non-job stream", task, eventlog.ExpectNew, ev("task.created"))
	appendAndCheck("expect-any append", other, eventlog.ExpectAny, claimed(1, 8))

	before, _, _ := reader.JobIndexRow(ctx, w, main)
	if _, err := s.Append(ctx, w, main, 5, []eventlog.NewEvent{claimed(9, 9)}); !eventlog.IsVersionConflict(err) {
		t.Fatalf("stale append err=%v, want a version conflict", err)
	}
	checkJobIndex(t, s, reader, w, "version conflict", streams)
	if _, err := s.Append(ctx, w, main, 13, []eventlog.NewEvent{{Kind: jobindex.KindEnqueued, Payload: json.RawMessage(`{"max_attempts":"many"}`)}}); err == nil {
		t.Fatal("an append the fold cannot apply succeeded, want an error")
	}
	if head, err := s.Head(ctx, w, main); err != nil || head != 13 {
		t.Fatalf("head after refused append=%d err=%v, want 13", head, err)
	}
	after, _, _ := reader.JobIndexRow(ctx, w, main)
	if !sameRow(before, after) {
		t.Fatalf("refused appends changed the row: %+v -> %+v", before, after)
	}
	checkJobIndex(t, s, reader, w, "fold error", streams)
	if _, ok, err := reader.JobIndexRow(ctx, w, task); err != nil || ok {
		t.Fatalf("non-job stream row ok=%v err=%v, want none", ok, err)
	}
	if repaired, err := reader.RepairJobIndex(ctx, w); err != nil || repaired != 0 {
		t.Fatalf("repair of a matching index=%d err=%v, want 0", repaired, err)
	}
	if position, rows, err := reader.UnfinishedJobs(ctx, ws(t)+"-empty"); err != nil || position != 0 || len(rows) != 0 {
		t.Fatalf("empty workspace position=%d rows=%v err=%v", position, rows, err)
	}
}

func jobEvent(t *testing.T, kind string, payload any, at time.Time) eventlog.NewEvent {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return eventlog.NewEvent{Kind: kind, ActorID: "queue", ActorRole: "system", Payload: raw, At: at}
}

func checkJobIndex(t *testing.T, s eventlog.Store, reader jobindex.Reader, w, step string, streams []eventlog.StreamID) {
	t.Helper()
	ctx := context.Background()
	var want []jobindex.Row
	for _, stream := range streams {
		events, err := s.Read(ctx, w, stream, 0, 0)
		if err != nil {
			t.Fatalf("%s: read %s: %v", step, stream, err)
		}
		row, ok, err := reader.JobIndexRow(ctx, w, stream)
		if err != nil {
			t.Fatalf("%s: index row %s: %v", step, stream, err)
		}
		if len(events) == 0 {
			if ok {
				t.Fatalf("%s: %s has no events but an index row %+v", step, stream, row)
			}
			continue
		}
		job, err := jobindex.Fold(w, stream, events)
		if err != nil {
			t.Fatalf("%s: fold %s: %v", step, stream, err)
		}
		folded := jobindex.RowOf(job)
		if !ok || !sameRow(row, folded) {
			t.Fatalf("%s: %s index row=%+v (present %v), full fold=%+v", step, stream, row, ok, folded)
		}
		generation, err := reader.ReadGeneration(ctx, w, stream, folded.Version)
		if err != nil {
			t.Fatalf("%s: read generation %s: %v", step, stream, err)
		}
		if got, wantVersions := versions(generation), versions(jobindex.CurrentGeneration(events, folded.Version)); fmt.Sprint(got) != fmt.Sprint(wantVersions) {
			t.Fatalf("%s: %s current generation versions=%v, want %v", step, stream, got, wantVersions)
		}
		if jobindex.Unfinished(folded.State) {
			want = append(want, folded)
		}
	}
	tail, err := s.Tail(ctx, w, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	position, rows, err := reader.UnfinishedJobs(ctx, w)
	if err != nil {
		t.Fatalf("%s: unfinished jobs: %v", step, err)
	}
	if last := tail[len(tail)-1].Position; position != last {
		t.Fatalf("%s: unfinished position=%d, want head %d", step, position, last)
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Stream < want[j].Stream })
	if len(rows) != len(want) {
		t.Fatalf("%s: unfinished rows=%+v, want %+v", step, rows, want)
	}
	for i := range rows {
		if !sameRow(rows[i], want[i]) {
			t.Fatalf("%s: unfinished row %d=%+v, want %+v", step, i, rows[i], want[i])
		}
	}
}

func sameRow(a, b jobindex.Row) bool {
	return a.Workspace == b.Workspace && a.Stream == b.Stream && a.Kind == b.Kind && a.State == b.State &&
		a.Generation == b.Generation && a.Version == b.Version && a.NextAt.Equal(b.NextAt) && a.ClaimedAt.Equal(b.ClaimedAt)
}
