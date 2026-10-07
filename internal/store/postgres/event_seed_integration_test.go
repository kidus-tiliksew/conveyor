package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// postgresSeedEvents inserts test-only events with controlled identity
// values. The identity sequence is not advanced, so ordinary appends after the
// seed receive lower IDs, modelling SingleStore's per-aggregator ranges.
func postgresSeedEvents(t *testing.T, st *Store, ctx context.Context, base int64, events []core.Event) []core.Event {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	if base == 0 {
		if err := st.pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) + (1::bigint << 40) FROM events`).Scan(&base); err != nil {
			t.Fatal(err)
		}
	}
	out := make([]core.Event, 0, len(events))
	for _, e := range events {
		if e.ID <= 0 || e.At.IsZero() {
			t.Fatalf("seed event needs a positive rank and a time: %+v", e)
		}
		e.ID = base + e.ID
		if e.Payload == nil {
			e.Payload = core.JSONPayload(struct{}{})
		}
		if e.ActorRole == "" {
			e.ActorID, e.ActorRole = "system", core.ActorSystem
		}
		if _, err := st.pool.Exec(ctx, `INSERT INTO events (id, task_id, job_id, kind, actor_id, actor_role, payload_json, at, workspace_id) OVERRIDING SYSTEM VALUE VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6, $7::jsonb, $8, $9)`, e.ID, e.TaskID, e.JobID, e.Kind, e.ActorID, string(e.ActorRole), string(e.Payload), e.At, ws); err != nil {
			t.Fatal(err)
		}
		e.At = e.At.UTC().Truncate(time.Microsecond)
		out = append(out, e)
	}
	return out
}
