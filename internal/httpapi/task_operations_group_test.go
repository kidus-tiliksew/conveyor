package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// groupTestStore wraps a store to count, time, fail, or intercept the reads
// the grouped task-operations path issues. Hooks run synchronously inside the
// request, so ordering is deterministic without sleeps.
type groupTestStore struct {
	store.Store
	mu          sync.Mutex
	calls       map[string]int
	candidateNS time.Duration
	detailNS    time.Duration
	detailSizes []int
	fail        map[string]error
	failActive  bool // fail only the open-set candidate read
	beforeIDs   func(ids []string)
	dropDetail  string
	captured    context.Context
}

func newGroupTestStore(base store.Store) *groupTestStore {
	return &groupTestStore{Store: base, calls: map[string]int{}, fail: map[string]error{}}
}

func (st *groupTestStore) count(name string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.calls[name]++
	return st.fail[name]
}

func (st *groupTestStore) reset() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.calls, st.candidateNS, st.detailNS, st.detailSizes = map[string]int{}, 0, 0, nil
}

func (st *groupTestStore) totalCalls() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	total := 0
	for _, n := range st.calls {
		total += n
	}
	return total
}

func (st *groupTestStore) ListTaskOperationsCandidates(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	if err := st.count("ListTaskOperationsCandidates"); err != nil && (!st.failActive || len(filter.States) > 0) {
		return nil, err
	}
	started := time.Now()
	defer func() { st.candidateNS += time.Since(started) }()
	return st.Store.ListTaskOperationsCandidates(ctx, filter)
}

func (st *groupTestStore) ListTaskOperationsForIDs(ctx context.Context, ids []string) (store.TaskOperationsPage, error) {
	if err := st.count("ListTaskOperationsForIDs"); err != nil {
		return store.TaskOperationsPage{}, err
	}
	st.detailSizes = append(st.detailSizes, len(ids))
	if st.beforeIDs != nil {
		st.beforeIDs(ids)
	}
	started := time.Now()
	page, err := st.Store.ListTaskOperationsForIDs(ctx, ids)
	st.detailNS += time.Since(started)
	if st.dropDetail != "" {
		page.Tasks = slices.DeleteFunc(page.Tasks, func(task core.Task) bool { return task.ID == st.dropDetail })
	}
	return page, err
}

func (st *groupTestStore) ListTaskOperations(ctx context.Context, query store.TaskOperationsQuery) (store.TaskOperationsPage, error) {
	st.captured = ctx
	return st.Store.ListTaskOperations(ctx, query)
}

func (st *groupTestStore) ListActivityMarkersForTasks(ctx context.Context, ids []string) ([]store.ActivityMarker, error) {
	if err := st.count("ListActivityMarkersForTasks"); err != nil {
		return nil, err
	}
	return st.Store.ListActivityMarkersForTasks(ctx, ids)
}

func (st *groupTestStore) ListDependencyBlockers(ctx context.Context, ids []string) (map[string]store.DependencyBlockers, error) {
	if err := st.count("ListDependencyBlockers"); err != nil {
		return nil, err
	}
	return st.Store.ListDependencyBlockers(ctx, ids)
}

func (st *groupTestStore) ListWorkOrdersForTasks(ctx context.Context, ids []string) ([]core.WorkOrder, error) {
	_ = st.count("ListWorkOrdersForTasks")
	return st.Store.ListWorkOrdersForTasks(ctx, ids)
}

func (st *groupTestStore) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	_ = st.count("ListPendingProposals")
	return st.Store.ListPendingProposals(ctx)
}

func (st *groupTestStore) ListTaskHeads(ctx context.Context, ids []string) (map[string]store.TaskHead, error) {
	_ = st.count("ListTaskHeads")
	return st.Store.ListTaskHeads(ctx, ids)
}

func (st *groupTestStore) ListNextActionEvents(ctx context.Context, ids []string) (map[string][]core.Event, error) {
	_ = st.count("ListNextActionEvents")
	return st.Store.ListNextActionEvents(ctx, ids)
}

func (st *groupTestStore) ListLatestSpecVersionNumbers(ctx context.Context, ids []string) (map[string]int, error) {
	_ = st.count("ListLatestSpecVersionNumbers")
	return st.Store.ListLatestSpecVersionNumbers(ctx, ids)
}

func (st *groupTestStore) ListClaimBlockingProposalsForTasks(ctx context.Context, ids []string) (map[string][]store.ClaimBlockingProposal, error) {
	_ = st.count("ListClaimBlockingProposalsForTasks")
	return st.Store.ListClaimBlockingProposalsForTasks(ctx, ids)
}

func (st *groupTestStore) ListTaskContextProposals(ctx context.Context, taskID string, state core.TaskContextProposalState) ([]core.TaskContextProposal, error) {
	_ = st.count("ListTaskContextProposals")
	return st.Store.ListTaskContextProposals(ctx, taskID, state)
}

func (st *groupTestStore) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	_ = st.count("ListEvents")
	return st.Store.ListEvents(ctx, taskID)
}

func (st *groupTestStore) GetLatestSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error) {
	_ = st.count("GetLatestSpecVersion")
	return st.Store.GetLatestSpecVersion(ctx, taskID)
}

func (st *groupTestStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	_ = st.count("GetTask")
	return st.Store.GetTask(ctx, id)
}

func (st *groupTestStore) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	_ = st.count("ListTasksFiltered")
	return st.Store.ListTasksFiltered(ctx, filter)
}

// groupFixture is a seeded memory workspace whose tasks cover every group.
type groupFixture struct {
	base    store.Store
	ctx     context.Context
	created time.Time
	byGroup map[string][]string
	repo    map[string]string
}

// seedGroupFixture creates n tasks of each group shape, alternating the
// conveyor and web repositories, with creation times that tie in pairs. A web
// dependent blocks on a conveyor task, so a repository filter hides its
// blocker (req-operator-next-action AC-1.5; AC-6.5).
func seedGroupFixture(t testing.TB, base store.Store, n int, terminal int) groupFixture {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	// Orders are created now, so no queue deadline has passed while the
	// test runs; creation times tie in pairs a few minutes apart.
	created := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Minute)
	f := groupFixture{base: base, ctx: ctx, created: created, byGroup: map[string][]string{}, repo: map[string]string{}}
	sequence := 0
	add := func(group, prefix string, task core.Task, dependencies ...string) string {
		t.Helper()
		task.ID = fmt.Sprintf("%s-%04d", prefix, sequence)
		task.Workspace, task.Title, task.Body = "demo", "Task "+task.ID, "Body of "+task.ID
		task.BaseBranch, task.Branch = "main", "conveyor/task-"+task.ID
		task.Repo = "conveyor"
		if sequence%2 == 1 {
			task.Repo = "web"
		}
		if task.NextStage == "" {
			task.NextStage = core.StageImplement
		}
		task.CreatedAt = created.Add(time.Duration(sequence/2) * time.Minute)
		sequence++
		if err := base.CreateTaskWithDependencies(ctx, task, dependencies); err != nil {
			t.Fatal(err)
		}
		f.byGroup[group] = append(f.byGroup[group], task.ID)
		f.repo[task.ID] = task.Repo
		return task.ID
	}
	queueOrder := func(id string) {
		t.Helper()
		now := time.Now().UTC()
		job := core.Job{ID: id + "-job", TaskID: id, Stage: core.StageImplement, StartedAt: now}
		if err := base.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(base).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		add("operator", "plan", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval})
		agent := add("agent", "queued", core.Task{State: core.TaskQueued})
		queueOrder(agent)
		add("waiting", "approved", core.Task{State: core.TaskApproved, MergeApproval: false})
		blocked := add("waiting", "blocked", core.Task{State: core.TaskQueued}, agent)
		queueOrder(blocked)
	}
	for i := 0; i < terminal; i++ {
		add("terminal", "merged", core.Task{State: core.TaskMerged})
	}
	return f
}

type groupServer struct {
	t      testing.TB
	server *Server
	store  *groupTestStore
}

func newGroupServer(t testing.TB, base store.Store) groupServer {
	t.Helper()
	wrapped := newGroupTestStore(base)
	server := NewServer(wrapped)
	server.Workspace = "demo"
	return groupServer{t: t, server: server, store: wrapped}
}

func (g groupServer) get(path string) *httptest.ResponseRecorder {
	g.t.Helper()
	response := httptest.NewRecorder()
	g.server.Handler().ServeHTTP(response, authenticatedMemoryRead(g.server, httptest.NewRequest(http.MethodGet, path, nil)))
	return response
}

type groupRow struct {
	Task struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Repo  string `json:"repo"`
		Body  string `json:"body"`
	} `json:"task"`
	NeedsAttention bool `json:"needs_attention"`
	NextAction     struct {
		Actor string `json:"actor"`
		Kind  string `json:"kind"`
	} `json:"next_action"`
}

func decodeGroupRows(t testing.TB, response *httptest.ResponseRecorder) []groupRow {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var rows []groupRow
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return rows
}

func groupCounts(response *httptest.ResponseRecorder) map[taskOperationsGroup]string {
	out := map[taskOperationsGroup]string{}
	for _, group := range taskOperationsGroups {
		out[group] = response.Header().Get(taskOperationsGroupHeaders[group])
	}
	return out
}

func rowIDs(rows []groupRow) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].Task.ID
	}
	return ids
}

// tally groups the ungrouped read's rows by their stated actor.
func tally(t testing.TB, rows []groupRow) map[taskOperationsGroup][]string {
	t.Helper()
	out := map[taskOperationsGroup][]string{}
	for _, row := range rows {
		group, ok := taskOperationsGroupForActor(row.NextAction.Actor)
		if !ok {
			t.Fatalf("row %s has actor %q", row.Task.ID, row.NextAction.Actor)
		}
		out[group] = append(out[group], row.Task.ID)
	}
	return out
}

func TestTaskOperationsGroupSelectorValidation(t *testing.T) {
	g := newGroupServer(t, store.NewMemory())
	seedGroupFixture(t, g.store.Store, 1, 1)
	refusal := errTaskOperationsGroup.Error() + "\n"
	for _, query := range []string{"group=", "group=Operator", "group=OPERATOR", "group=factory", "group=dependency", "group=none", "group=all", "group=operator&group=operator", "group=operator&group=agent", "group=%20operator"} {
		response := g.get("/v1/task-operations?workspace_id=demo&" + query)
		if response.Code != http.StatusBadRequest || response.Body.String() != refusal {
			t.Fatalf("%s: status=%d body=%q", query, response.Code, response.Body.String())
		}
		if response.Header().Get(taskOperationsGroupHeaders[taskOperationsGroupOperator]) != "" {
			t.Fatalf("%s: refusal carried group headers", query)
		}
	}
	for _, group := range taskOperationsGroups {
		if response := g.get("/v1/task-operations?workspace_id=demo&group=" + string(group)); response.Code != http.StatusOK {
			t.Fatalf("group %s status=%d body=%s", group, response.Code, response.Body.String())
		}
	}
	// A malformed filter keeps its own refusal even beside a malformed group.
	if response := g.get("/v1/task-operations?workspace_id=demo&state=unknown&group=bogus"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid task state") {
		t.Fatalf("filter refusal status=%d body=%q", response.Code, response.Body.String())
	}
	if response := g.get("/v1/task-operations?workspace_id=demo&group=operator&offset=1"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "offset requires limit") {
		t.Fatalf("offset refusal status=%d body=%q", response.Code, response.Body.String())
	}
	// Neighbouring reads never read the selector.
	for _, path := range []string{"/v1/activity?workspace_id=demo", "/v1/requirements/req-none/events?workspace_id=demo&limit=10", "/v1/task-map?workspace_id=demo", "/v1/attention/tasks?workspace_id=demo"} {
		plain, selected := g.get(path), g.get(path+"&group=bogus")
		if plain.Code != selected.Code || plain.Body.String() != selected.Body.String() || strings.Contains(selected.Body.String(), "group must be supplied") {
			t.Fatalf("%s changed with group: %d %q vs %d %q", path, plain.Code, plain.Body.String(), selected.Code, selected.Body.String())
		}
	}
}

func TestTaskOperationsGroupPartitionsByStatedActor(t *testing.T) {
	g := newGroupServer(t, store.NewMemory())
	f := seedGroupFixture(t, g.store.Store, 2, 3)
	all := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo"))
	tallied := tally(t, all)
	for _, group := range taskOperationsGroups {
		want := f.byGroup[string(group)]
		got := tallied[group]
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("ungrouped %s members=%v want %v", group, got, want)
		}
	}
	opts, err := g.server.nextActionOptions(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(f.ctx))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range taskOperationsGroups {
		response := g.get("/v1/task-operations?workspace_id=demo&group=" + string(group))
		rows := decodeGroupRows(t, response)
		if response.Header().Get("X-Conveyor-Total") != "" || response.Header().Get("X-Conveyor-Limit") != "" {
			t.Fatalf("unpaginated group %s carried pagination headers %v", group, response.Header())
		}
		counts := groupCounts(response)
		for _, member := range taskOperationsGroups {
			if counts[member] != strconv.Itoa(len(f.byGroup[string(member)])) {
				t.Fatalf("group %s counts=%v", group, counts)
			}
		}
		wantOrder := []string{}
		for _, row := range all {
			if g, _ := taskOperationsGroupForActor(row.NextAction.Actor); g == group {
				wantOrder = append(wantOrder, row.Task.ID)
			}
		}
		if !slices.Equal(rowIDs(rows), wantOrder) {
			t.Fatalf("group %s rows=%v want %v", group, rowIDs(rows), wantOrder)
		}
		for _, row := range rows {
			member, _ := taskOperationsGroupForActor(row.NextAction.Actor)
			if member != group || row.NeedsAttention != (row.NextAction.Actor == nextaction.ActorOperator) || row.Task.Body == "" {
				t.Fatalf("group %s row %+v", group, row)
			}
			single, err := store.ReadNextAction(f.ctx, f.base, row.Task.ID, opts)
			if err != nil {
				t.Fatal(err)
			}
			if single.Actor != row.NextAction.Actor || single.Kind != row.NextAction.Kind {
				t.Fatalf("row %s actor=%s/%s, next-action read %s/%s", row.Task.ID, row.NextAction.Actor, row.NextAction.Kind, single.Actor, single.Kind)
			}
			route := g.get("/v1/tasks/" + row.Task.ID + "/next-action?workspace_id=demo")
			var stated struct {
				Actor string `json:"actor"`
			}
			if route.Code != http.StatusOK || json.Unmarshal(route.Body.Bytes(), &stated) != nil || stated.Actor != row.NextAction.Actor {
				t.Fatalf("row %s actor %s, route status=%d body=%s", row.Task.ID, row.NextAction.Actor, route.Code, route.Body.String())
			}
		}
	}
	// Approved with the merge gate off is the factory, counted as waiting, and
	// a dependency-blocked dependent waits too (AC-1.5).
	waiting := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&group=waiting"))
	actors := map[string]bool{}
	for _, row := range waiting {
		actors[row.NextAction.Actor] = true
	}
	if !actors[nextaction.ActorFactory] || !actors[nextaction.ActorDependency] {
		t.Fatalf("waiting actors=%v", actors)
	}
}

func TestTaskOperationsGroupPagingAndCounts(t *testing.T) {
	g := newGroupServer(t, store.NewMemory())
	f := seedGroupFixture(t, g.store.Store, 3, 4)
	waiting := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&group=waiting"))
	if len(waiting) != len(f.byGroup["waiting"]) {
		t.Fatalf("waiting rows=%d", len(waiting))
	}
	for offset := range waiting {
		response := g.get(fmt.Sprintf("/v1/task-operations?workspace_id=demo&group=waiting&limit=1&offset=%d", offset))
		rows := decodeGroupRows(t, response)
		if len(rows) != 1 || rows[0].Task.ID != waiting[offset].Task.ID {
			t.Fatalf("offset %d rows=%v", offset, rowIDs(rows))
		}
		if response.Header().Get("X-Conveyor-Total") != strconv.Itoa(len(waiting)) || response.Header().Get("X-Conveyor-Limit") != "1" || response.Header().Get("X-Conveyor-Offset") != strconv.Itoa(offset) {
			t.Fatalf("offset %d headers=%v", offset, response.Header())
		}
	}
	// Each group pages on its own offset.
	operator := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&group=operator&limit=2&offset=1"))
	if want := f.byGroup["operator"]; len(operator) != 2 {
		t.Fatalf("operator page=%v of %v", rowIDs(operator), want)
	}
	past := g.get("/v1/task-operations?workspace_id=demo&group=terminal&limit=5&offset=40")
	if body := strings.TrimSpace(past.Body.String()); past.Code != http.StatusOK || body != "[]" || past.Header().Get("X-Conveyor-Total") != "4" || groupCounts(past)[taskOperationsGroupTerminal] != "4" {
		t.Fatalf("past-end status=%d body=%q headers=%v", past.Code, body, past.Header())
	}
	empty := g.get("/v1/task-operations?workspace_id=demo&group=agent&limit=5&state=merged")
	if body := strings.TrimSpace(empty.Body.String()); body != "[]" || empty.Header().Get("X-Conveyor-Total") != "0" {
		t.Fatalf("empty group body=%q headers=%v", body, empty.Header())
	}
	if counts := groupCounts(empty); counts[taskOperationsGroupTerminal] != "4" || counts[taskOperationsGroupAgent] != "0" || counts[taskOperationsGroupOperator] != "0" || counts[taskOperationsGroupWaiting] != "0" {
		t.Fatalf("empty group counts=%v", counts)
	}
	none := g.get("/v1/task-operations?workspace_id=demo&group=operator&limit=5&q=nothing-matches-this")
	if body := strings.TrimSpace(none.Body.String()); body != "[]" || none.Header().Get("X-Conveyor-Total") != "0" {
		t.Fatalf("no-candidate body=%q headers=%v", body, none.Header())
	}
	for _, group := range taskOperationsGroups {
		if groupCounts(none)[group] != "0" {
			t.Fatalf("no-candidate counts=%v", groupCounts(none))
		}
	}
	for query, want := range map[string]string{
		"limit=0":           "limit must be between 1 and 200",
		"limit=201":         "limit must be between 1 and 200",
		"limit=1&limit=2":   "limit must be supplied once",
		"limit=1&offset=-1": "offset must be between",
	} {
		response := g.get("/v1/task-operations?workspace_id=demo&group=waiting&" + query)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("%s status=%d body=%q", query, response.Code, response.Body.String())
		}
	}
}

func TestTaskOperationsGroupCombinedFilters(t *testing.T) {
	g := newGroupServer(t, store.NewMemory())
	f := seedGroupFixture(t, g.store.Store, 3, 4)
	at := func(minutes int) string {
		return f.created.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
	}
	for _, filter := range []string{
		"repository=web",
		"repository=conveyor",
		"state=queued",
		"state=queued&state=approved&repository=web",
		"q=blocked",
		"q=0001&repository=web",
		"created_from=" + at(2),
		"created_to=" + at(3) + "&repository=conveyor",
		"updated_from=2000-01-01T00:00:00Z&state=awaiting_human",
		"updated_to=" + at(4) + "&repository=web",
		"assignee=unassigned&repository=web&state=queued",
	} {
		all := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&"+filter))
		tallied := tally(t, all)
		for _, group := range taskOperationsGroups {
			response := g.get("/v1/task-operations?workspace_id=demo&limit=200&group=" + string(group) + "&" + filter)
			rows := decodeGroupRows(t, response)
			if !slices.Equal(rowIDs(rows), tallied[group]) && !(len(rows) == 0 && len(tallied[group]) == 0) {
				t.Fatalf("%s group %s rows=%v want %v", filter, group, rowIDs(rows), tallied[group])
			}
			if response.Header().Get("X-Conveyor-Total") != strconv.Itoa(len(tallied[group])) {
				t.Fatalf("%s group %s total=%s want %d", filter, group, response.Header().Get("X-Conveyor-Total"), len(tallied[group]))
			}
			sum := 0
			for _, member := range taskOperationsGroups {
				count, err := strconv.Atoi(groupCounts(response)[member])
				if err != nil || count != len(tallied[member]) {
					t.Fatalf("%s group %s counts=%v tallies=%v", filter, group, groupCounts(response), tallied)
				}
				sum += count
			}
			if sum != len(all) {
				t.Fatalf("%s counts sum=%d want %d", filter, sum, len(all))
			}
		}
	}
	// A web dependent whose blocker is a conveyor task stays blocked under a
	// web-only filter: the open set is read without the filter.
	web := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&group=waiting&repository=web"))
	blocked := 0
	for _, row := range web {
		if row.NextAction.Actor == nextaction.ActorDependency {
			blocked++
		}
	}
	if blocked == 0 {
		t.Fatalf("web waiting rows=%+v carry no dependency-blocked task", web)
	}
}

// referenceListTaskOperations is the ungrouped handler exactly as it stood
// before the group selector, kept as the oracle for the unchanged read.
func referenceListTaskOperations(s *Server, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query, paginated, err := parseTaskOperationsQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page, err := s.Store.ListTaskOperations(ctx, query)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if paginated {
		w.Header().Set("X-Conveyor-Total", strconv.Itoa(page.Total))
		w.Header().Set("X-Conveyor-Limit", strconv.Itoa(query.Limit))
		w.Header().Set("X-Conveyor-Offset", strconv.Itoa(query.Offset))
	}
	taskIDs := make([]string, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		taskIDs = append(taskIDs, task.ID)
	}
	markers, err := s.Store.ListActivityMarkersForTasks(ctx, taskIDs)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	markerByTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	opts.MarkersLoaded, opts.Markers = true, markers
	projected, err := store.ReadNextActions(ctx, s.Store, page.Tasks, opts)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// (component-task-lifecycle); the view reads it, and reads the unsatisfiable edges the
	// task record itself does not carry.
	blockers, err := s.Store.ListDependencyBlockers(ctx, taskIDs)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Attached documents repeat across rows; memoizing only the document reads
	// keeps the authoritative fold in store.TaskContextFromEvents.
	documents := &taskContextDocumentCache{Store: s.Store, requirements: map[string]core.Requirement{}, designs: map[string]core.SystemDesign{}}
	items := make([]taskOperationsItem, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		events := page.Events[task.ID]
		task.Context, err = store.TaskContextFromEvents(ctx, documents, events)
		if err != nil {
			log.Printf("handle API request: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		latest, hasPlan := page.Plans[task.ID]
		marker := markerByTask[task.ID]
		if core.TaskTerminal(task.State) {
			marker.Stalled = nil
		}
		compact := nextaction.CompactFrom(projected[task.ID])
		items = append(items, taskOperationsItem{
			Task:                 task,
			LatestStage:          marker.LatestStage,
			LastEventAt:          marker.LastEventAt,
			Stalled:              taskStalledSummaryFor(marker.Stalled),
			NeedsAttention:       projected[task.ID].Actor == nextaction.ActorOperator,
			NextAction:           &compact,
			UnsatisfiableTaskIDs: blockers[task.ID].UnsatisfiableTaskIDs,
			ChildRollup:          taskChildRollupFor(task.Children),
			Plan:                 taskPlanStatusFor(task, latest, hasPlan, events),
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func TestTaskOperationsUngroupedResponseUnchanged(t *testing.T) {
	base := store.NewMemory()
	g := newGroupServer(t, base)
	f := seedGroupFixture(t, base, 2, 2)
	if _, err := base.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: f.byGroup["operator"][0], Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})}); err != nil {
		t.Fatal(err)
	}
	if err := base.AppendEvent(f.ctx, core.Event{TaskID: f.byGroup["agent"][0], Kind: "task.state_changed", ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"command": "probe"})}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "&limit=3", "&limit=2&offset=3", "&repository=web", "&state=queued&limit=1", "&q=plan"} {
		path := "/v1/task-operations?workspace_id=demo" + query
		current := g.get(path)
		if current.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, current.Code)
		}
		reference := httptest.NewRecorder()
		referenceListTaskOperations(g.server, reference, httptest.NewRequest(http.MethodGet, path, nil).WithContext(g.store.captured))
		if !bytes.Equal(current.Body.Bytes(), reference.Body.Bytes()) {
			t.Fatalf("%s body changed:\n got %s\nwant %s", path, current.Body.String(), reference.Body.String())
		}
		for _, header := range []string{"Content-Type", "X-Conveyor-Total", "X-Conveyor-Limit", "X-Conveyor-Offset", taskOperationsGroupHeaders[taskOperationsGroupOperator]} {
			if current.Header().Get(header) != reference.Header().Get(header) {
				t.Fatalf("%s header %s=%q want %q", path, header, current.Header().Get(header), reference.Header().Get(header))
			}
		}
	}
	if g.store.calls["ListTaskOperationsCandidates"] != 0 || g.store.calls["ListTaskOperationsForIDs"] != 0 {
		t.Fatalf("ungrouped read used grouped store reads: %v", g.store.calls)
	}
}

func TestTaskOperationsGroupHydratesInChunks(t *testing.T) {
	g := newGroupServer(t, store.NewMemory())
	f := seedGroupFixture(t, g.store.Store, 1, 205)
	g.store.reset()
	rows := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&group=terminal"))
	if len(rows) != 205 || !slices.Equal(g.store.detailSizes, []int{200, 5}) {
		t.Fatalf("rows=%d chunks=%v", len(rows), g.store.detailSizes)
	}
	want := slices.Clone(f.byGroup["terminal"])
	all := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&state=merged"))
	if got := rowIDs(rows); !slices.Equal(got, rowIDs(all)) || len(want) != len(got) {
		t.Fatalf("chunked order differs from the ungrouped order")
	}
	for _, row := range rows {
		if row.Task.Body == "" {
			t.Fatalf("chunked row %s lost its detail", row.Task.ID)
		}
	}
	g.store.reset()
	decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&group=terminal&limit=200&offset=5"))
	if !slices.Equal(g.store.detailSizes, []int{200}) {
		t.Fatalf("paged chunks=%v", g.store.detailSizes)
	}
}

func TestTaskOperationsGroupKeepsCandidateObservation(t *testing.T) {
	base := store.NewMemory()
	g := newGroupServer(t, base)
	f := seedGroupFixture(t, base, 1, 1)
	target := f.byGroup["operator"][0]
	// The hook cancels the selected task after the projection and before the
	// detail read, inside the request: no timing is involved.
	g.store.beforeIDs = func(ids []string) {
		if !slices.Contains(ids, target) {
			return
		}
		g.store.beforeIDs = nil
		if _, err := taskops.New(base).Perform(f.ctx, target, taskops.Command{Kind: core.TaskCancel}); err != nil {
			t.Errorf("cancel %s: %v", target, err)
		}
	}
	response := g.get("/v1/task-operations?workspace_id=demo&group=operator&limit=25")
	rows := decodeGroupRows(t, response)
	if len(rows) != 1 || rows[0].Task.ID != target || rows[0].Task.State != string(core.TaskAwaiting) || rows[0].NextAction.Actor != nextaction.ActorOperator || !rows[0].NeedsAttention {
		t.Fatalf("row did not keep the projected observation: %+v", rows)
	}
	if groupCounts(response)[taskOperationsGroupOperator] != "1" || response.Header().Get("X-Conveyor-Total") != "1" {
		t.Fatalf("counts=%v", groupCounts(response))
	}
	// A later read observes the cancellation.
	later := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&group=terminal"))
	if !slices.Contains(rowIDs(later), target) {
		t.Fatalf("later terminal rows=%v", rowIDs(later))
	}
}

func TestTaskOperationsGroupReadFailures(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	for name, configure := range map[string]func(*groupTestStore){
		"candidates": func(st *groupTestStore) {
			st.fail["ListTaskOperationsCandidates"] = errors.New("candidate read failed")
		},
		"open set": func(st *groupTestStore) {
			st.fail["ListTaskOperationsCandidates"] = errors.New("open-set read failed")
			st.failActive = true
		},
		"markers":        func(st *groupTestStore) { st.fail["ListActivityMarkersForTasks"] = errors.New("marker read failed") },
		"detail":         func(st *groupTestStore) { st.fail["ListTaskOperationsForIDs"] = errors.New("detail read failed") },
		"blockers":       func(st *groupTestStore) { st.fail["ListDependencyBlockers"] = errors.New("blocker read failed") },
		"missing detail": func(st *groupTestStore) { st.dropDetail = "plan-0000" },
	} {
		t.Run(name, func(t *testing.T) {
			g := newGroupServer(t, store.NewMemory())
			seedGroupFixture(t, g.store.Store, 1, 1)
			configure(g.store)
			path := "/v1/task-operations?workspace_id=demo&group=operator&limit=5"
			if name == "open set" {
				path += "&repository=conveyor"
			}
			logs.Reset()
			response := g.get(path)
			if response.Code != http.StatusInternalServerError || response.Body.String() != "internal server error\n" {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			for _, group := range taskOperationsGroups {
				if response.Header().Get(taskOperationsGroupHeaders[group]) != "" {
					t.Fatalf("failed read carried %s", taskOperationsGroupHeaders[group])
				}
			}
			if !strings.Contains(logs.String(), "handle API request:") {
				t.Fatalf("log=%q", logs.String())
			}
			if name == "missing detail" && !strings.Contains(logs.String(), "task plan-0000 missing from the detail read") {
				t.Fatalf("log=%q", logs.String())
			}
		})
	}
}

func TestPartitionTaskOperationsRefusesUnknownActor(t *testing.T) {
	tasks := []core.Task{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	projected := map[string]nextaction.NextAction{
		"a": {Actor: nextaction.ActorOperator}, "b": {Actor: nextaction.ActorAgent}, "c": {Actor: nextaction.ActorFactory},
		"d": {Actor: nextaction.ActorDependency}, "e": {Actor: nextaction.ActorNone},
	}
	partition, err := partitionTaskOperations(tasks, projected, taskOperationsGroupWaiting)
	if err != nil {
		t.Fatal(err)
	}
	if partition.counts[taskOperationsGroupOperator] != 1 || partition.counts[taskOperationsGroupAgent] != 1 || partition.counts[taskOperationsGroupWaiting] != 2 || partition.counts[taskOperationsGroupTerminal] != 1 {
		t.Fatalf("counts=%v", partition.counts)
	}
	if ids := []string{partition.selected[0].ID, partition.selected[1].ID}; !slices.Equal(ids, []string{"c", "d"}) {
		t.Fatalf("selected=%v", ids)
	}
	if _, err = partitionTaskOperations(append(tasks, core.Task{ID: "f"}), projected, taskOperationsGroupOperator); err == nil || !strings.Contains(err.Error(), "task f has no projected next action") {
		t.Fatalf("missing projection err=%v", err)
	}
	projected["e"] = nextaction.NextAction{Actor: "robot"}
	if _, err = partitionTaskOperations(tasks, projected, taskOperationsGroupOperator); err == nil || !strings.Contains(err.Error(), `unexpected next-action actor "robot"`) {
		t.Fatalf("unknown actor err=%v", err)
	}
	for _, actor := range []string{"", "Operator", "robot"} {
		if _, ok := taskOperationsGroupForActor(actor); ok {
			t.Fatalf("actor %q mapped to a group", actor)
		}
	}
}

// TestTaskOperationsGroupScaleBoundedReads measures the grouped read over a
// memory fixture of about 700 mixed tasks and proves its store reads do not
// grow with task count. Timings are logged as evidence, not asserted.
func TestTaskOperationsGroupScaleBoundedReads(t *testing.T) {
	measure := func(n, terminal int) (map[string]int, int) {
		g := newGroupServer(t, store.NewMemory())
		seedGroupFixture(t, g.store.Store, n, terminal)
		total := 4*n + terminal
		var calls map[string]int
		for run := 0; run < 4; run++ {
			g.store.reset()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			started := time.Now()
			response := g.get("/v1/task-operations?workspace_id=demo&group=waiting&limit=25")
			elapsed := time.Since(started)
			runtime.ReadMemStats(&after)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d", response.Code)
			}
			g.store.mu.Lock()
			calls = map[string]int{}
			for name, count := range g.store.calls {
				calls[name] = count
			}
			candidate, detail := g.store.candidateNS, g.store.detailNS
			g.store.mu.Unlock()
			label := "repeated"
			if run == 0 {
				label = "cold"
			}
			t.Logf("memory grouped read: %d tasks (%d per open shape, %d merged), %s: total %s, candidates %s, details %s, projection and assembly %s, %d store calls, %d allocations, %d bytes allocated, %d response bytes",
				total, n, terminal, label, elapsed, candidate, detail, elapsed-candidate-detail, g.store.totalCalls(), after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc, response.Body.Len())
		}
		return calls, total
	}
	small, _ := measure(5, 20)
	large, total := measure(100, 300)
	if total != 700 {
		t.Fatalf("large fixture has %d tasks", total)
	}
	if fmt.Sprint(small) != fmt.Sprint(large) {
		t.Fatalf("store reads grew with task count:\nsmall %v\nlarge %v", small, large)
	}
	for _, perTask := range []string{"ListEvents", "GetLatestSpecVersion", "GetTask", "ListTasksFiltered"} {
		if large[perTask] != 0 {
			t.Fatalf("grouped read issued %s %d times: %v", perTask, large[perTask], large)
		}
	}
}

// TestTaskOperationsGroupContextAndAssigneeFilters seeds served requirements,
// governing designs, a detached attachment of each kind, and an assigned user
// across every actor group. It asserts each group's exact membership, all
// four pre-pagination counts, X-Conveyor-Total, and every limit/offset page
// against the whole filtered set derived from the seeded attributes, with each
// filter member alone and combined (req-operator-next-action AC-6.2, AC-6.5;
// req-task-centric-operations-view AC-2.3, AC-2.4).
func TestTaskOperationsGroupContextAndAssigneeFilters(t *testing.T) {
	base := store.NewMemory()
	g := newGroupServer(t, base)
	f := seedGroupFixture(t, base, 3, 4)
	at := f.created.Add(30 * time.Minute)
	attach := func(taskID, kind, documentID string) {
		t.Helper()
		at = at.Add(time.Second)
		if err := base.AppendEvent(f.ctx, core.Event{TaskID: taskID, Kind: kind, At: at, Payload: core.JSONPayload(map[string]any{"id": documentID, "version": 1})}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetMemoryWorkspaceMember(base, "demo", "usr-group", true); err != nil {
		t.Fatal(err)
	}
	assign := func(taskID string) {
		t.Helper()
		if _, err := taskops.New(base).SetAssignee(f.ctx, taskID, "usr-group"); err != nil {
			t.Fatalf("assign %s: %v", taskID, err)
		}
	}
	// waiting holds approved tasks at even indexes and blocked dependents at odd ones.
	operator, agent, waiting, terminal := f.byGroup["operator"], f.byGroup["agent"], f.byGroup["waiting"], f.byGroup["terminal"]
	serves := map[string]bool{}
	governs := map[string]bool{}
	assigned := map[string]bool{}
	for _, id := range []string{operator[0], operator[2], agent[0], waiting[0], waiting[1], terminal[0], terminal[3]} {
		attach(id, store.TaskContextRequirementAdded, "req-group")
		serves[id] = true
	}
	// A detached requirement and a detached design no longer match.
	for _, id := range []string{agent[1], terminal[1]} {
		attach(id, store.TaskContextRequirementAdded, "req-group")
		attach(id, store.TaskContextRequirementRemoved, "req-group")
	}
	for _, id := range []string{operator[1], operator[2], agent[2], waiting[3], terminal[0], terminal[2]} {
		attach(id, store.TaskContextDesignAdded, "design-group")
		governs[id] = true
	}
	attach(waiting[2], store.TaskContextDesignAdded, "design-group")
	attach(waiting[2], store.TaskContextDesignRemoved, "design-group")
	for _, id := range []string{operator[0], operator[1], agent[0], agent[2], waiting[1], waiting[2], waiting[3]} {
		assign(id)
		assigned[id] = true
	}
	groupOf := map[string]taskOperationsGroup{}
	for _, group := range taskOperationsGroups {
		for _, id := range f.byGroup[string(group)] {
			groupOf[id] = group
		}
	}
	stateOf := map[string]string{}
	all := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo"))
	for _, row := range all {
		stateOf[row.Task.ID] = row.Task.State
		if member, _ := taskOperationsGroupForActor(row.NextAction.Actor); member != groupOf[row.Task.ID] {
			t.Fatalf("fixture task %s stated actor %s, seeded in %s", row.Task.ID, row.NextAction.Actor, groupOf[row.Task.ID])
		}
	}
	for _, testCase := range []struct {
		name  string
		query string
		match func(id string) bool
	}{
		{"served requirement", "serves_requirement=req-group", func(id string) bool { return serves[id] }},
		{"detached or absent requirement", "serves_requirement=req-absent", func(string) bool { return false }},
		{"several requirements", "serves_requirement=req-absent&serves_requirement=req-group", func(id string) bool { return serves[id] }},
		{"governing design", "governing_design=design-group", func(id string) bool { return governs[id] }},
		{"several designs", "governing_design=design-absent&governing_design=design-group", func(id string) bool { return governs[id] }},
		{"assignee", "assignee=usr-group", func(id string) bool { return assigned[id] }},
		{"unassigned", "assignee=unassigned", func(id string) bool { return !assigned[id] }},
		{"unknown assignee", "assignee=usr-nobody", func(string) bool { return false }},
		{"requirement and design", "serves_requirement=req-group&governing_design=design-group", func(id string) bool { return serves[id] && governs[id] }},
		{"requirement and assignee", "serves_requirement=req-group&assignee=usr-group", func(id string) bool { return serves[id] && assigned[id] }},
		{"design and assignee", "governing_design=design-group&assignee=usr-group", func(id string) bool { return governs[id] && assigned[id] }},
		{"requirement and repository", "serves_requirement=req-group&repository=web", func(id string) bool { return serves[id] && f.repo[id] == "web" }},
		{"design and state", "governing_design=design-group&state=awaiting_human&state=merged", func(id string) bool {
			return governs[id] && (stateOf[id] == string(core.TaskAwaiting) || stateOf[id] == string(core.TaskMerged))
		}},
		{"assignee and free text", "assignee=usr-group&q=queued", func(id string) bool { return assigned[id] && strings.Contains(id, "queued") }},
		{"unassigned requirement in conveyor", "assignee=unassigned&serves_requirement=req-group&repository=conveyor", func(id string) bool {
			return !assigned[id] && serves[id] && f.repo[id] == "conveyor"
		}},
		{"every member", "serves_requirement=req-group&governing_design=design-group&assignee=unassigned&repository=conveyor&state=merged&q=merged&updated_from=2000-01-01T00:00:00Z&created_from=" + f.created.Format(time.RFC3339), func(id string) bool {
			return serves[id] && governs[id] && !assigned[id] && f.repo[id] == "conveyor" && stateOf[id] == string(core.TaskMerged)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			want := map[taskOperationsGroup][]string{}
			matched := 0
			for _, row := range all {
				if testCase.match(row.Task.ID) {
					want[groupOf[row.Task.ID]] = append(want[groupOf[row.Task.ID]], row.Task.ID)
					matched++
				}
			}
			if empty := strings.Contains(testCase.name, "absent") || strings.Contains(testCase.name, "unknown"); empty != (matched == 0) {
				t.Fatalf("case selects %d seeded tasks", matched)
			}
			for _, group := range taskOperationsGroups {
				response := g.get("/v1/task-operations?workspace_id=demo&limit=200&group=" + string(group) + "&" + testCase.query)
				rows := decodeGroupRows(t, response)
				if got := rowIDs(rows); !slices.Equal(got, want[group]) && len(got)+len(want[group]) > 0 {
					t.Fatalf("group %s rows=%v want %v", group, got, want[group])
				}
				for _, row := range rows {
					if member, _ := taskOperationsGroupForActor(row.NextAction.Actor); member != group {
						t.Fatalf("group %s row %s actor %s", group, row.Task.ID, row.NextAction.Actor)
					}
				}
				if response.Header().Get("X-Conveyor-Total") != strconv.Itoa(len(want[group])) {
					t.Fatalf("group %s total=%s want %d", group, response.Header().Get("X-Conveyor-Total"), len(want[group]))
				}
				for _, member := range taskOperationsGroups {
					if groupCounts(response)[member] != strconv.Itoa(len(want[member])) {
						t.Fatalf("group %s counts=%v want %v", group, groupCounts(response), want)
					}
				}
				// Every one-row page, and one page past the end.
				for offset := 0; offset <= len(want[group]); offset++ {
					page := g.get(fmt.Sprintf("/v1/task-operations?workspace_id=demo&limit=1&offset=%d&group=%s&%s", offset, group, testCase.query))
					pageRows := decodeGroupRows(t, page)
					var wantPage []string
					if offset < len(want[group]) {
						wantPage = want[group][offset : offset+1]
					}
					if got := rowIDs(pageRows); !slices.Equal(got, wantPage) && len(got)+len(wantPage) > 0 {
						t.Fatalf("group %s offset %d rows=%v want %v", group, offset, got, wantPage)
					}
					if page.Header().Get("X-Conveyor-Total") != strconv.Itoa(len(want[group])) || page.Header().Get("X-Conveyor-Offset") != strconv.Itoa(offset) {
						t.Fatalf("group %s offset %d headers=%v", group, offset, page.Header())
					}
				}
			}
			ungrouped := decodeGroupRows(t, g.get("/v1/task-operations?workspace_id=demo&"+testCase.query))
			if len(ungrouped) != matched {
				t.Fatalf("ungrouped read selected %d tasks, want %d", len(ungrouped), matched)
			}
		})
	}
	// The fixture is meaningful: matching and nonmatching tasks sit in several groups.
	for name, set := range map[string]map[string]bool{"served": serves, "governed": governs, "assigned": assigned} {
		groups := map[taskOperationsGroup]bool{}
		for id := range set {
			groups[groupOf[id]] = true
		}
		if len(groups) < 3 || len(set) == len(groupOf) {
			t.Fatalf("%s tasks span only %v", name, groups)
		}
	}
}
