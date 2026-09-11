package postgres

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// Preserve the pre-optimization query as an independent pagination oracle.
const originalDocumentEventPageSQL = `WITH matched AS MATERIALIZED (
 SELECT e.id,e.at FROM events e WHERE e.workspace_id=$1 AND ($4::bigint=0 OR e.id<=$4) AND (
 ($2='requirement' AND ((e.task_id IS NULL AND e.payload_json->>'requirement_id'=$3) OR EXISTS (
 SELECT 1 FROM task_context_proposals p WHERE p.workspace_id=$1 AND p.task_id=e.task_id AND p.target_kind='requirement' AND p.target_id=$3 AND p.state='confirmed')))
 OR ($2='system_design' AND e.kind LIKE 'system_design.%' AND e.payload_json->>'document_id'=$3))
 ), selected AS (
 SELECT id,at FROM matched ORDER BY at DESC,id DESC LIMIT $5 OFFSET $6
 ) SELECT (SELECT count(*) FROM matched),
 CASE WHEN $4::bigint>0 THEN $4 ELSE COALESCE((SELECT max(id) FROM matched),0) END,
 COALESCE((SELECT jsonb_agg(jsonb_build_object('id',e.id,'task_id',COALESCE(e.task_id,''),'job_id',COALESCE(e.job_id,''),
 'kind',e.kind,'actor_id',e.actor_id,'actor_role',e.actor_role,'payload',e.payload_json,'at',e.at) ORDER BY e.at DESC,e.id DESC)
 FROM selected p JOIN events e ON e.id=p.id AND e.workspace_id=$1),'[]'::jsonb)`

func TestDocumentEventQueryPlansIntegration(t *testing.T) {
	st, ctx, ws := newPhase61IntegrationStore(t)
	defer st.Close()
	fixture := storetest.SeedDocumentEventMembership(t, st, ctx)
	count := 0
	if raw := os.Getenv("CONVEYOR_DOCUMENT_EVENT_MEASUREMENT"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		if err != nil || count < 100000 {
			t.Fatal("measurement needs at least 100000 noise events")
		}
	}
	if count > 0 {
		task := phase61Task(ws, "a-noise-"+core.NewTaskID(), core.TaskRunning, "")
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		// Use the public append API; this fixture never writes ledger rows with SQL.
		for i := 0; i < count; i++ {
			if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "fixture.activity", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Payload: core.JSONPayload(map[string]any{"index": i})}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.pool.Exec(ctx, "ANALYZE events; ANALYZE task_context_proposals"); err != nil {
			t.Fatal(err)
		}
		var actual int
		if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE workspace_id=$1", ws).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		t.Logf("measurement workspace events=%d", actual)
	}
	for _, kind := range []core.LineageNodeType{core.LineageRequirement, core.LineageSystemDesign} {
		id, query := fixture.Requirement, requirementDocumentEventPageSQL
		if kind == core.LineageSystemDesign {
			id, query = fixture.Design, systemDesignDocumentEventPageSQL
		}
		first, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []store.DocumentEventQuery{{Limit: 50}, {Limit: 3, Offset: 1}, {Limit: 3, Offset: 2, SnapshotID: first.SnapshotID}, {Limit: 3, SnapshotID: first.SnapshotID - 1}, {Limit: 3, Offset: 999, SnapshotID: first.SnapshotID}} {
			var total int
			var snapshot int64
			var payload []byte
			err := st.pool.QueryRow(ctx, originalDocumentEventPageSQL, ws, string(kind), id, q.SnapshotID, q.Limit, q.Offset).Scan(&total, &snapshot, &payload)
			if err != nil {
				t.Fatal(err)
			}
			var expected []core.Event
			if err := json.Unmarshal(payload, &expected); err != nil {
				t.Fatal(err)
			}
			got, err := st.ListDocumentEventPage(ctx, kind, id, q)
			if err != nil || got.Total != total || got.SnapshotID != snapshot || !reflect.DeepEqual(got.Events, expected) {
				t.Fatalf("%s query=%+v changed results: %+v err=%v", kind, q, got, err)
			}
		}
		if count > 0 {
			for _, variant := range []struct {
				name, sql string
				args      []any
			}{
				{"before", originalDocumentEventPageSQL, []any{ws, string(kind), id, int64(0), 50, 0}},
				{"after", query, []any{ws, id, int64(0), 50, 0}},
				{"after pinned offset", query, []any{ws, id, first.SnapshotID, 3, 2}},
			} {
				rows, err := st.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+variant.sql, variant.args...)
				if err != nil {
					t.Fatal(err)
				}
				var lines []string
				for rows.Next() {
					var line string
					if err := rows.Scan(&line); err != nil {
						t.Fatal(err)
					}
					lines = append(lines, line)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				plan := strings.Join(lines, "\n")
				t.Logf("%s %s:\n%s", kind, variant.name, plan)
				if strings.HasPrefix(variant.name, "after") && (strings.Contains(plan, "JIT:") || strings.Contains(plan, "Seq Scan on events")) {
					t.Fatalf("document query still scans workspace or compiles JIT:\n%s", plan)
				}
				if strings.HasPrefix(variant.name, "after") {
					var raw []byte
					if err := st.pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+variant.sql, variant.args...).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var plans []struct{ Plan map[string]any }
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatalf("invalid JSON plan: %s err=%v", raw, err)
					}
					assertDocumentEventScanBound(t, plans[0].Plan, first.Total)
				}
			}
		}
	}
}

// An index scan without a task/document condition can still walk all history.
// Count visited event rows as well as returned rows to catch that regression.
// Permit a small indexed snapshot range: PostgreSQL may read the first few IDs
// more cheaply than looking up a task, without scanning workspace history.
func assertDocumentEventScanBound(t *testing.T, node map[string]any, members int) {
	t.Helper()
	if node["Relation Name"] == "events" {
		number := func(key string) float64 { value, _ := node[key].(float64); return value }
		visited := (number("Actual Rows") + number("Rows Removed by Filter") + number("Rows Removed by Index Recheck")) * number("Actual Loops")
		if visited > float64(max(store.MaxTaskOperationsLimit, members)) {
			t.Fatalf("event scan visited %.0f rows for %d members: %+v", visited, members, node)
		}
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		assertDocumentEventScanBound(t, child.(map[string]any), members)
	}
}
