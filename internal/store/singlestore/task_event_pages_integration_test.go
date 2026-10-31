package singlestore

import (
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

// TestTaskEventWindowDelayedCommitIntegration reverses commit order on
// SingleStore: a lower event ID commits after a higher one was captured, and
// the single-statement recount refuses the next window with the restart
// diagnostic, for ascending and descending windows and for an exact kind and a
// kind set (component-mcp-investigation-reads; task 261015-3721c6).
func TestTaskEventWindowDelayedCommitIntegration(t *testing.T) {
	st := integrationStore(t)
	ws := "event-window-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
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
	insert := "INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at, event_shard_key) VALUES (?, ?, 'test.delayed', 'fixture', 'system', '{}', ?, ?)"
	for i := 0; i < 3; i++ {
		if _, err := st.db.ExecContext(ctx, insert, ws, task.ID, at.Add(time.Duration(i)*time.Second), task.ID); err != nil {
			t.Fatal(err)
		}
	}
	delayed, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = delayed.Rollback() }()
	result, err := delayed.ExecContext(ctx, insert, ws, task.ID, at.Add(-time.Hour), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	lowID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = st.db.ExecContext(ctx, insert, ws, task.ID, at.Add(time.Hour), task.ID)
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
	descending := []store.TaskEventWindowQuery{
		{TaskID: task.ID, Kind: "test.delayed", Descending: true, Limit: 2, MaxBytes: store.TaskEventWindowMaxBytes},
		{TaskID: task.ID, Kinds: []string{"test.delayed", "test.absent"}, Descending: true, Limit: 2, MaxBytes: store.TaskEventWindowMaxBytes},
	}
	for i, q := range descending {
		newest, err := st.ReadTaskEventWindow(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if newest.Boundary != first.Boundary || len(newest.Events) != 2 || newest.Events[1].ID != highID || !newest.More {
			t.Fatalf("descending capture=%+v", newest)
		}
		descending[i].Boundary, descending[i].Before = &newest.Boundary, &store.TaskEventPosition{At: newest.Events[0].At, ID: newest.Events[0].ID}
	}
	if err = delayed.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, q := range descending {
		for range 2 {
			if _, err = st.ReadTaskEventWindow(ctx, q); !errors.Is(err, store.ErrTaskEventHistoryChanged) || !strings.Contains(err.Error(), "restart read") {
				t.Fatalf("descending delayed lower-ID commit err=%v", err)
			}
		}
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
	descending[0].Boundary, descending[0].Before, descending[0].Limit = nil, nil, 1000
	if restarted, err = st.ReadTaskEventWindow(ctx, descending[0]); err != nil || restarted.Boundary.Count != 5 || len(restarted.Events) != 5 || restarted.Events[0].ID != lowID || restarted.More {
		t.Fatalf("restarted descending traversal=%+v err=%v", restarted, err)
	}
}

// TestTaskEventWindowFetchBoundsIntegration inspects the bytes the driver
// delivers: the lookahead and every candidate past the byte budget, including
// events whose oversized column is not the payload, return only fixed-width
// metadata, and a payload a PayloadLimit omits is never transferred, including
// by the one-row notify fingerprint (component-mcp-investigation-reads; task
// 261015-3721c6).
func TestTaskEventWindowFetchBoundsIntegration(t *testing.T) {
	st := integrationStore(t)
	ws := "event-fetch-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: ws, Repo: "repo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	task.Branch = "conveyor/task-" + task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	huge := strings.Repeat("x", 2<<20)
	insert := "INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at, event_shard_key) VALUES (?, ?, ?, ?, 'system', '{}', ?, ?)"
	ids := make([]int64, 4)
	for i, row := range []struct{ kind, actor string }{{"test.fetch", "fixture"}, {"test.fetch", "fixture"}, {"test.fetch", huge}, {huge, "fixture"}} {
		result, err := st.db.ExecContext(ctx, insert, ws, task.ID, row.kind, row.actor, at.Add(time.Duration(i)*time.Second), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ids[i], err = result.LastInsertId(); err != nil {
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
	// A payload over the limit is omitted without transfer, even one larger
	// than the whole window budget, and its stored length is reported.
	payloadTask := core.Task{ID: core.NewTaskID(), Workspace: ws, Repo: "repo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	payloadTask.Branch = "conveyor/task-" + payloadTask.ID
	if err := st.CreateTask(ctx, payloadTask); err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	payloadIDs := make([]int64, 2)
	small := `{"n":1}`
	for i, payload := range []string{`{"blob":"` + strings.Repeat("y", 2<<20) + `"}`, small} {
		result, err := st.db.ExecContext(ctx, "INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at, event_shard_key) VALUES (?, ?, 'test.payload', 'fixture', 'system', ?, ?, ?)", ws, payloadTask.ID, payload, later.Add(time.Duration(i)*time.Second), payloadTask.ID)
		if err != nil {
			t.Fatal(err)
		}
		if payloadIDs[i], err = result.LastInsertId(); err != nil {
			t.Fatal(err)
		}
	}
	var storedBytes, smallBytes int
	if err := st.db.QueryRowContext(ctx, "SELECT LENGTH(payload_json) FROM events WHERE workspace_id = ? AND id = ?", ws, payloadIDs[0]).Scan(&storedBytes); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, "SELECT LENGTH(payload_json) FROM events WHERE workspace_id = ? AND id = ?", ws, payloadIDs[1]).Scan(&smallBytes); err != nil {
		t.Fatal(err)
	}
	columnBytes := len(payloadTask.ID) + len("test.payload") + len("fixture") + len("system")
	payloadQuery := store.TaskEventWindowQuery{TaskID: payloadTask.ID, Kind: "test.payload", Limit: 1000, MaxBytes: store.TaskEventWindowMaxBytes}
	for range 2 {
		var tooLarge *store.TaskEventTooLargeError
		if _, got, err = read(payloadQuery); !errors.As(err, &tooLarge) || tooLarge.EventID != payloadIDs[0] || got[payloadIDs[0]] != 0 {
			t.Fatalf("unlimited oversized payload err=%v fetched=%v", err, got)
		}
	}
	payloadQuery.PayloadLimit = 1024
	window, got, err = read(payloadQuery)
	if err != nil || len(window.Events) != 2 || window.More || window.Events[0].ID != payloadIDs[0] || window.Events[0].Payload != nil || len(window.Events[1].Payload) != smallBytes {
		t.Fatalf("payload-limited window=%+v err=%v", window, err)
	}
	if len(window.OmittedPayloadBytes) != 1 || window.OmittedPayloadBytes[payloadIDs[0]] != storedBytes || storedBytes <= 2<<20 {
		t.Fatalf("omitted payload bytes=%v want %d:%d", window.OmittedPayloadBytes, payloadIDs[0], storedBytes)
	}
	if got[payloadIDs[0]] != columnBytes || got[payloadIDs[1]] != columnBytes+smallBytes {
		t.Fatalf("omitted payload transferred: fetched=%v want %d without payload", got, columnBytes)
	}

	// The notify fingerprint transfers one row without its payload; the
	// lookahead row transfers only fixed-width metadata.
	fingerprint := store.TaskEventWindowQuery{TaskID: payloadTask.ID, Descending: true, Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes, PayloadLimit: 1}
	window, got, err = read(fingerprint)
	if err != nil || window.Boundary != (store.TaskEventBoundary{MaxID: payloadIDs[1], Count: 3}) || len(window.Events) != 1 || window.Events[0].ID != payloadIDs[1] || window.Events[0].Payload != nil || !window.More {
		t.Fatalf("fingerprint=%+v err=%v", window, err)
	}
	if len(got) != 2 || got[payloadIDs[1]] != columnBytes || got[payloadIDs[0]] != 0 {
		t.Fatalf("fingerprint fetched=%v want only %d:%d", got, payloadIDs[1], columnBytes)
	}

	// Every task writer records task.created, so an empty ledger is seeded as
	// a bare task row; its fingerprint is the zero boundary.
	emptyID := core.NewTaskID()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO tasks
		(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval)
		VALUES(?,?,'test','empty ledger','','','L2','repo','main',?,?,?,'',true,true)`,
		emptyID, ws, "conveyor/task-"+emptyID, core.TaskRunning, later); err != nil {
		t.Fatal(err)
	}
	fingerprint.TaskID = emptyID
	window, got, err = read(fingerprint)
	if err != nil || window.Boundary != (store.TaskEventBoundary{}) || len(window.Events) != 0 || window.More || window.OmittedPayloadBytes != nil || len(got) != 0 {
		t.Fatalf("empty ledger fingerprint=%+v fetched=%v err=%v", window, got, err)
	}
}
