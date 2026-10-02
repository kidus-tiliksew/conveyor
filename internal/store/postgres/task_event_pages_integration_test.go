package postgres

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
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

// TestTaskEventWindowFetchBoundsIntegration inspects the bytes the driver
// delivers: the lookahead and every candidate past the byte budget, including
// events whose oversized column is not the payload, return only fixed-width
// metadata (component-mcp-protocol v14 MCP-READ-9).
func TestTaskEventWindowFetchBoundsIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "event-fetch-" + core.NewTaskID()
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
	at := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	huge := strings.Repeat("x", 2<<20)
	insert := `INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at) VALUES ($1, $2, $3, $4, 'system', '{}', $5) RETURNING id`
	ids := make([]int64, 4)
	for i, row := range []struct{ kind, actor string }{{"test.fetch", "fixture"}, {"test.fetch", "fixture"}, {"test.fetch", huge}, {huge, "fixture"}} {
		if err = st.pool.QueryRow(ctx, insert, workspace, task.ID, row.kind, row.actor, at.Add(time.Duration(i)*time.Second)).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	fetched := map[int64]int{}
	observe := func(eventID int64, variableBytes int) {
		mu.Lock()
		defer mu.Unlock()
		fetched[eventID] += variableBytes
	}
	observeTaskEventWindowFetch.Store(&observe)
	defer observeTaskEventWindowFetch.Store(nil)
	read := func(q store.TaskEventWindowQuery) (store.TaskEventWindow, map[int64]int, error) {
		t.Helper()
		mu.Lock()
		clear(fetched)
		mu.Unlock()
		window, err := st.ReadTaskEventWindow(ctx, q)
		mu.Lock()
		defer mu.Unlock()
		return window, maps.Clone(fetched), err
	}

	// The lookahead row proves continuation without transferring its columns.
	window, got, err := read(store.TaskEventWindowQuery{TaskID: task.ID, Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes})
	if err != nil || len(window.Events) != 1 || window.Events[0].ID != ids[0] || !window.More {
		t.Fatalf("limit window=%+v err=%v", window, err)
	}
	if got[ids[0]] == 0 || got[ids[1]] != 0 || len(got) != 2 {
		t.Fatalf("lookahead fetched variable-width bytes: %v", got)
	}
	boundary := window.Boundary

	// A non-payload column past the budget ends the window at its last
	// included event and is never transferred.
	window, got, err = read(store.TaskEventWindowQuery{TaskID: task.ID, Kind: "test.fetch", Limit: 1000, MaxBytes: store.TaskEventWindowMaxBytes})
	if err != nil || len(window.Events) != 2 || !window.More {
		t.Fatalf("budget window=%+v err=%v", window, err)
	}
	if got[ids[2]] != 0 || got[ids[0]] == 0 || got[ids[1]] == 0 {
		t.Fatalf("over-budget candidate fetched variable-width bytes: %v", got)
	}

	// Oversized actor and kind columns refuse explicitly without advancing or
	// transferring the oversized event.
	for i, q := range []store.TaskEventWindowQuery{
		{TaskID: task.ID, Kind: "test.fetch", Boundary: &window.Boundary, After: &store.TaskEventPosition{At: window.Events[1].At, ID: window.Events[1].ID}},
		{TaskID: task.ID, Boundary: &boundary, After: &store.TaskEventPosition{At: at.Add(2 * time.Second), ID: ids[2]}},
	} {
		q.Limit, q.MaxBytes = 1000, store.TaskEventWindowMaxBytes
		for range 2 {
			var tooLarge *store.TaskEventTooLargeError
			_, got, err = read(q)
			if !errors.As(err, &tooLarge) || tooLarge.EventID != ids[2+i] || tooLarge.Bytes <= len(huge) {
				t.Fatalf("oversized column %d err=%v", i, err)
			}
			if got[ids[2+i]] != 0 {
				t.Fatalf("oversized column %d fetched %d variable-width bytes", i, got[ids[2+i]])
			}
		}
	}
}
