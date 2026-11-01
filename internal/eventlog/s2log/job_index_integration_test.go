package s2log

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
)

// TestSingleStoreFillJobIndexMatchesFold: job streams in every state,
// including several generations, lose their index rows or keep a stale
// one, as before migration 0030. The fill writes one row per job stream
// equal to a full fold, a rerun writes the same rows, a newer row is never
// moved back, and RepairJobIndex then finds nothing to rebuild
// (component-durable-queue, The job index; req-quiet-idle-runtime AC-5.1).
func TestSingleStoreFillJobIndexMatchesFold(t *testing.T) {
	db := integrationDB(t)
	st := New(db)
	ctx := t.Context()
	ws := t.Name() + time.Now().Format("150405.000000000")
	base := time.Date(2026, 11, 1, 12, 0, 0, 123456789, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	put := func(key string, expected eventlog.Version, kind string, payload any) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, ws, jobindex.StreamFor("demo", key), expected, []eventlog.NewEvent{{Kind: kind, ActorID: "test", ActorRole: "system", Payload: raw, At: at(0)}}); err != nil {
			t.Fatal(err)
		}
	}
	enq := func(key string, v eventlog.Version, scheduled time.Time) {
		put(key, v, jobindex.KindEnqueued, jobindex.EnqueuedPayload{Kind: "demo", Key: key, Args: json.RawMessage(`{}`), MaxAttempts: 3, ScheduledAt: scheduled})
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
	put("generations", 5, jobindex.KindDiscarded, jobindex.OutcomePayload{Attempt: 1, Error: "fatal"})
	enq("generations", 6, time.Time{})
	if _, err := st.Append(ctx, ws, "task/not-a-job", eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "task.created"}}); err != nil {
		t.Fatal(err)
	}
	keys := []string{"available", "scheduled-enqueue", "retried", "snoozed", "rescued", "running", "completed", "discarded", "generations"}

	if _, err := db.ExecContext(ctx, `DELETE FROM event_log_job_index WHERE workspace_id = ?`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO event_log_job_index (workspace_id, stream_id, kind, state, generation, version) VALUES (?, 'job/demo:completed', 'demo', 'available', 1, 1)`, ws); err != nil {
		t.Fatal(err)
	}
	check := func(step string) {
		t.Helper()
		for _, key := range keys {
			stream := jobindex.StreamFor("demo", key)
			events, err := st.Read(ctx, ws, stream, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			job, err := jobindex.Fold(ws, stream, events)
			if err != nil {
				t.Fatal(err)
			}
			want := jobindex.RowOf(job)
			row, ok, err := st.JobIndexRow(ctx, ws, stream)
			if err != nil || !ok || row.Kind != want.Kind || row.State != want.State || row.Generation != want.Generation ||
				row.Version != want.Version || !row.NextAt.Equal(want.NextAt) || !row.ClaimedAt.Equal(want.ClaimedAt) {
				t.Fatalf("%s: %s row=%+v ok=%v err=%v, full fold=%+v", step, key, row, ok, err, want)
			}
		}
		if _, ok, err := st.JobIndexRow(ctx, ws, "task/not-a-job"); err != nil || ok {
			t.Fatalf("%s: a non-job stream has a row (err %v)", step, err)
		}
	}
	filled, err := fillJobIndex(ctx, db, ws)
	if err != nil || filled != len(keys) {
		t.Fatalf("filled=%d err=%v, want %d", filled, err, len(keys))
	}
	check("fill")
	if _, err := fillJobIndex(ctx, db, ws); err != nil {
		t.Fatal(err)
	}
	check("rerun")
	if _, err := db.ExecContext(ctx, `UPDATE event_log_job_index SET version = 999 WHERE workspace_id = ? AND stream_id = 'job/demo:available'`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := fillJobIndex(ctx, db, ws); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := st.JobIndexRow(ctx, ws, jobindex.StreamFor("demo", "available")); row.Version != 999 {
		t.Fatalf("fill moved a newer row back: %+v", row)
	}
	if _, err := db.ExecContext(ctx, `UPDATE event_log_job_index SET version = 1 WHERE workspace_id = ? AND stream_id = 'job/demo:available'`, ws); err != nil {
		t.Fatal(err)
	}
	if repaired, err := st.RepairJobIndex(ctx, ws); err != nil || repaired != 0 {
		t.Fatalf("repair after fill=%d err=%v, want 0", repaired, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM event_log_job_index WHERE workspace_id = ? AND stream_id = 'job/demo:running'`, ws); err != nil {
		t.Fatal(err)
	}
	if repaired, err := st.RepairJobIndex(ctx, ws); err != nil || repaired != 1 {
		t.Fatalf("repair of a deleted row=%d err=%v, want 1", repaired, err)
	}
	check("repair")
	_, unfinished, err := st.UnfinishedJobs(context.Background(), ws)
	if err != nil || len(unfinished) != 7 {
		t.Fatalf("unfinished=%d err=%v, want 7", len(unfinished), err)
	}
}
