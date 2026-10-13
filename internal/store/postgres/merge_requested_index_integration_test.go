package postgres

import (
	"encoding/json"
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
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+monitorInFlightMergeQuery, workspace, "conveyor", now.Add(-24*time.Hour)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 || plans[0].Plan == nil {
		t.Fatalf("invalid JSON plan: %s err=%v", raw, err)
	}
	assertMergeRequestedIndexPlan(t, plans[0].Plan)
}

func assertMergeRequestedIndexPlan(t *testing.T, root map[string]any) {
	t.Helper()

	const indexName = "events_merge_requested_inflight_idx"
	requiredIndexConditions := []string{
		"workspace_id",
		"payload_json ->> 'repository'",
		"at >",
	}
	foundIndex := false
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		switch node["Node Type"] {
		case "Sort", "Incremental Sort":
			t.Fatalf("EXPLAIN plan unexpectedly sorts:\n%+v", root)
		}
		if node["Index Name"] == indexName {
			foundIndex = true
			indexCond, _ := node["Index Cond"].(string)
			for _, term := range requiredIndexConditions {
				if !strings.Contains(indexCond, term) {
					t.Fatalf("index %s has Index Cond %q missing %q:\n%+v", indexName, indexCond, term, root)
				}
			}
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			childNode, ok := child.(map[string]any)
			if !ok {
				t.Fatalf("invalid child plan node: %+v", child)
			}
			visit(childNode)
		}
	}
	visit(root)
	if !foundIndex {
		t.Fatalf("EXPLAIN did not use %s:\n%+v", indexName, root)
	}
}
