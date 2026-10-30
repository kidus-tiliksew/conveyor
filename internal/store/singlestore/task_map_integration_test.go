package singlestore

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

const (
	taskMapSelectedBodySentinel     = "SINGLESTORE-TASK-MAP-BODY-SENTINEL"
	taskMapSelectedSnapshotSentinel = "SINGLESTORE-TASK-MAP-SNAPSHOT-SENTINEL"
)

// selectedBytesRecorder inspects every value the database returns to the
// store and records each query whose result rows carry a sentinel, so a test
// proves which bytes a read selects rather than only what it serializes.
type selectedBytesRecorder struct {
	mu        sync.Mutex
	recording bool
	needles   [][]byte
	queries   []string
	hits      map[string]string
	rows      int
	bytes     int
}

func (r *selectedBytesRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording, r.queries, r.hits, r.rows, r.bytes = true, nil, map[string]string{}, 0, 0
}

// totals reports the rows and value bytes returned while recording.
func (r *selectedBytesRecorder) totals() (rows, bytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rows, r.bytes
}

func (r *selectedBytesRecorder) stop() (queries []string, hits map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = false
	return append([]string(nil), r.queries...), r.hits
}

func (r *selectedBytesRecorder) query(sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recording {
		r.queries = append(r.queries, sql)
	}
}

func (r *selectedBytesRecorder) inspect(sql string, values []driver.Value) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recording {
		return
	}
	r.rows++
	for _, value := range values {
		var raw []byte
		switch typed := value.(type) {
		case []byte:
			raw = typed
		case string:
			raw = []byte(typed)
		default:
			continue
		}
		r.bytes += len(raw)
		for _, needle := range r.needles {
			if bytes.Contains(raw, needle) {
				r.hits[string(needle)] = sql
			}
		}
	}
}

type recordingConnector struct {
	driver.Connector
	recorder *selectedBytesRecorder
}

func (c recordingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingConn{Conn: conn, recorder: c.recorder}, nil
}

type recordingConn struct {
	driver.Conn
	recorder *selectedBytesRecorder
}

func (c *recordingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	c.recorder.query(query)
	return &recordingRows{Rows: rows, recorder: c.recorder, query: query}, nil
}

func (c *recordingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &recordingStmt{Stmt: stmt, recorder: c.recorder, query: query}, nil
}

func (c *recordingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, query, args)
}

func (c *recordingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *recordingConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func (c *recordingConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *recordingConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *recordingConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

type recordingStmt struct {
	driver.Stmt
	recorder *selectedBytesRecorder
	query    string
}

func (s *recordingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	s.recorder.query(s.query)
	return &recordingRows{Rows: rows, recorder: s.recorder, query: s.query}, nil
}

func (s *recordingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s *recordingStmt) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := s.Stmt.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type recordingRows struct {
	driver.Rows
	recorder *selectedBytesRecorder
	query    string
}

func (r *recordingRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.recorder.inspect(r.query, dest)
	}
	return err
}

// The authenticated task map read on SingleStore selects no task body, no
// unrelated task history, and no work-order authority snapshot, even when
// the workspace holds body-bearing task.created events, irrelevant history,
// and populated historical snapshots (req-board-map AC-3.1;
// component-persistence). The recorder sees every row value the database
// returns during the request.
func TestTaskMapSelectsNoBodiesHistoriesOrSnapshotsIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "task-map-bytes"
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Map", Email: "owner@example.test", DisplayName: "Owner"}, "task-map-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	human := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	_, version, err := st.CreateRequirement(human, core.Requirement{ID: "req-map-bytes", Title: "Map bytes"}, core.RequirementVersion{Content: "# Map bytes", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The map reads light rows."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(human, "req-map-bytes", version.Version); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat(taskMapSelectedBodySentinel+" ", 64)
	created := time.Now().UTC().Add(-time.Hour)
	newTask := func(id string, state core.TaskState, dependencies ...string) {
		t.Helper()
		created = created.Add(time.Second)
		task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Map " + id, Body: body, State: state, NextStage: core.StageImplement, CreatedAt: created}
		if err := st.CreateTaskWithDependencies(ctx, task, dependencies); err != nil {
			t.Fatal(err)
		}
		// Irrelevant, body-bearing history that no map fold reads.
		if err := st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "agent.note_recorded", ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"text": body})}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateTaskContext(human, id, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{"req-map-bytes"}}}); err != nil {
			t.Fatal(err)
		}
	}
	newTask("map-merged", core.TaskQueued)
	newTask("map-closed", core.TaskQueued)
	newTask("map-open", core.TaskQueued, "map-merged", "map-closed")
	newTask("map-running", core.TaskRunning)
	newTask("map-parked", core.TaskParked)
	newTask("map-unrelated", core.TaskQueued)
	plane := taskops.New(st)
	for id, command := range map[string]core.TaskCommand{"map-merged": core.TaskMergeRecover, "map-unrelated": core.TaskMergeRecover, "map-closed": core.TaskCancel} {
		if _, err = plane.Perform(ctx, id, taskops.Command{Kind: command}); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: "map-parked", Kind: "task.state_changed", ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"command": "triage.park"})}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"map-open", "map-running"} {
		job := core.Job{ID: id + "-job", TaskID: id, Stage: core.StageImplement, StartedAt: time.Now().UTC()}
		if err = st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	served := core.JSONPayload([]map[string]any{{"id": "req-map-bytes", "title": "Map bytes", "content": taskMapSelectedSnapshotSentinel}})
	governance := core.JSONPayload(map[string]any{"designs": []map[string]any{{"id": "component-persistence", "content": strings.Repeat(taskMapSelectedSnapshotSentinel+" ", 64)}}})
	if _, err = sharedDatabase.admin.ExecContext(t.Context(), "UPDATE `"+sharedDatabase.name+"`.work_orders SET served_requirement_snapshot=?, governance_snapshot=? WHERE workspace_id=?", served, governance, workspace); err != nil {
		t.Fatal(err)
	}

	recorder := &selectedBytesRecorder{needles: [][]byte{[]byte(taskMapSelectedBodySentinel), []byte(taskMapSelectedSnapshotSentinel)}}
	connector, err := mysql.NewConnector(sharedDatabase.cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(recordingConnector{Connector: connector, recorder: recorder})
	t.Cleanup(func() { _ = db.Close() })
	recorded := &Store{db: db, log: s2log.New(db)}
	server := httpapi.NewServer(recorded)
	server.Workspaces = recorded
	server.Workspace = workspace

	// The recorder observes selected bytes: a direct history read reports
	// both sentinels.
	recorder.start()
	if _, err = recorded.ListEvents(ctx, "map-open"); err != nil {
		t.Fatal(err)
	}
	if _, err = recorded.listOrders(ctx, recorded.db, []string{"map-open"}); err != nil {
		t.Fatal(err)
	}
	if _, hits := recorder.stop(); len(hits) != 2 {
		t.Fatalf("recorder control hits=%v, want both sentinels", hits)
	}

	for _, query := range []string{"", "&include_finished=true"} {
		recorder.start()
		request := httptest.NewRequest(http.MethodGet, "/v1/task-map?workspace_id="+workspace+query, nil)
		request.Header.Set("Authorization", "Bearer task-map-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		queries, hits := recorder.stop()
		if response.Code != http.StatusOK {
			t.Fatalf("%q status=%d body=%s", query, response.Code, response.Body.String())
		}
		if len(hits) != 0 {
			t.Fatalf("%q task map read selected sentinel bytes: %v", query, hits)
		}
		if len(queries) == 0 {
			t.Fatalf("%q recorder saw no query", query)
		}
		var decoded struct {
			Tasks []struct {
				ID         string `json:"id"`
				NextAction struct {
					Kind string `json:"kind"`
				} `json:"next_action"`
				Requirements []struct {
					ID string `json:"id"`
				} `json:"requirements"`
			} `json:"tasks"`
			Requirements []struct {
				ID              string `json:"id"`
				OpenTaskCount   int    `json:"open_task_count"`
				MergedTaskCount int    `json:"merged_task_count"`
				TotalTaskCount  int    `json:"total_task_count"`
			} `json:"requirements"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		want := 5
		if query != "" {
			want = 6
		}
		if len(decoded.Tasks) != want {
			t.Fatalf("%q tasks=%+v want %d", query, decoded.Tasks, want)
		}
		kinds := map[string]string{}
		for _, item := range decoded.Tasks {
			kinds[item.ID] = item.NextAction.Kind
			if len(item.Requirements) != 1 {
				t.Fatalf("%q task %s requirements=%+v", query, item.ID, item.Requirements)
			}
		}
		if kinds["map-parked"] != "parked" || kinds["map-open"] == "" {
			t.Fatalf("%q next actions=%v", query, kinds)
		}
		if len(decoded.Requirements) != 1 || decoded.Requirements[0].OpenTaskCount != 3 || decoded.Requirements[0].MergedTaskCount != 2 || decoded.Requirements[0].TotalTaskCount != 5 {
			t.Fatalf("%q requirements=%+v", query, decoded.Requirements)
		}
	}
}

// The bounded next-action history and design-pin reads on SingleStore return
// the same rows and value bytes when a task's state-change, pull-request,
// bounce, conflict-episode, and attachment history grows tenfold and its
// payloads grow, with the order set fixed. No payload sentinel reaches the
// store, while a direct history read through the same recorder sees it, and
// each summary equals the full-history fold (component-persistence, Task map
// and next-action batch reads).
func TestNextActionHistorySelectsBoundedRowsIntegration(t *testing.T) {
	const sentinel = "SINGLESTORE-HISTORY-PAYLOAD-SENTINEL"
	st := integrationStore(t)
	workspace := "history-bytes"
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seed := func(id string, rounds, padding int) {
		t.Helper()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: workspace, Title: "History " + id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskApproved, MergeApproval: true, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		orders := []string{id + "-implement-1", id + "-implement-2"}
		for _, order := range orders {
			if err := st.CreateJob(ctx, core.Job{ID: order, TaskID: id, Stage: core.StageImplement, State: core.JobPending}); err != nil {
				t.Fatal(err)
			}
			if err := storetest.CreateWorkOrder(ctx, st, core.WorkOrder{ID: order, TaskID: id, JobID: order, Stage: core.StageImplement, State: core.WorkOrderQueued, LastAttemptOutcome: core.WorkOrderOutcomeReleased, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
		pad := strings.Repeat(sentinel+" ", padding)
		at := now.Add(-time.Hour)
		// The history goes through AppendEvent's row writer in one
		// transaction per task; a SingleStore transaction per event made
		// seeding most of this test's time.
		var history []core.Event
		appendEvent := func(kind string, payload map[string]any) {
			at = at.Add(time.Millisecond)
			history = append(history, core.Event{TaskID: id, Kind: kind, ActorRole: core.ActorSystem, At: at, Payload: core.JSONPayload(payload)})
		}
		heads := func(extra map[string]any) map[string]any {
			out := map[string]any{"approved_head": "a", "new_head": "b", "note": pad}
			for key, value := range extra {
				out[key] = value
			}
			return out
		}
		appendEvent("merge.blocked", heads(nil))
		appendEvent("merge.conflict_fix_dispatched", heads(map[string]any{"work_order_id": orders[0]}))
		for i := 0; i < rounds; i++ {
			appendEvent("task.state_changed", map[string]any{"command": fmt.Sprintf("superseded-%d", i), "note": pad})
			appendEvent("task.state_changed", map[string]any{"from": "queued", "to": "running", "note": pad})
			appendEvent("pull_request.opened", map[string]any{"number": 100 + i, "note": pad})
			appendEvent("pipeline.bounce_limit", map[string]any{"window": i + 1, "note": pad})
			appendEvent("merge.conflict_cleared", map[string]any{"note": pad})
			appendEvent("merge.blocked", heads(nil))
			appendEvent("merge.conflict_dispatch_failed", heads(map[string]any{"next_retry_at": now}))
			appendEvent("merge.conflict_dispatch_exhausted", heads(nil))
			appendEvent(store.TaskContextDesignAdded, map[string]any{"id": "design-history", "version": i + 1, "note": pad})
			appendEvent(store.TaskContextDesignRemoved, map[string]any{"id": "design-history", "note": pad})
		}
		appendEvent("merge.conflict_cleared", map[string]any{"note": pad})
		appendEvent("merge.blocked", heads(nil))
		// The latest uncleared episode grows too: repeated dispatches of the
		// same order and accepted, null, missing, and large invalid failures.
		for i := 0; i < rounds; i++ {
			appendEvent("merge.conflict_fix_dispatched", heads(map[string]any{"work_order_id": orders[1]}))
			appendEvent("merge.conflict_dispatch_failed", heads(map[string]any{"next_retry_at": now.Add(time.Duration(i) * time.Minute)}))
			appendEvent("merge.conflict_dispatch_failed", heads(map[string]any{"next_retry_at": "invalid " + pad}))
			appendEvent("merge.conflict_dispatch_failed", heads(map[string]any{"next_retry_at": nil}))
			missing := heads(nil)
			delete(missing, "note")
			appendEvent("merge.conflict_dispatch_failed", missing)
		}
		appendEvent("merge.conflict_fix_dispatched", heads(map[string]any{"work_order_id": orders[1]}))
		appendEvent("merge.conflict_dispatch_failed", heads(map[string]any{"next_retry_at": now.Add(time.Hour)}))
		appendEvent("pull_request.opened", map[string]any{"number": 7, "note": pad})
		appendEvent("pipeline.bounce_limit", map[string]any{"window": 3, "note": pad})
		appendEvent("task.state_changed", map[string]any{"command": "triage.park", "note": pad})
		appendEvent(store.TaskContextDesignAdded, map[string]any{"id": "design-history", "version": 4, "note": pad})
		if err := st.documentTx(ctx, func(tx *sql.Tx) error {
			for _, event := range history {
				if err := insertEvent(ctx, tx, event); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("history-small-1", 20, 1)
	seed("history-large-1", 200, 10)

	recorder := &selectedBytesRecorder{needles: [][]byte{[]byte(sentinel)}}
	connector, err := mysql.NewConnector(sharedDatabase.cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(recordingConnector{Connector: connector, recorder: recorder})
	t.Cleanup(func() { _ = db.Close() })
	recorded := &Store{db: db, log: s2log.New(db)}

	recorder.start()
	if _, err = recorded.ListEvents(ctx, "history-small-1"); err != nil {
		t.Fatal(err)
	}
	if _, hits := recorder.stop(); len(hits) != 1 {
		t.Fatalf("recorder control hits=%v, want the payload sentinel", hits)
	}
	type observation struct{ queries, rows, bytes int }
	observe := func(id string, rounds int) observation {
		t.Helper()
		recorder.start()
		histories, err := recorded.ListNextActionHistory(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		pins, err := recorded.ListTaskContextDesignPins(ctx, []string{id}, []string{"design-history"})
		if err != nil {
			t.Fatal(err)
		}
		rows, bytes := recorder.totals()
		queries, hits := recorder.stop()
		if len(hits) != 0 {
			t.Fatalf("task %s bounded reads selected payload sentinels: %v", id, hits)
		}
		events, err := st.ListEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		orders, err := st.ListWorkOrdersForTasks(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		got, want := histories[id], store.FoldNextActionHistory(events, orders)
		if got.ParkCommand != want.ParkCommand || got.PullRequest != want.PullRequest || got.BounceCount != want.BounceCount ||
			!sameConflict(store.MergeConflictFromHistory(got.MergeConflict, orders), store.MergeConflictFromHistory(want.MergeConflict, orders)) ||
			got.MergeConflict.Failures != want.MergeConflict.Failures || !slices.Equal(got.MergeConflict.DispatchedOrderIDs, want.MergeConflict.DispatchedOrderIDs) {
			t.Fatalf("task %s history=%+v full fold %+v", id, got, want)
		}
		if _, wantPins := store.ActiveTaskContextReferences(events); pins[id]["design-history"] != 4 || len(pins[id]) != len(wantPins) {
			t.Fatalf("task %s pins=%+v full fold %+v", id, pins[id], wantPins)
		}
		if got.ParkCommand != "triage.park" || got.PullRequest != 7 || got.BounceCount != 3 || len(got.MergeConflict.DispatchedOrderIDs) != 2 || got.MergeConflict.Failures != 3*rounds+1 || len(got.MergeConflict.NextRetryAt) > 37 {
			t.Fatalf("task %s summary=%+v", id, got)
		}
		return observation{queries: len(queries), rows: rows, bytes: bytes}
	}
	small := observe("history-small-1", 20)
	large := observe("history-large-1", 200)
	// Three latest rows, one summary row, two dispatched orders, and one
	// current pin, over three queries.
	if small != large || small.queries != 3 || small.rows != 7 {
		t.Fatalf("bounded reads small=%+v large=%+v, want equal with 3 queries and 7 rows", small, large)
	}
}

// sameConflict compares merge-conflict results by instant, because each
// decode of an offset time builds its own fixed zone.
func sameConflict(a, b nextaction.MergeConflict) bool {
	return a.Open == b.Open && a.Exhausted == b.Exhausted && a.NextRetryAt.Equal(b.NextRetryAt)
}
