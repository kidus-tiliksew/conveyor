package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestMonitorMergeRequestedInFlightIndexIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)

	target := phase61Task(workspace, "merge-index-target-"+core.NewTaskID(), core.TaskRunning, "")
	other := phase61Task(workspace, "merge-index-other-"+core.NewTaskID(), core.TaskRunning, "")
	for _, task := range []core.Task{target, other} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := st.pool.Exec(ctx, `
INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at)
SELECT $1, $2, 'fixture.activity', 'fixture', 'system', jsonb_build_object('index', series),
       $3::timestamptz + series * interval '1 microsecond'
FROM generate_series(1, 2000) AS rows(series)`, workspace, target.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at)
SELECT $1, $2, 'merge.requested', 'fixture', 'system', jsonb_build_object('repository', $4::text),
       $3::timestamptz + series * interval '1 minute'
FROM generate_series(1, 5) AS rows(series)
UNION ALL
SELECT $1, $5, 'merge.requested', 'fixture', 'system', jsonb_build_object('repository', $6::text),
       $3::timestamptz + (series + 5) * interval '1 minute'
FROM generate_series(1, 5) AS rows(series)`, workspace, target.ID, now, "conveyor", other.ID, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, "ANALYZE events"); err != nil {
		t.Fatal(err)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err = tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN (COSTS OFF) "+monitorInFlightMergeQuery, workspace, "conveyor", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if !strings.Contains(plan.String(), "events_merge_requested_inflight_idx") {
		t.Fatalf("EXPLAIN did not use events_merge_requested_inflight_idx:\n%s", plan.String())
	}
}
