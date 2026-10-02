package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestTaskEventWindowDelayedCommitIntegration reverses commit order: a lower
// event ID commits after a higher one was captured. The next window must
// refuse with the restart diagnostic instead of returning a changed set
// (component-mcp-protocol v14 MCP-READ-9/MCP-READ-10).
func TestTaskEventWindowDelayedCommitIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "event-window-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Model: "operator", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP},
	}}}); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC)
	insert := `INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at) VALUES ($1, $2, 'test.delayed', 'fixture', 'system', '{}', $3) RETURNING id`
	for i := 0; i < 3; i++ {
		if _, err = st.pool.Exec(ctx, insert, workspace, task.ID, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	delayed, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = delayed.Rollback(ctx) }()
	var lowID, highID int64
	// The delayed row has an earlier timestamp than everything captured.
	if err = delayed.QueryRow(ctx, insert, workspace, task.ID, at.Add(-time.Hour)).Scan(&lowID); err != nil {
		t.Fatal(err)
	}
	if err = st.pool.QueryRow(ctx, insert, workspace, task.ID, at.Add(time.Hour)).Scan(&highID); err != nil {
		t.Fatal(err)
	}
	if lowID >= highID {
		t.Fatalf("fixture did not allocate a lower uncommitted ID: low=%d high=%d", lowID, highID)
	}
	query := store.TaskEventWindowQuery{TaskID: task.ID, Kind: "test.delayed", Limit: 2, MaxBytes: store.TaskEventWindowMaxBytes}
	first, err := st.ReadTaskEventWindow(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if first.Boundary.MaxID != highID || first.Boundary.Count != 4 || len(first.Events) != 2 || !first.More {
		t.Fatalf("capture=%+v", first)
	}
	if err = delayed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	last := first.Events[len(first.Events)-1]
	query.Boundary, query.After = &first.Boundary, &store.TaskEventPosition{At: last.At, ID: last.ID}
	for range 2 {
		if _, err = st.ReadTaskEventWindow(ctx, query); !errors.Is(err, store.ErrTaskEventHistoryChanged) || !strings.Contains(err.Error(), "restart read") {
			t.Fatalf("delayed lower-ID commit err=%v", err)
		}
	}
	restarted, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: "test.delayed", Limit: 1000, MaxBytes: store.TaskEventWindowMaxBytes})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Boundary.Count != 5 || len(restarted.Events) != 5 || restarted.Events[0].ID != lowID || restarted.More {
		t.Fatalf("restarted traversal=%+v", restarted)
	}
	// The seek is served by the (task_id, at, id) timeline index rather than a
	// scan of unrelated histories.
	var plan strings.Builder
	rows, err := st.pool.Query(ctx, `EXPLAIN SELECT id FROM events e WHERE e.task_id = $1 AND e.id <= $2 AND (e.at, e.id) > ($3::timestamptz, $4::bigint) ORDER BY e.at, e.id LIMIT 1001`, task.ID, highID, at, lowID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "events_task_timeline_idx") {
		t.Logf("planner chose a different access path for this small fixture:\n%s", plan.String())
	}
}
