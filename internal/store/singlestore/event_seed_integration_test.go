package singlestore

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// singlestoreSeedEvents inserts test-only events with explicit AUTO_INCREMENT
// values far above existing IDs, reproducing the non-monotonic ranges that
// per-aggregator allocation produces in production.
func singlestoreSeedEvents(t *testing.T, st *Store, ctx context.Context, base int64, events []core.Event) []core.Event {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	if base == 0 {
		if err := st.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0)+(1<<40) FROM events`).Scan(&base); err != nil {
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
		e.At = e.At.UTC().Truncate(time.Microsecond)
		if _, err := st.db.ExecContext(ctx, `INSERT INTO events (id,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id) VALUES (?,NULLIF(?,''),NULLIF(?,''),?,?,?,?,?,?)`, e.ID, e.TaskID, e.JobID, e.Kind, e.ActorID, string(e.ActorRole), string(e.Payload), e.At, ws); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}
