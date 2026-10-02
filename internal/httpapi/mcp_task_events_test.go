package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// mcpWindowStore observes every bounded window read and refuses the unbounded
// ledger interfaces, proving list_task_events never materializes a history.
type mcpWindowStore struct {
	store.Store
	t       *testing.T
	queries *[]store.TaskEventWindowQuery
	fail    *error
}

func (s mcpWindowStore) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	*s.queries = append(*s.queries, q)
	if q.Limit > store.TaskEventWindowMaxEvents || q.MaxBytes > store.TaskEventWindowMaxBytes {
		s.t.Errorf("unbounded window query %+v", q)
	}
	if *s.fail != nil {
		return store.TaskEventWindow{}, *s.fail
	}
	window, err := s.Store.ReadTaskEventWindow(ctx, q)
	if len(window.Events) > store.TaskEventWindowMaxEvents {
		s.t.Errorf("window returned %d events", len(window.Events))
	}
	return window, err
}
func (s mcpWindowStore) ListEvents(context.Context, string) ([]core.Event, error) {
	s.t.Error("list_task_events called the unbounded ledger")
	return nil, fmt.Errorf("unbounded ledger read")
}
func (s mcpWindowStore) ListEventsAfter(context.Context, string, int64) ([]core.Event, error) {
	s.t.Error("list_task_events called the unbounded ID cursor")
	return nil, fmt.Errorf("unbounded ledger read")
}

func eventIDs(t *testing.T, page mcpReadPage) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(page.Items))
	for _, raw := range page.Items {
		var item struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	return ids
}

type eventTraversal struct {
	ids     []int64
	pages   []mcpReadPage
	windows []string
}

// traverseEvents follows next_offset within a window and next_cursor across
// windows until the traversal declares completion.
func traverseEvents(t *testing.T, s *Server, args map[string]any, each func(mcpReadPage)) eventTraversal {
	t.Helper()
	var result eventTraversal
	request := maps.Clone(args)
	for range 100000 {
		page, toolErr := mcpReadCall(t, s, "reader", "list_task_events", request)
		if toolErr != "" {
			t.Fatalf("traversal page %d: %s", len(result.pages), toolErr)
		}
		if page.HistoryTotal == nil || page.Total > mcpReadMaxItems || page.Limit != args["limit"] {
			t.Fatalf("window page=%+v", page)
		}
		if len(result.windows) == 0 || result.windows[len(result.windows)-1] != page.Snapshot {
			result.windows = append(result.windows, page.Snapshot)
		}
		result.ids = append(result.ids, eventIDs(t, page)...)
		result.pages = append(result.pages, page)
		if each != nil {
			each(page)
		}
		request = maps.Clone(args)
		switch {
		case page.NextOffset != nil:
			if page.NextCursor != "" {
				t.Fatal("cursor advertised before the window was exhausted")
			}
			request["snapshot"], request["offset"] = page.Snapshot, *page.NextOffset
		case page.NextCursor != "":
			request["cursor"] = page.NextCursor
		default:
			return result
		}
	}
	t.Fatal("traversal did not terminate")
	return result
}

func newMCPWindowFixture(t *testing.T) (*Server, context.Context, *[]store.TaskEventWindowQuery, *error) {
	t.Helper()
	s, ctx := newMCPReadFixture(t)
	queries, fail := &[]store.TaskEventWindowQuery{}, new(error)
	s.Store = mcpWindowStore{Store: s.Store, t: t, queries: queries, fail: fail}
	return s, ctx, queries, fail
}

func ledgerIDs(t *testing.T, s *Server, ctx context.Context, taskID, kind string) []int64 {
	t.Helper()
	events, err := s.Store.(mcpWindowStore).Store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, event := range events {
		if kind == "" || event.Kind == kind {
			ids = append(ids, event.ID)
		}
	}
	return ids
}

func TestMCPTaskEventsTraverseLongHistoryAcrossWindows(t *testing.T) {
	s, ctx, queries, _ := newMCPWindowFixture(t)
	if err := s.Store.CreateTask(ctx, core.Task{ID: "long", Workspace: "demo", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	// 2105 renewals arrive newest-first in groups of five sharing a timestamp,
	// so ID order disagrees with time order and ties cross window boundaries.
	for i := 0; i < 2105; i++ {
		kind := "work_order.renewed"
		if i%7 == 0 {
			kind = "work_order.renewed.audit"
		}
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "long", Kind: kind, At: base.Add(time.Duration(1000-i/5) * time.Second), Payload: core.JSONPayload(map[string]any{"state": "claimed", "transcript": strings.Repeat("t", 64)})}); err != nil {
			t.Fatal(err)
		}
	}
	want := ledgerIDs(t, s, ctx, "long", "")
	if len(want) <= mcpReadMaxItems {
		t.Fatalf("fixture has only %d events", len(want))
	}
	before, _ := s.Store.GetTask(ctx, "long")
	args := map[string]any{"workspace_id": "demo", "task_id": "long", "limit": 25}
	var windowExpiry time.Time
	result := traverseEvents(t, s, args, func(page mcpReadPage) {
		if *page.HistoryTotal != len(want) {
			t.Fatalf("history_total=%d want %d", *page.HistoryTotal, len(want))
		}
		if windowExpiry.IsZero() {
			windowExpiry = page.ExpiresAt
		} else if !page.ExpiresAt.Equal(windowExpiry) {
			t.Fatalf("window extended the original expiry: %s != %s", page.ExpiresAt, windowExpiry)
		}
		s.mcpReads.mu.Lock()
		entries, cursors := len(s.mcpReads.entries), len(s.mcpReads.cursors)
		s.mcpReads.mu.Unlock()
		if entries != 1 || cursors > 2 {
			t.Fatalf("traversal retained %d windows and %d cursors", entries, cursors)
		}
	})
	if !reflect.DeepEqual(result.ids, want) {
		t.Fatalf("traversal returned %d ids, ledger has %d; first mismatch hidden by ordering", len(result.ids), len(want))
	}
	if len(result.windows) != 3 || len(result.pages) < len(want)/25 {
		t.Fatalf("windows=%d pages=%d", len(result.windows), len(result.pages))
	}
	for _, q := range *queries {
		if q.Limit != store.TaskEventWindowMaxEvents || q.MaxBytes != store.TaskEventWindowMaxBytes {
			t.Fatalf("window query exceeded bounds: %+v", q)
		}
	}
	if len(*queries) != 3 {
		t.Fatalf("backend window reads=%d want one per window", len(*queries))
	}

	// Exact kind filtering reaches the store and traverses the subset only.
	args["event_kind"] = "work_order.renewed"
	filtered := traverseEvents(t, s, args, nil)
	if !reflect.DeepEqual(filtered.ids, ledgerIDs(t, s, ctx, "long", "work_order.renewed")) || (*queries)[len(*queries)-1].Kind != "work_order.renewed" {
		t.Fatalf("filtered traversal mismatch: %d ids", len(filtered.ids))
	}
	after, _ := s.Store.GetTask(ctx, "long")
	if !reflect.DeepEqual(before, after) || len(ledgerIDs(t, s, ctx, "long", "")) != len(want) {
		t.Fatal("event reads changed task state or appended events")
	}
}

func TestMCPTaskEventsStableBoundaryRetryAndRetirement(t *testing.T) {
	s, ctx, _, fail := newMCPWindowFixture(t)
	if err := s.Store.CreateTask(ctx, core.Task{ID: "bounded", Workspace: "demo", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1500; i++ {
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "bounded", Kind: "work_order.renewed", At: base.Add(time.Duration(i/3) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	captured := ledgerIDs(t, s, ctx, "bounded", "")
	args := map[string]any{"workspace_id": "demo", "task_id": "bounded", "limit": 100}
	first := mustRead(t, s, "list_task_events", args)
	if first.Total != mcpReadMaxItems || *first.HistoryTotal != len(captured) || first.NextCursor != "" || first.NextOffset == nil {
		t.Fatalf("first page=%+v", first)
	}
	// Higher-ID appends, including a backdated one, belong to a fresh traversal.
	for _, at := range []time.Time{base.Add(-time.Hour), base.Add(time.Hour)} {
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "bounded", Kind: "work_order.renewed", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	last := mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "bounded", "limit": 100, "snapshot": first.Snapshot, "offset": 900})
	if last.NextOffset != nil || last.NextCursor == "" {
		t.Fatalf("exhausted window page=%+v", last)
	}
	cursorArgs := map[string]any{"workspace_id": "demo", "task_id": "bounded", "limit": 100, "cursor": last.NextCursor}

	// A failed window read leaves the cursor unconsumed.
	*fail = store.ErrTaskEventHistoryChanged
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", cursorArgs); e != "event history changed: restart read" {
		t.Fatalf("history change err=%q", e)
	}
	*fail = nil
	second := mustRead(t, s, "list_task_events", cursorArgs)
	retry := mustRead(t, s, "list_task_events", cursorArgs)
	if !reflect.DeepEqual(second, retry) || second.Snapshot == first.Snapshot || *second.HistoryTotal != len(captured) || second.NextCursor != "" || second.Total != len(captured)-mcpReadMaxItems {
		t.Fatalf("retry or boundary changed: equal=%v snapshot=%v history=%d cursor=%q total=%d", reflect.DeepEqual(second, retry), second.Snapshot == first.Snapshot, *second.HistoryTotal, second.NextCursor, second.Total)
	}
	if got := append(eventIDs(t, first), eventIDs(t, last)...); len(got) != 200 {
		t.Fatalf("pages=%d", len(got))
	}
	if !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("successor window extended expiry")
	}
	// The predecessor window was retired when its cursor opened the successor.
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "bounded", "limit": 100, "snapshot": first.Snapshot, "offset": 100}); !strings.Contains(e, "snapshot unavailable") {
		t.Fatalf("retired window remained readable: %q", e)
	}
	secondPage := mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "bounded", "limit": 100, "snapshot": second.Snapshot, "offset": second.Total - 1})
	if secondPage.NextCursor != "" || secondPage.NextOffset != nil {
		t.Fatalf("completed traversal advertised continuation: %+v", secondPage)
	}
	// Using the successor retires the consumed cursor.
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", cursorArgs); !strings.Contains(e, "snapshot unavailable") {
		t.Fatalf("consumed cursor survived successor use: %q", e)
	}
	fresh := mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "bounded", "limit": 1})
	if *fresh.HistoryTotal != len(captured)+2 || eventIDs(t, fresh)[0] != slices.Max(captured)+1 {
		t.Fatalf("fresh traversal missed the backdated append: %+v", fresh)
	}
}

func TestMCPTaskEventsCursorRefusalsAndAuthorization(t *testing.T) {
	s, ctx, queries, _ := newMCPWindowFixture(t)
	if err := s.Store.CreateTask(ctx, core.Task{ID: "guarded", Workspace: "demo", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1001; i++ {
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "guarded", Kind: "work_order.renewed"}); err != nil {
			t.Fatal(err)
		}
	}
	page := mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "guarded", "limit": 100, "snapshot": mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "guarded", "limit": 1}).Snapshot, "offset": 1000})
	cursor := page.NextCursor
	if cursor == "" || page.Total != 1000 {
		t.Fatalf("no cursor for history over one window: %+v", page)
	}
	reads, ledgerBefore := len(*queries), ledgerIDs(t, s, ctx, "guarded", "")
	taskBefore, _ := s.Store.GetTask(ctx, "guarded")
	base := map[string]any{"workspace_id": "demo", "task_id": "guarded", "limit": 25, "cursor": cursor}
	with := func(key string, value any) map[string]any {
		args := maps.Clone(base)
		args[key] = value
		return args
	}
	for name, tc := range map[string]struct {
		token string
		args  map[string]any
		want  string
	}{
		"tampered":         {"reader", with("cursor", strings.Repeat("0", 32)), "snapshot unavailable"},
		"malformed":        {"reader", with("cursor", "not-a-cursor"), "invalid cursor"},
		"with snapshot":    {"reader", with("snapshot", page.Snapshot), "cannot be combined"},
		"with offset":      {"reader", with("offset", 1), "cannot be combined"},
		"changed filter":   {"reader", with("event_kind", "work_order.renewed"), "snapshot unavailable"},
		"foreign task":     {"reader", with("task_id", "other"), "snapshot unavailable"},
		"foreign owner":    {"other", base, "snapshot unavailable"},
		"agent":            {"agent", base, "operator-scoped user credential"},
		"run child":        {"child", base, "operator-scoped user credential"},
		"foreign space":    {"reader", with("workspace_id", "private"), "workspace_not_found"},
		"missing capacity": {"denied", base, "workspace_not_found"},
	} {
		if _, e := mcpReadCall(t, s, tc.token, "list_task_events", tc.args); !strings.Contains(e, tc.want) {
			t.Fatalf("%s: err=%q want %q", name, e, tc.want)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(context.WithValue(t.Context(), workerContextKey{}, core.Worker{ID: "worker", Workspace: "demo"}))
	if _, e := s.callMCPTool(request, "list_task_events", base); e == nil {
		t.Fatal("worker cursor read accepted")
	}
	if len(*queries) != reads {
		t.Fatal("a refused cursor reached the store")
	}
	if taskAfter, _ := s.Store.GetTask(ctx, "guarded"); !reflect.DeepEqual(taskBefore, taskAfter) || !reflect.DeepEqual(ledgerBefore, ledgerIDs(t, s, ctx, "guarded", "")) {
		t.Fatal("a refused read changed task state or events")
	}
	// Revocation refuses the next cursor before any lookup; restoring access
	// shows the refusal did not consume it.
	membership := s.Memberships.(*membershipFixture)
	delete(membership.roles["reader"], "demo")
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", base); !strings.Contains(e, "workspace_not_found") {
		t.Fatalf("revoked cursor: %q", e)
	}
	membership.roles["reader"]["demo"] = core.WorkspaceRoleViewer
	if next := mustRead(t, s, "list_task_events", base); len(next.Items) != len(ledgerIDs(t, s, ctx, "guarded", ""))-mcpReadMaxItems || next.NextCursor != "" {
		t.Fatalf("cursor after restored access=%+v", next)
	}
	// Expiry is fixed at capture; an expired cursor requires a restart.
	fresh := mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "guarded", "limit": 100, "snapshot": mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "guarded", "limit": 1}).Snapshot, "offset": 1000})
	s.mcpReads.mu.Lock()
	expired := s.mcpReads.cursors[fresh.NextCursor]
	expired.expires = time.Now().Add(-time.Second)
	s.mcpReads.cursors[fresh.NextCursor] = expired
	s.mcpReads.mu.Unlock()
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", with("cursor", fresh.NextCursor)); !strings.Contains(e, "snapshot unavailable") {
		t.Fatalf("expired cursor: %q", e)
	}
	// A process restart or another replica has no cursor state.
	s.mcpReads.mu.Lock()
	s.mcpReads.entries, s.mcpReads.cursors = nil, nil
	s.mcpReads.mu.Unlock()
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", with("cursor", fresh.NextCursor)); !strings.Contains(e, "snapshot unavailable") {
		t.Fatalf("cursor survived restart: %q", e)
	}
}

func TestMCPTaskEventsCacheBoundsOverManyWindows(t *testing.T) {
	s, ctx, queries, _ := newMCPWindowFixture(t)
	if err := s.Store.CreateTask(ctx, core.Task{ID: "wide", Workspace: "demo", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	// Large omitted payload fields consume the source budget, so windows end
	// at the byte boundary long before the item cap.
	blob := strings.Repeat("s", 40<<10)
	for i := 0; i < 40*25; i++ {
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "wide", Kind: "work_order.renewed", Payload: core.JSONPayload(map[string]any{"transcript": blob, "state": "claimed"})}); err != nil {
			t.Fatal(err)
		}
	}
	// Leave room for exactly one traversal, then fill the rest of the cache.
	s.mcpReads.mu.Lock()
	s.mcpReads.entries = map[string]mcpReadSnapshot{}
	for i := 0; i < mcpReadSnapshotCount-1; i++ {
		s.mcpReads.entries[fmt.Sprint("occupied-", i)] = mcpReadSnapshot{expires: time.Now().Add(time.Hour)}
	}
	s.mcpReads.mu.Unlock()
	result := traverseEvents(t, s, map[string]any{"workspace_id": "demo", "task_id": "wide", "limit": 25}, func(page mcpReadPage) {
		data, _ := json.Marshal(page)
		if len(data) > mcpReadMaxBytes || strings.Contains(string(data), blob[:64]) {
			t.Fatalf("page leaked or exceeded bounds: %d bytes", len(data))
		}
	})
	if len(result.windows) <= mcpReadSnapshotCount || !reflect.DeepEqual(result.ids, ledgerIDs(t, s, ctx, "wide", "")) {
		t.Fatalf("windows=%d ids=%d", len(result.windows), len(result.ids))
	}
	for _, q := range *queries {
		if q.MaxBytes != store.TaskEventWindowMaxBytes {
			t.Fatalf("query=%+v", q)
		}
	}
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "wide"}); !strings.Contains(e, "capacity") {
		t.Fatalf("full cache allocated another traversal: %q", e)
	}
}

func TestMCPTaskEventsOversizedEventsFailExplicitly(t *testing.T) {
	s, ctx, _, _ := newMCPWindowFixture(t)
	s.WorkOrders = &workorder.Service{Store: s.Store, RedactionSecrets: mcpReadSecretFixture{}}
	if err := s.Store.CreateTask(ctx, core.Task{ID: "oversized", Workspace: "demo", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for i, source := range []string{"triage private-value-for-test", "operator", strings.Repeat("x", mcpReadMaxBytes)} {
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "oversized", Kind: "task.context_proposed", At: base.Add(time.Duration(i) * time.Second), Payload: core.JSONPayload(map[string]any{"source": source})}); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"workspace_id": "demo", "task_id": "oversized", "event_kind": "task.context_proposed", "limit": 25}
	first := mustRead(t, s, "list_task_events", args)
	if len(first.Items) != 2 || first.NextCursor == "" || *first.HistoryTotal != 3 {
		t.Fatalf("window did not end before the oversized event: %+v", first)
	}
	if text := string(first.Items[0]); strings.Contains(text, "private-value-for-test") || !strings.Contains(text, "REDACTED") {
		t.Fatalf("event text was not redacted: %s", text)
	}
	cursorArgs := maps.Clone(args)
	cursorArgs["cursor"] = first.NextCursor
	for range 2 {
		if _, e := mcpReadCall(t, s, "reader", "list_task_events", cursorArgs); !strings.Contains(e, "exceeds the 65536-byte output budget") {
			t.Fatalf("oversized event err=%q", e)
		}
	}
	// A source event beyond the store window budget is refused by the store.
	if err := s.Store.CreateTask(ctx, core.Task{ID: "source", Workspace: "demo", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "source", Kind: "transcript.self_reported", Payload: core.JSONPayload(map[string]any{"transcript": strings.Repeat("y", store.TaskEventWindowMaxBytes)})}); err != nil {
		t.Fatal(err)
	}
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "source", "event_kind": "transcript.self_reported"}); !strings.Contains(e, "event window budget") {
		t.Fatalf("source budget err=%q", e)
	}
	small := mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "source", "event_kind": "absent"})
	if small.Total != 0 || *small.HistoryTotal != 0 || small.NextCursor != "" {
		t.Fatalf("empty filtered history=%+v", small)
	}
}

func TestMCPTaskEventsSchemaAdvertisesCursorOnlyForEvents(t *testing.T) {
	for _, tool := range mcpReadTools() {
		properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		_, cursor := properties["cursor"]
		if cursor != (tool["name"] == "list_task_events") {
			t.Fatalf("%s cursor advertised=%v", tool["name"], cursor)
		}
	}
	s, _ := newMCPReadFixture(t)
	if _, e := mcpReadCall(t, s, "reader", "list_tasks", map[string]any{"workspace_id": "demo", "cursor": strings.Repeat("a", 32)}); !strings.Contains(e, "unknown read argument cursor") {
		t.Fatalf("cursor accepted by another tool: %q", e)
	}
}
