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
	"sync"
	"sync/atomic"
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
		"with offset 0":    {"reader", with("offset", 0), "cannot be combined"},
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

// TestMCPTaskEventsFullCacheAdvancesExistingTraversals fills every process
// snapshot slot with active traversals from four credentials at their quota,
// whose windows are smaller than one page, then advances each repeatedly.
// Replacement frees the predecessor's consumed cursor before admission, so a
// full cache never refuses an existing traversal; a fifth credential is refused.
func TestMCPTaskEventsFullCacheAdvancesExistingTraversals(t *testing.T) {
	s, ctx, _, _ := newMCPCapacityFixture(t)
	s.Store = mcpWindowStore{Store: s.Store, t: t, queries: &[]store.TaskEventWindowQuery{}, fail: new(error)}
	h := s.Handler()
	if err := s.Store.CreateTask(ctx, core.Task{ID: "saturated", Workspace: "demo", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	// Omitted 64 KiB fields end each window at the source budget after about
	// 16 events, well below one 25-item page.
	for i := 0; i < 100; i++ {
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "saturated", Kind: "work_order.renewed", Payload: core.JSONPayload(map[string]any{"transcript": strings.Repeat("x", 64<<10)})}); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"workspace_id": "demo", "task_id": "saturated", "limit": 25}
	type traversal struct {
		credential      string
		expires         time.Time
		cursor, retired string
		ids             []int64
	}
	credentials := []string{"c1", "c2", "c3", "c4"}
	traversals := make([]traversal, mcpReadSnapshotCount)
	for i := range traversals {
		credential := credentials[i/mcpReadCredentialSnapshots]
		first := readAs(t, h, credential, "list_task_events", args)
		if first.NextCursor == "" || first.NextOffset != nil || len(first.Items) >= 25 {
			t.Fatalf("first window is not smaller than one page: %+v", first)
		}
		traversals[i] = traversal{credential: credential, expires: first.ExpiresAt, cursor: first.NextCursor, ids: eventIDs(t, first)}
	}
	cacheSize := func() (int, int) {
		s.mcpReads.mu.Lock()
		defer s.mcpReads.mu.Unlock()
		return len(s.mcpReads.entries), len(s.mcpReads.cursors)
	}
	if e := refusalAs(t, h, "c5", "list_task_events", args); !strings.Contains(e, "capacity") {
		t.Fatalf("full cache admitted another traversal: %q", e)
	}
	for round := 0; ; round++ {
		active := 0
		for i := range traversals {
			tr := &traversals[i]
			if tr.cursor == "" {
				continue
			}
			active++
			request := maps.Clone(args)
			request["cursor"] = tr.cursor
			page := readAs(t, h, tr.credential, "list_task_events", request)
			if retry := readAs(t, h, tr.credential, "list_task_events", request); !reflect.DeepEqual(page, retry) {
				t.Fatalf("round %d traversal %d: retry returned a different window", round, i)
			}
			if !page.ExpiresAt.Equal(tr.expires) {
				t.Fatalf("round %d traversal %d extended expiry: %s != %s", round, i, page.ExpiresAt, tr.expires)
			}
			if page.NextOffset != nil {
				t.Fatalf("round %d traversal %d window exceeded one page", round, i)
			}
			if tr.retired != "" {
				retired := maps.Clone(args)
				retired["cursor"] = tr.retired
				if e := refusalAs(t, h, tr.credential, "list_task_events", retired); !strings.Contains(e, "snapshot unavailable") {
					t.Fatalf("round %d traversal %d: retired cursor still readable: %q", round, i, e)
				}
			}
			tr.ids = append(tr.ids, eventIDs(t, page)...)
			tr.retired, tr.cursor = tr.cursor, page.NextCursor
			if entries, cursors := cacheSize(); entries > mcpReadSnapshotCount || cursors > mcpReadCursorCount {
				t.Fatalf("round %d cache holds %d snapshots and %d cursors", round, entries, cursors)
			}
		}
		if active == 0 {
			if round < 3 {
				t.Fatalf("traversals completed after %d rounds", round)
			}
			break
		}
	}
	want := ledgerIDs(t, s, ctx, "saturated", "")
	for i, tr := range traversals {
		if !reflect.DeepEqual(tr.ids, want) {
			t.Fatalf("traversal %d returned %d ids, want %d", i, len(tr.ids), len(want))
		}
	}
	if entries, _ := cacheSize(); entries != mcpReadSnapshotCount {
		t.Fatalf("completed traversals hold %d snapshots, want one each", entries)
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

// mcpCursorStore is a goroutine-safe window store: it counts window reads and
// can fail them, so concurrent cursor tests stay race-free.
type mcpCursorStore struct {
	store.Store
	mu    *sync.Mutex
	reads *int
	fail  *error
}

func (s mcpCursorStore) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	s.mu.Lock()
	*s.reads++
	fail := *s.fail
	s.mu.Unlock()
	if fail != nil {
		return store.TaskEventWindow{}, fail
	}
	return s.Store.ReadTaskEventWindow(ctx, q)
}

type mcpCursorFixture struct {
	s     *Server
	h     http.Handler
	clock *mcpReadTestClock
	mu    *sync.Mutex
	reads *int
	fail  *error
}

func (f mcpCursorFixture) windowReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.reads
}
func (f mcpCursorFixture) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.fail = err
}

var mcpCursorArgs = map[string]any{"workspace_id": "demo", "task_id": "saturated", "limit": 25}

// newMCPCursorFixture holds a history whose omitted 64 KiB fields end every
// window after about 16 events, so each first read issues a next cursor.
func newMCPCursorFixture(t *testing.T) mcpCursorFixture {
	t.Helper()
	s, ctx, clock, _ := newMCPCapacityFixture(t)
	if err := s.Store.CreateTask(ctx, core.Task{ID: "saturated", Workspace: "demo", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: "saturated", Kind: "work_order.renewed", Payload: core.JSONPayload(map[string]any{"transcript": strings.Repeat("x", 64<<10)})}); err != nil {
			t.Fatal(err)
		}
	}
	f := mcpCursorFixture{s: s, clock: clock, mu: &sync.Mutex{}, reads: new(int), fail: new(error)}
	s.Store = mcpCursorStore{Store: s.Store, mu: f.mu, reads: f.reads, fail: f.fail}
	f.h = s.Handler()
	return f
}

// cursorTraversal tracks one traversal's current window, next cursor and the
// consumed cursor retained for retry.
type cursorTraversal struct {
	credential, window, next, retry string
	expires                         time.Time
	last                            mcpReadPage
}

func startTraversal(t *testing.T, f mcpCursorFixture, credential string) *cursorTraversal {
	t.Helper()
	page := readAs(t, f.h, credential, "list_task_events", mcpCursorArgs)
	if page.NextCursor == "" || page.NextOffset != nil {
		t.Fatalf("window did not end before one page: %+v", page)
	}
	return &cursorTraversal{credential: credential, window: page.Snapshot, next: page.NextCursor, expires: page.ExpiresAt, last: page}
}

func (tr *cursorTraversal) advance(t *testing.T, f mcpCursorFixture) {
	t.Helper()
	page := readAs(t, f.h, tr.credential, "list_task_events", withArgs(mcpCursorArgs, "cursor", tr.next))
	if !page.ExpiresAt.Equal(tr.expires) {
		t.Fatalf("advance extended expiry: %s != %s", page.ExpiresAt, tr.expires)
	}
	tr.retry, tr.next, tr.window, tr.last = tr.next, page.NextCursor, page.Snapshot, page
}

// assertTraversalGone proves every token of an evicted traversal is refused.
func assertTraversalGone(t *testing.T, f mcpCursorFixture, tr *cursorTraversal) {
	t.Helper()
	for _, args := range []map[string]any{withArgs(mcpCursorArgs, "snapshot", tr.window), withArgs(mcpCursorArgs, "cursor", tr.next), withArgs(mcpCursorArgs, "cursor", tr.retry)} {
		if args["cursor"] == "" {
			continue
		}
		if e := refusalAs(t, f.h, tr.credential, "list_task_events", args); e != "snapshot unavailable: restart read" {
			t.Fatalf("evicted traversal token: %q", e)
		}
	}
	entries, cursors := mcpCacheView(f.s)
	if _, ok := entries[tr.window]; ok {
		t.Fatal("evicted window retained")
	}
	for _, token := range []string{tr.next, tr.retry} {
		if _, ok := cursors[token]; ok && token != "" {
			t.Fatal("evicted traversal kept a cursor")
		}
	}
}

func assertTraversalRetained(t *testing.T, f mcpCursorFixture, tr *cursorTraversal) {
	t.Helper()
	entries, cursors := mcpCacheView(f.s)
	if _, ok := entries[tr.window]; !ok {
		t.Fatal("traversal window was evicted")
	}
	if _, ok := cursors[tr.next]; !ok {
		t.Fatal("traversal next cursor was evicted")
	}
}

// TestMCPTaskEventsCredentialCursorLimits proves next/retry cursor accounting,
// the 16-cursor caller quota with LRU traversal eviction of every linked
// cursor, isolation of other and same-user credentials, and the 64-cursor
// process bound across four credentials.
func TestMCPTaskEventsCredentialCursorLimits(t *testing.T) {
	f := newMCPCursorFixture(t)
	occupancy := func(credentialID string) mcpCacheOccupancy { return mcpCacheOwners(f.s)[credentialID] }
	first := startTraversal(t, f, "reader")
	if o := occupancy("reader-pat"); o != (mcpCacheOccupancy{1, 1}) {
		t.Fatalf("first window=%+v", o)
	}
	first.advance(t, f)
	if o := occupancy("reader-pat"); o != (mcpCacheOccupancy{1, 2}) {
		t.Fatalf("advance keeps next plus retry: %+v", o)
	}
	readAs(t, f.h, "reader", "list_task_events", withArgs(mcpCursorArgs, "snapshot", first.window))
	if o := occupancy("reader-pat"); o != (mcpCacheOccupancy{1, 1}) {
		t.Fatalf("successor use retires the retry cursor: %+v", o)
	}
	first.advance(t, f)
	traversals := []*cursorTraversal{first}
	for range mcpReadCredentialSnapshots - 1 {
		tr := startTraversal(t, f, "reader")
		tr.advance(t, f)
		traversals = append(traversals, tr)
	}
	if o := occupancy("reader-pat"); o != (mcpCacheOccupancy{mcpReadCredentialSnapshots, mcpReadCredentialCursors}) {
		t.Fatalf("quota fixture=%+v", o)
	}
	// A ninth traversal evicts the least-recently-used traversal with its
	// window, retry cursor and next cursor.
	ninth := startTraversal(t, f, "reader")
	if o := occupancy("reader-pat"); o != (mcpCacheOccupancy{mcpReadCredentialSnapshots, mcpReadCredentialCursors - 1}) {
		t.Fatalf("after eviction=%+v", o)
	}
	assertTraversalGone(t, f, first)
	for _, tr := range traversals[1:] {
		assertTraversalRetained(t, f, tr)
	}
	ninth.advance(t, f)
	if o := occupancy("reader-pat"); o != (mcpCacheOccupancy{mcpReadCredentialSnapshots, mcpReadCredentialCursors}) {
		t.Fatalf("after ninth advance=%+v", o)
	}
	// Another credential of the same user cannot use or consume these tokens.
	victim := traversals[1]
	for _, args := range []map[string]any{withArgs(mcpCursorArgs, "cursor", victim.next), withArgs(mcpCursorArgs, "cursor", victim.retry), withArgs(mcpCursorArgs, "snapshot", victim.window)} {
		if e := refusalAs(t, f.h, "reader-2", "list_task_events", args); e != "snapshot unavailable: restart read" {
			t.Fatalf("same-user credential exchanged a token: %q", e)
		}
	}
	victim.advance(t, f)
	// Other credentials fill their own quotas without evicting reader.
	before := mcpCacheOwners(f.s)["reader-pat"]
	for _, credential := range []string{"reader-2", "other", "c1"} {
		for range mcpReadCredentialSnapshots {
			startTraversal(t, f, credential).advance(t, f)
		}
	}
	owners := mcpCacheOwners(f.s)
	if owners["reader-pat"] != before {
		t.Fatalf("other credentials evicted reader: %+v -> %+v", before, owners["reader-pat"])
	}
	entries, cursors := mcpCacheView(f.s)
	if len(entries) != mcpReadSnapshotCount || len(cursors) != mcpReadCursorCount {
		t.Fatalf("process holds %d snapshots and %d cursors", len(entries), len(cursors))
	}
	for owner, o := range owners {
		if o.cursors > mcpReadCredentialCursors || o.snapshots > mcpReadCredentialSnapshots {
			t.Fatalf("%s exceeded its quota: %+v", owner, o)
		}
	}
	if e := refusalAs(t, f.h, "c2", "list_task_events", mcpCursorArgs); !strings.HasPrefix(e, "snapshot capacity reached: process limit 32 retained snapshots; retry at ") {
		t.Fatalf("process bound=%q", e)
	}
	assertCacheUnchanged(t, f.s, "process refusal", entries, cursors)
}

// TestMCPTaskEventsConcurrentCursorRetryAndEviction covers concurrent use of
// one cursor, advancing at full occupancy, protection of an in-flight
// traversal, failure preservation and retry-driven LRU order.
func TestMCPTaskEventsConcurrentCursorRetryAndEviction(t *testing.T) {
	t.Run("duplicate advance yields one successor", func(t *testing.T) {
		f := newMCPCursorFixture(t)
		tr := startTraversal(t, f, "reader")
		const callers = 6
		reads := f.windowReads()
		// Every caller finishes its store read and rendering before any admits.
		var arrived atomic.Int64
		gate := make(chan struct{})
		f.s.mcpReads.beforeAdmit = func() {
			if arrived.Add(1) == callers {
				close(gate)
			}
			select {
			case <-gate:
			case <-time.After(30 * time.Second):
				t.Error("barrier timed out")
			}
		}
		pages := make([]mcpReadPage, callers)
		var done sync.WaitGroup
		for i := range callers {
			done.Add(1)
			go func() {
				defer done.Done()
				page, _, toolErr, err := mcpReadTry(f.h, "reader", "list_task_events", withArgs(mcpCursorArgs, "cursor", tr.next))
				if err != nil || toolErr != "" {
					t.Errorf("caller %d: %v %s", i, err, toolErr)
				}
				pages[i] = page
			}()
		}
		done.Wait()
		f.s.mcpReads.beforeAdmit = nil
		for i := range pages {
			if !reflect.DeepEqual(pages[0], pages[i]) || pages[i].Snapshot == "" {
				t.Fatalf("caller %d received a different successor", i)
			}
		}
		if f.windowReads()-reads != callers {
			t.Fatalf("callers were not concurrent: %d reads", f.windowReads()-reads)
		}
		if o := mcpCacheOwners(f.s)["reader-pat"]; o != (mcpCacheOccupancy{1, 2}) {
			t.Fatalf("duplicate advance occupancy=%+v", o)
		}
	})
	t.Run("advance at full occupancy replaces predecessor", func(t *testing.T) {
		f := newMCPCursorFixture(t)
		var traversals []*cursorTraversal
		for range mcpReadCredentialSnapshots {
			tr := startTraversal(t, f, "reader")
			tr.advance(t, f)
			traversals = append(traversals, tr)
		}
		for _, tr := range traversals {
			predecessor, retry := tr.window, tr.retry
			tr.advance(t, f)
			entries, cursors := mcpCacheView(f.s)
			if _, ok := entries[predecessor]; ok {
				t.Fatal("predecessor window survived replacement")
			}
			if _, ok := cursors[retry]; ok {
				t.Fatal("predecessor's consumed cursor survived replacement")
			}
			for _, other := range traversals {
				assertTraversalRetained(t, f, other)
			}
			if o := mcpCacheOwners(f.s)["reader-pat"]; o != (mcpCacheOccupancy{mcpReadCredentialSnapshots, mcpReadCredentialCursors}) {
				t.Fatalf("occupancy=%+v", o)
			}
		}
	})
	t.Run("in-flight traversal is protected", func(t *testing.T) {
		f := newMCPCursorFixture(t)
		var traversals []*cursorTraversal
		for range mcpReadCredentialSnapshots {
			tr := startTraversal(t, f, "reader")
			tr.advance(t, f)
			traversals = append(traversals, tr)
		}
		lru := traversals[0]
		var paused atomic.Bool
		parked, release := make(chan struct{}), make(chan struct{})
		f.s.mcpReads.beforeAdmit = func() {
			if paused.CompareAndSwap(false, true) {
				close(parked)
				<-release
			}
		}
		var advanced mcpReadPage
		var advanceErr string
		done := make(chan struct{})
		go func() {
			defer close(done)
			page, _, toolErr, err := mcpReadTry(f.h, "reader", "list_task_events", withArgs(mcpCursorArgs, "cursor", lru.next))
			if err != nil {
				toolErr = err.Error()
			}
			advanced, advanceErr = page, toolErr
		}()
		<-parked
		ninth := startTraversal(t, f, "reader")
		assertTraversalRetained(t, f, lru)
		assertTraversalGone(t, f, traversals[1])
		close(release)
		<-done
		f.s.mcpReads.beforeAdmit = nil
		if advanceErr != "" || advanced.Snapshot == "" {
			t.Fatalf("protected advance failed: %q", advanceErr)
		}
		entries, _ := mcpCacheView(f.s)
		if _, ok := entries[advanced.Snapshot]; !ok {
			t.Fatal("protected advance was not admitted")
		}
		assertTraversalRetained(t, f, ninth)
		if o := mcpCacheOwners(f.s)["reader-pat"]; o.snapshots != mcpReadCredentialSnapshots || o.cursors > mcpReadCredentialCursors {
			t.Fatalf("occupancy=%+v", o)
		}
		f.s.mcpReads.mu.Lock()
		protected := len(f.s.mcpReads.advancing)
		f.s.mcpReads.mu.Unlock()
		if protected != 0 {
			t.Fatalf("%d traversals stayed protected", protected)
		}
	})
	t.Run("failures preserve predecessor and cursor", func(t *testing.T) {
		f := newMCPCursorFixture(t)
		tr := startTraversal(t, f, "reader")
		tr.advance(t, f)
		// Retire the retry cursor so the next advance adds one cursor.
		readAs(t, f.h, "reader", "list_task_events", withArgs(mcpCursorArgs, "snapshot", tr.window))
		entries, cursors := mcpCacheView(f.s)
		next := withArgs(mcpCursorArgs, "cursor", tr.next)
		f.setFail(fmt.Errorf("window store unavailable"))
		if e := refusalAs(t, f.h, "reader", "list_task_events", next); e != "window store unavailable" {
			t.Fatalf("store failure=%q", e)
		}
		f.setFail(nil)
		assertCacheUnchanged(t, f.s, "store failure", entries, cursors)
		original := f.s.WorkOrders
		f.s.WorkOrders = &workorder.Service{Store: f.s.Store, RedactionSecrets: mcpReadSecretFixture{fail: true}}
		if e := refusalAs(t, f.h, "reader", "list_task_events", next); e != "read redaction unavailable" {
			t.Fatalf("render failure=%q", e)
		}
		f.s.WorkOrders = original
		assertCacheUnchanged(t, f.s, "render failure", entries, cursors)
		// Fill the process cursor ceiling with other credentials' cursors.
		f.s.mcpReads.mu.Lock()
		for i := len(f.s.mcpReads.cursors); i < mcpReadCursorCount; i++ {
			f.s.mcpReads.cursors[fmt.Sprintf("%032x", i)] = mcpEventCursor{owner: fmt.Sprintf("synthetic-%d", i%4), expires: mcpReadTestBase.Add(time.Minute + time.Duration(i)*time.Second)}
		}
		f.s.mcpReads.mu.Unlock()
		entries, cursors = mcpCacheView(f.s)
		if e := refusalAs(t, f.h, "reader", "list_task_events", next); e != "event cursor capacity reached: process limit 64 retained cursors; retry at 2026-10-08T12:01:01Z after expiry (within 5 minutes)" {
			t.Fatalf("cap failure=%q", e)
		}
		assertCacheUnchanged(t, f.s, "cursor ceiling", entries, cursors)
		f.clock.Set(mcpReadTestBase.Add(time.Minute + time.Second))
		predecessor := tr.window
		tr.advance(t, f)
		if _, ok := mcpCacheViewEntry(f.s, predecessor); ok {
			t.Fatal("predecessor survived the recovered advance")
		}
	})
	t.Run("retry refreshes LRU without extending expiry", func(t *testing.T) {
		f := newMCPCursorFixture(t)
		var traversals []*cursorTraversal
		for range mcpReadCredentialSnapshots {
			tr := startTraversal(t, f, "reader")
			tr.advance(t, f)
			traversals = append(traversals, tr)
			f.clock.Advance(time.Second)
		}
		lru := traversals[0]
		retried := readAs(t, f.h, "reader", "list_task_events", withArgs(mcpCursorArgs, "cursor", lru.retry))
		if !reflect.DeepEqual(retried, lru.last) || !retried.ExpiresAt.Equal(lru.expires) {
			t.Fatalf("retry changed the window or its expiry: %s", retried.ExpiresAt)
		}
		startTraversal(t, f, "reader")
		assertTraversalRetained(t, f, lru)
		assertTraversalGone(t, f, traversals[1])
		f.clock.Set(lru.expires)
		if e := refusalAs(t, f.h, "reader", "list_task_events", withArgs(mcpCursorArgs, "cursor", lru.retry)); e != "snapshot unavailable: restart read" {
			t.Fatalf("retry outlived the traversal expiry: %q", e)
		}
	})
}

func mcpCacheViewEntry(s *Server, token string) (mcpReadSnapshot, bool) {
	entries, _ := mcpCacheView(s)
	entry, ok := entries[token]
	return entry, ok
}

// TestMCPTaskEventsProcessCursorRefusalRecovery proves the explicit 64-cursor
// refusal with its earliest expiry, admission after that expiry, and that at
// a natural full cache existing traversals still advance while a new
// credential waits for the earliest expiry.
func TestMCPTaskEventsProcessCursorRefusalRecovery(t *testing.T) {
	t.Run("cursor ceiling", func(t *testing.T) {
		f := newMCPCursorFixture(t)
		f.s.mcpReads.mu.Lock()
		f.s.mcpReads.cursors = map[string]mcpEventCursor{}
		for i := range mcpReadCursorCount {
			f.s.mcpReads.cursors[fmt.Sprintf("%032x", i)] = mcpEventCursor{owner: fmt.Sprintf("synthetic-%d", i%4), expires: mcpReadTestBase.Add(time.Minute + time.Duration(i)*time.Second)}
		}
		f.s.mcpReads.mu.Unlock()
		entries, cursors := mcpCacheView(f.s)
		reads := f.windowReads()
		e := refusalAs(t, f.h, "c5", "list_task_events", mcpCursorArgs)
		if e != "event cursor capacity reached: process limit 64 retained cursors; retry at 2026-10-08T12:01:00Z after expiry (within 5 minutes)" {
			t.Fatalf("cursor refusal=%q", e)
		}
		for _, secret := range []string{"c5-pat", "synthetic", "saturated", "workspace_id", fmt.Sprintf("%032x", 0)} {
			if strings.Contains(e, secret) {
				t.Fatalf("refusal disclosed %q", secret)
			}
		}
		if f.windowReads() != reads+1 {
			t.Fatal("refusal did not follow rendering")
		}
		assertCacheUnchanged(t, f.s, "cursor refusal", entries, cursors)
		// Two expiries leave room for the first window and its advance.
		f.clock.Set(mcpReadTestBase.Add(time.Minute + time.Second))
		tr := startTraversal(t, f, "c5")
		tr.advance(t, f)
		if _, _, toolErr, _ := mcpReadTry(f.h, "c6", "list_task_events", mcpCursorArgs); !strings.HasPrefix(toolErr, "event cursor capacity reached: process limit 64 retained cursors; retry at 2026-10-08T12:01:02Z") {
			t.Fatalf("next earliest cursor expiry=%q", toolErr)
		}
	})
	t.Run("natural full cache", func(t *testing.T) {
		f := newMCPCursorFixture(t)
		var traversals []*cursorTraversal
		for _, credential := range []string{"c1", "c2", "c3", "c4"} {
			for range mcpReadCredentialSnapshots {
				tr := startTraversal(t, f, credential)
				tr.advance(t, f)
				traversals = append(traversals, tr)
			}
			f.clock.Advance(time.Second)
		}
		entries, cursors := mcpCacheView(f.s)
		if len(entries) != mcpReadSnapshotCount || len(cursors) != mcpReadCursorCount {
			t.Fatalf("fixture holds %d snapshots and %d cursors", len(entries), len(cursors))
		}
		if e := refusalAs(t, f.h, "c5", "list_task_events", mcpCursorArgs); e != "snapshot capacity reached: process limit 32 retained snapshots; retry at 2026-10-08T12:05:00Z after expiry (within 5 minutes)" {
			t.Fatalf("full cache refusal=%q", e)
		}
		assertCacheUnchanged(t, f.s, "full refusal", entries, cursors)
		for _, tr := range traversals {
			tr.advance(t, f)
		}
		f.clock.Set(mcpReadTestBase.Add(mcpReadSnapshotTTL))
		for _, tr := range traversals[:mcpReadCredentialSnapshots] {
			assertTraversalGone(t, f, tr)
		}
		startTraversal(t, f, "c5").advance(t, f)
		for _, tr := range traversals[mcpReadCredentialSnapshots:] {
			assertTraversalRetained(t, f, tr)
		}
	})
}
