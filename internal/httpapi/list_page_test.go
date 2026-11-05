package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestParseListPage(t *testing.T) {
	for _, tc := range []struct {
		query   string
		dflt    int
		want    listPage
		refusal string
	}{
		{query: "", dflt: 50, want: listPage{Limit: 50, Paged: true}},
		{query: "limit=1", dflt: 50, want: listPage{Limit: 1, Paged: true}},
		{query: "limit=200&offset=2147483647", dflt: 50, want: listPage{Limit: 200, Offset: 2147483647, Paged: true}},
		// A document or membership list passes no default: without limit it
		// stays unpaged, and it still accepts and refuses the same parameters.
		{query: "", dflt: 0, want: listPage{}},
		{query: "limit=2&offset=1", dflt: 0, want: listPage{Limit: 2, Offset: 1, Paged: true}},
		{query: "limit=201", dflt: 0, refusal: "limit must be between 1 and 200"},
		{query: "offset=1", dflt: 0, refusal: "offset requires limit"},
		{query: "limit=0", dflt: 50, refusal: "limit must be between 1 and 200"},
		{query: "limit=-1", dflt: 50, refusal: "limit must be between 1 and 200"},
		{query: "limit=201", dflt: 50, refusal: "limit must be between 1 and 200"},
		{query: "limit=abc", dflt: 50, refusal: "limit must be between 1 and 200"},
		{query: "limit=", dflt: 50, refusal: "limit must be between 1 and 200"},
		{query: "limit=1&limit=2", dflt: 50, refusal: "limit must be supplied once"},
		{query: "offset=1", dflt: 50, refusal: "offset requires limit"},
		{query: "limit=1&offset=-1", dflt: 50, refusal: "offset must be between 0 and 2147483647"},
		{query: "limit=1&offset=abc", dflt: 50, refusal: "offset must be between 0 and 2147483647"},
		{query: "limit=1&offset=2147483648", dflt: 50, refusal: "offset must be between 0 and 2147483647"},
		{query: "limit=1&offset=0&offset=1", dflt: 50, refusal: "offset must be supplied once"},
	} {
		t.Run(fmt.Sprintf("%s default %d", tc.query, tc.dflt), func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseListPage(values, tc.dflt)
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("err = %v, want %q", err, tc.refusal)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("page = %+v err = %v, want %+v", got, err, tc.want)
			}
		})
	}
}

func TestListPageWriteHeaders(t *testing.T) {
	paged := httptest.NewRecorder()
	listPage{Limit: 5, Offset: 10, Paged: true}.writeHeaders(paged, 42)
	for name, want := range map[string]string{"X-Conveyor-Total": "42", "X-Conveyor-Limit": "5", "X-Conveyor-Offset": "10"} {
		if got := paged.Header().Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	unpaged := httptest.NewRecorder()
	listPage{}.writeHeaders(unpaged, 42)
	if len(unpaged.Header()) != 0 {
		t.Fatalf("unpaged headers = %+v", unpaged.Header())
	}
}

const ownedRoutesTask = "owned-routes-task"

// ownedRoute is one list route this rule covers. failing names the store read
// that fails the route, and defaultLimit the page the route applies without
// limit.
type ownedRoute struct {
	path         string
	failing      string
	defaultLimit int
	rows         func(t *testing.T, body []byte) int
}

func arrayRows(t *testing.T, body []byte) int {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode rows: %v: %s", err, body)
	}
	return len(rows)
}

func objectRows(field string) func(*testing.T, []byte) int {
	return func(t *testing.T, body []byte) int {
		t.Helper()
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil {
			t.Fatalf("decode object: %v: %s", err, body)
		}
		return arrayRows(t, object[field])
	}
}

func ownedRoutes() []ownedRoute {
	return []ownedRoute{
		{path: "/v1/next-actions", failing: "ListTasksFiltered", defaultLimit: defaultInboxPageLimit, rows: objectRows("items")},
		{path: "/v1/inbox", failing: "ListCallerAttentionTaskPage", defaultLimit: defaultInboxPageLimit, rows: objectRows("items")},
		{path: "/v1/inbox/acts", failing: "ListRecoveryActPage", defaultLimit: defaultInboxPageLimit, rows: objectRows("acts")},
		{path: "/v1/task-operations", failing: "ListTaskOperations", defaultLimit: defaultTaskListLimit, rows: arrayRows},
		{path: "/v1/attention/tasks", failing: "ListCallerAttentionTaskPage", defaultLimit: defaultTaskListLimit, rows: arrayRows},
		{path: "/v1/tasks/" + ownedRoutesTask + "/jobs", failing: "ListJobPage", defaultLimit: defaultHistoryPageLimit, rows: arrayRows},
		{path: "/v1/tasks/" + ownedRoutesTask + "/events", failing: "ListEventPage", defaultLimit: defaultHistoryPageLimit, rows: arrayRows},
	}
}

// ownedRoutesFixture seeds more rows than any owned route's default page: open
// operator-assigned gate tasks for the inbox and attention lists, and jobs,
// events, and recovery acts on one task.
func ownedRoutesFixture(t *testing.T, st store.Store) (*Server, int) {
	t.Helper()
	const rowCount = defaultTaskListLimit + 5
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	const owner = "usr_owner"
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.CreateTask(ctx, core.Task{ID: ownedRoutesTask, Workspace: "demo", Repo: "conveyor", Branch: ownedRoutesTask, State: core.TaskRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := range rowCount {
		id := fmt.Sprintf("owned-gate-%03d", i)
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Repo: "conveyor", Branch: id, State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, Assignee: &core.TaskAssignee{UserID: owner}, CreatedAt: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: fmt.Sprintf("owned-job-%03d", i), TaskID: ownedRoutesTask, Stage: core.StageImplement, State: core.JobPending, StartedAt: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendEvent(ctx, core.Event{TaskID: ownedRoutesTask, Kind: "work_order.recovered", ActorID: "clock", ActorRole: core.ActorSystem, At: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	server.Credentials = staticCredentialVerifier{"token": {ID: "pat_1", OwnerUserID: owner, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	return server, rowCount
}

func ownedRouteGet(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path, nil)))
	return response
}

func withQuery(path, query string) string {
	if query == "" {
		return path + "?workspace_id=demo"
	}
	return path + "?workspace_id=demo&" + query
}

// TestOwnedListRoutesFollowThePagingRule walks DEC-105(4) over each list route:
// a default page with all three headers, a named page, a total past the end,
// and 400 for every out-of-range value.
func TestOwnedListRoutesFollowThePagingRule(t *testing.T) {
	server, rowCount := ownedRoutesFixture(t, store.NewMemory())
	for _, route := range ownedRoutes() {
		t.Run(route.path, func(t *testing.T) {
			requirePage := func(response *httptest.ResponseRecorder, wantRows, wantLimit, wantOffset int) {
				t.Helper()
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if got := route.rows(t, response.Body.Bytes()); got != wantRows {
					t.Fatalf("rows = %d, want %d", got, wantRows)
				}
				total, err := strconv.Atoi(response.Header().Get("X-Conveyor-Total"))
				if err != nil || total < rowCount {
					t.Fatalf("total = %q, want at least %d", response.Header().Get("X-Conveyor-Total"), rowCount)
				}
				if response.Header().Get("X-Conveyor-Limit") != strconv.Itoa(wantLimit) || response.Header().Get("X-Conveyor-Offset") != strconv.Itoa(wantOffset) {
					t.Fatalf("headers = %+v, want limit %d offset %d", response.Header(), wantLimit, wantOffset)
				}
			}
			requirePage(ownedRouteGet(t, server, withQuery(route.path, "")), route.defaultLimit, route.defaultLimit, 0)
			requirePage(ownedRouteGet(t, server, withQuery(route.path, "limit=2&offset=1")), 2, 2, 1)
			requirePage(ownedRouteGet(t, server, withQuery(route.path, "limit=1&offset=2147483647")), 0, 1, 2147483647)
			for _, refused := range []string{"limit=0", "limit=-1", "limit=201", "limit=abc", "limit=1&limit=2", "offset=1", "limit=1&offset=-1", "limit=1&offset=2147483648"} {
				if response := ownedRouteGet(t, server, withQuery(route.path, refused)); response.Code != http.StatusBadRequest {
					t.Fatalf("%s status=%d body=%s", refused, response.Code, response.Body.String())
				}
			}
		})
	}
}

// TestOwnedListRoutesAnswerStoreFailureGenerically proves each route answers a
// failing store read with the generic 500, keeps the driver text out of the
// response, and logs the cause, redacted, with the request ID (DEC-105(1)).
func TestOwnedListRoutesAnswerStoreFailureGenerically(t *testing.T) {
	base := store.NewMemory()
	server, _ := ownedRoutesFixture(t, base)
	for _, route := range ownedRoutes() {
		t.Run(route.path, func(t *testing.T) {
			captured := captureStandardLog(t)
			server.Store = newFailingStore(base, route.failing)
			defer func() { server.Store = base }()
			response := ownedRouteGet(t, server, withQuery(route.path, ""))
			if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			logged := captured.String()
			if !strings.Contains(logged, "httpapi: store failure: request_id=") || strings.Contains(logged, `request_id=""`) || !strings.Contains(logged, "does not exist") {
				t.Fatalf("log = %q", logged)
			}
			if strings.Contains(logged, failingStoreCredential) {
				t.Fatalf("log carries the credential: %q", logged)
			}
		})
	}
}

// TestNextActionsAnswersActLogFailureGenerically covers the second store read of
// GET /v1/next-actions.
func TestNextActionsAnswersActLogFailureGenerically(t *testing.T) {
	base := store.NewMemory()
	server, _ := ownedRoutesFixture(t, base)
	captureStandardLog(t)
	server.Store = newFailingStore(base, "ListFactoryActs")
	response := ownedRouteGet(t, server, withQuery("/v1/next-actions", ""))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "does not exist") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

// TestGetNextActionSeparatesMissingFromOutage keeps an outage from reading as
// not found: a missing task answers 404, and any other store error answers the
// generic 500 (DEC-105(1), (2)).
func TestGetNextActionSeparatesMissingFromOutage(t *testing.T) {
	base := store.NewMemory()
	server, _ := ownedRoutesFixture(t, base)
	missing := ownedRouteGet(t, server, withQuery("/v1/tasks/absent/next-action", ""))
	if missing.Code != http.StatusNotFound || strings.TrimSpace(missing.Body.String()) != "task not found" {
		t.Fatalf("missing status=%d body=%q", missing.Code, missing.Body.String())
	}
	captureStandardLog(t)
	server.Store = newFailingStore(base, "GetTask")
	outage := ownedRouteGet(t, server, withQuery("/v1/tasks/"+ownedRoutesTask+"/next-action", ""))
	if outage.Code != http.StatusInternalServerError || strings.TrimSpace(outage.Body.String()) != "internal server error" {
		t.Fatalf("outage status=%d body=%q", outage.Code, outage.Body.String())
	}
}

// TestAttentionRefusesEveryFilterParameter pins the route to workspace_id and
// the page parameters. A web client sends only limit and offset.
func TestAttentionRefusesEveryFilterParameter(t *testing.T) {
	server, _ := ownedRoutesFixture(t, store.NewMemory())
	for _, name := range []string{"state", "repository", "assignee", "subject", "query", "updated_from", "created_to", "since", "unknown"} {
		response := ownedRouteGet(t, server, withQuery("/v1/attention/tasks", name+"=value"))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), name) {
			t.Fatalf("%s status=%d body=%q", name, response.Code, response.Body.String())
		}
	}
	if response := ownedRouteGet(t, server, withQuery("/v1/attention/tasks", "limit=1&offset=0")); response.Code != http.StatusOK {
		t.Fatalf("page parameters status=%d body=%s", response.Code, response.Body.String())
	}
}

// TestInboxActsStatesRecoveryActsNewestFirst pins the route's rows to the store
// page: each act keeps its kind and the newest act leads.
func TestInboxActsStatesRecoveryActsNewestFirst(t *testing.T) {
	server, rowCount := ownedRoutesFixture(t, store.NewMemory())
	response := ownedRouteGet(t, server, withQuery("/v1/inbox/acts", "limit=2"))
	var body struct {
		Acts []struct {
			Kind      string    `json:"kind"`
			At        time.Time `json:"at"`
			Automatic bool      `json:"automatic"`
		} `json:"acts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Header().Get("X-Conveyor-Total") != strconv.Itoa(rowCount) || len(body.Acts) != 2 || body.Acts[0].Kind != "recovered" || !body.Acts[0].Automatic || !body.Acts[0].At.After(body.Acts[1].At) {
		t.Fatalf("total=%q acts=%+v", response.Header().Get("X-Conveyor-Total"), body.Acts)
	}
	if bad := ownedRouteGet(t, server, withQuery("/v1/inbox/acts", "since=yesterday")); bad.Code != http.StatusBadRequest {
		t.Fatalf("since status=%d body=%s", bad.Code, bad.Body.String())
	}
}
