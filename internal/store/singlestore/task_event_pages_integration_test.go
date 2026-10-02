package singlestore

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestTaskEventWindowDelayedCommitIntegration reverses commit order on
// SingleStore: a lower event ID commits after a higher one was captured, and
// the single-statement recount refuses the next window with the restart
// diagnostic (component-mcp-protocol v14 MCP-READ-9/MCP-READ-10).
func TestTaskEventWindowDelayedCommitIntegration(t *testing.T) {
	st := integrationStore(t)
	ws := "event-window-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), ws)
	cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: ws, Repo: "repo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	task.Branch = "conveyor/task-" + task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC)
	insert := "INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at) VALUES (?, ?, 'test.delayed', 'fixture', 'system', '{}', ?)"
	for i := 0; i < 3; i++ {
		if _, err := st.db.ExecContext(ctx, insert, ws, task.ID, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	delayed, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = delayed.Rollback() }()
	result, err := delayed.ExecContext(ctx, insert, ws, task.ID, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	lowID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = st.db.ExecContext(ctx, insert, ws, task.ID, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	highID, err := result.LastInsertId()
	if err != nil {
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
	if err = delayed.Commit(); err != nil {
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
}
