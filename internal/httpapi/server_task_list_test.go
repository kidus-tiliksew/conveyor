package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskListCountingStore records the reads GET /v1/tasks issues, so a test can
// prove they do not grow with merged and closed history (component-http-api,
// Task list read).
type taskListCountingStore struct {
	store.Store
	mu        sync.Mutex
	calls     map[string]int
	filters   []store.TaskFilter
	projected []string
}

func newTaskListCountingStore(st store.Store) *taskListCountingStore {
	return &taskListCountingStore{Store: st, calls: map[string]int{}}
}

func (s *taskListCountingStore) record(name string, ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[name]++
	s.projected = append(s.projected, ids...)
}

func (s *taskListCountingStore) ListTasks(ctx context.Context) ([]core.Task, error) {
	s.record("ListTasks")
	return s.Store.ListTasks(ctx)
}

func (s *taskListCountingStore) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	s.record("ListTasksFiltered")
	s.mu.Lock()
	s.filters = append(s.filters, filter)
	s.mu.Unlock()
	return s.Store.ListTasksFiltered(ctx, filter)
}

func (s *taskListCountingStore) ListTaskPage(ctx context.Context, query store.TaskOperationsQuery) (store.TaskPage, error) {
	s.record("ListTaskPage")
	return s.Store.ListTaskPage(ctx, query)
}

func (s *taskListCountingStore) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	s.record("ListWorkOrders")
	return s.Store.ListWorkOrders(ctx)
}

func (s *taskListCountingStore) ListLiveWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	s.record("ListLiveWorkOrders")
	return s.Store.ListLiveWorkOrders(ctx)
}

func (s *taskListCountingStore) ListWorkOrdersForTasks(ctx context.Context, ids []string) ([]core.WorkOrder, error) {
	s.record("ListWorkOrdersForTasks", ids...)
	return s.Store.ListWorkOrdersForTasks(ctx, ids)
}

func (s *taskListCountingStore) ListTaskWorkOrders(ctx context.Context, id string) ([]core.WorkOrder, error) {
	s.record("ListTaskWorkOrders", id)
	return s.Store.ListTaskWorkOrders(ctx, id)
}

func (s *taskListCountingStore) ListActivityMarkersForTasks(ctx context.Context, ids []string) ([]store.ActivityMarker, error) {
	s.record("ListActivityMarkersForTasks", ids...)
	return s.Store.ListActivityMarkersForTasks(ctx, ids)
}

func (s *taskListCountingStore) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	s.record("ListPendingProposals")
	return s.Store.ListPendingProposals(ctx)
}

func (s *taskListCountingStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	s.record("GetTask", id)
	return s.Store.GetTask(ctx, id)
}

func (s *taskListCountingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls, s.filters, s.projected = map[string]int{}, nil, nil
}

func (s *taskListCountingStore) snapshot() (map[string]int, []store.TaskFilter, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := make(map[string]int, len(s.calls))
	for name, count := range s.calls {
		calls[name] = count
	}
	return calls, append([]store.TaskFilter(nil), s.filters...), append([]string(nil), s.projected...)
}

var taskListOpenStates = []core.TaskState{core.TaskClaiming, core.TaskQueued, core.TaskRunning, core.TaskAwaiting, core.TaskApproved, core.TaskParked}

// seedTaskListWorkspace writes three open tasks and terminal merged and
// closed tasks created after them, so the newest page holds only history.
func seedTaskListWorkspace(t *testing.T, terminal int) (*taskListCountingStore, http.Handler, []string) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	open := []core.Task{
		{ID: "open-queued", Title: "Queued ledger work", Repo: "conveyor", State: core.TaskQueued, NextStage: core.StageImplement},
		{ID: "open-running", Title: "Running ledger work", Repo: "web", State: core.TaskRunning, NextStage: core.StageImplement},
		{ID: "open-awaiting", Title: "Awaiting plan", Repo: "conveyor", State: core.TaskAwaiting, NextStage: core.StageImplement, GateKind: core.GateKindPlanApproval},
	}
	var openIDs []string
	for index, task := range open {
		task.Workspace, task.BaseBranch, task.Branch, task.Source = "demo", "main", "conveyor/task-"+task.ID, "operator"
		task.CreatedAt = base.Add(time.Duration(index) * time.Minute)
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		openIDs = append(openIDs, task.ID)
	}
	for index := range terminal {
		state := core.TaskMerged
		if index%2 == 1 {
			state = core.TaskClosed
		}
		id := fmt.Sprintf("history-%04d", index)
		if err := st.CreateTask(ctx, core.Task{
			ID: id, Workspace: "demo", Title: "History " + id, Repo: "conveyor", Source: "operator", BaseBranch: "main",
			Branch: "conveyor/task-" + id, State: state, NextStage: core.StageReview,
			CreatedAt: base.Add(time.Hour + time.Duration(index)*time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	counter := newTaskListCountingStore(st)
	server := NewServer(counter)
	server.Workspace = "demo"
	return counter, authenticatedMemoryHandler(server), openIDs
}

type taskListRow struct {
	core.Task
	NextAction *json.RawMessage `json:"next_action"`
}

func getTaskList(t *testing.T, handler http.Handler, target string) (*httptest.ResponseRecorder, []taskListRow) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusOK {
		return response, nil
	}
	var rows []taskListRow
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode %s: %v", target, err)
	}
	return response, rows
}

func taskListIDs(rows []taskListRow) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}

func requireTaskListHeaders(t *testing.T, response *httptest.ResponseRecorder, total, limit, offset int) {
	t.Helper()
	for name, want := range map[string]int{"X-Conveyor-Total": total, "X-Conveyor-Limit": limit, "X-Conveyor-Offset": offset} {
		if got := response.Header().Get(name); got != strconv.Itoa(want) {
			t.Fatalf("%s=%q, want %d", name, got, want)
		}
	}
}

func TestTaskListDefaultPageIsBounded(t *testing.T) {
	small, smallHandler, _ := seedTaskListWorkspace(t, 25)
	large, largeHandler, openIDs := seedTaskListWorkspace(t, 250)
	readCalls := func(counter *taskListCountingStore, handler http.Handler, total int) (map[string]int, []store.TaskFilter, []string, []taskListRow) {
		t.Helper()
		counter.reset()
		response, rows := getTaskList(t, handler, "/v1/tasks")
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		requireTaskListHeaders(t, response, total, defaultTaskListLimit, 0)
		calls, filters, projected := counter.snapshot()
		return calls, filters, projected, rows
	}
	smallCalls, _, _, smallRows := readCalls(small, smallHandler, 28)
	if len(smallRows) != 28 {
		t.Fatalf("small fixture rows=%d, want all 28", len(smallRows))
	}
	largeCalls, filters, projected, rows := readCalls(large, largeHandler, 253)
	if len(rows) != defaultTaskListLimit {
		t.Fatalf("unparameterized rows=%d, want the %d-row default", len(rows), defaultTaskListLimit)
	}
	for i, row := range rows {
		if want := fmt.Sprintf("history-%04d", 249-i); row.ID != want {
			t.Fatalf("row %d=%s, want %s (newest first)", i, row.ID, want)
		}
		if row.NextAction == nil {
			t.Fatalf("row %s carries no next_action", row.ID)
		}
	}
	if !reflect.DeepEqual(smallCalls, largeCalls) {
		t.Fatalf("store calls grew with history: 25 terminal=%v, 250 terminal=%v", smallCalls, largeCalls)
	}
	if largeCalls["ListTasks"] != 0 || largeCalls["ListTaskPage"] != 1 || largeCalls["ListWorkOrders"] != 0 {
		t.Fatalf("task list reads=%v, want one ListTaskPage and no whole-workspace read", largeCalls)
	}
	// The projection's dependency view reads open tasks by state, never the
	// merged and closed history (component-work-orders, Next action).
	for _, filter := range filters {
		if !reflect.DeepEqual(filter.States, taskListOpenStates) {
			t.Fatalf("ListTasksFiltered filter=%+v, want only the open-state read", filter)
		}
	}
	page := map[string]bool{}
	for _, row := range rows {
		page[row.ID] = true
	}
	for _, id := range projected {
		if !page[id] && !slices.Contains(openIDs, id) {
			t.Fatalf("projection read task %s outside the page and the open tasks", id)
		}
	}
}

func TestTaskListFiltersAndPages(t *testing.T) {
	counter, handler, _ := seedTaskListWorkspace(t, 250)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	everything, err := counter.Store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantAll := make([]string, len(everything))
	for i, task := range everything {
		wantAll[i] = task.ID
	}

	// Pages concatenate to the previous unpaged response, in its order.
	var walked []string
	for offset := 0; ; offset += 200 {
		response, rows := getTaskList(t, handler, "/v1/tasks?limit=200&offset="+strconv.Itoa(offset))
		if response.Code != http.StatusOK {
			t.Fatalf("page %d status=%d body=%s", offset, response.Code, response.Body.String())
		}
		requireTaskListHeaders(t, response, 253, 200, offset)
		walked = append(walked, taskListIDs(rows)...)
		if len(rows) < 200 {
			break
		}
	}
	if !reflect.DeepEqual(walked, wantAll) {
		t.Fatalf("walked pages=%v, want the unpaged order %v", walked, wantAll)
	}

	openQuery := "/v1/tasks?state=claiming&state=queued&state=running&state=awaiting_human&state=approved&state=parked&limit=200"
	response, open := getTaskList(t, handler, openQuery)
	if response.Code != http.StatusOK {
		t.Fatalf("open status=%d body=%s", response.Code, response.Body.String())
	}
	requireTaskListHeaders(t, response, 3, 200, 0)
	gotOpen := taskListIDs(open)
	// Newest first: open-awaiting, open-running, open-queued.
	if !reflect.DeepEqual(gotOpen, []string{"open-awaiting", "open-running", "open-queued"}) {
		t.Fatalf("open rows=%v", gotOpen)
	}
	// A filtered response equals the previous unpaged response narrowed to the
	// same rows: the same task fields and the same compact next action.
	_, full := getTaskList(t, handler, "/v1/tasks?limit=200&offset=200")
	byID := map[string]taskListRow{}
	for _, row := range full {
		byID[row.ID] = row
	}
	for _, row := range open {
		previous, ok := byID[row.ID]
		if !ok {
			t.Fatalf("open row %s missing from the unfiltered pages", row.ID)
		}
		got, _ := json.Marshal(row)
		want, _ := json.Marshal(previous)
		if string(got) != string(want) {
			t.Fatalf("filtered row %s=%s, want %s", row.ID, got, want)
		}
	}

	for _, applied := range []struct {
		query string
		want  []string
	}{
		{"repository=web", []string{"open-running"}},
		{"q=ledger&state=queued&state=running", []string{"open-running", "open-queued"}},
		{"state=merged&limit=2", []string{"history-0248", "history-0246"}},
		{"state=closed&limit=2&offset=1", []string{"history-0247", "history-0245"}},
	} {
		response, rows := getTaskList(t, handler, "/v1/tasks?"+applied.query)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", applied.query, response.Code, response.Body.String())
		}
		if got := taskListIDs(rows); !reflect.DeepEqual(got, applied.want) {
			t.Fatalf("%s rows=%v, want %v", applied.query, got, applied.want)
		}
	}
	response, _ = getTaskList(t, handler, "/v1/tasks?state=merged")
	requireTaskListHeaders(t, response, 125, defaultTaskListLimit, 0)

	for _, refused := range []string{"limit=0", "limit=201", "limit=1&limit=2", "offset=3", "limit=1&offset=-1", "state=unknown", "updated_from=yesterday"} {
		response, _ := getTaskList(t, handler, "/v1/tasks?"+refused)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400", refused, response.Code)
		}
	}
}
