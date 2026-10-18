package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

const taskMapTestBody = "TASK-MAP-HANDLER-BODY-SENTINEL"

// taskMapTestFixture seeds one workspace: an open conveyor task depending on
// a merged and a closed task, an open app task, and an unrelated merged task,
// with requirement attachments on each.
type taskMapTestFixture struct {
	openConveyor, openApp, depMerged, depClosed, unrelated string
	requirement                                            string
}

func seedTaskMap(t *testing.T, ctx context.Context, st store.Store, workspace string) taskMapTestFixture {
	t.Helper()
	ctx = store.WithActor(store.WithWorkspace(ctx, workspace), store.Actor{ID: "operator", Role: core.ActorHuman})
	created := time.Now().UTC().Add(-time.Hour)
	newTask := func(title, repo string, dependencies ...string) string {
		t.Helper()
		id := workspace + "-" + core.NewTaskID()
		created = created.Add(time.Second)
		task := core.Task{ID: id, Workspace: workspace, Repo: repo, BaseBranch: "main", Branch: "conveyor/task-" + id, Title: title, Body: taskMapTestBody, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: created}
		if err := st.CreateTaskWithDependencies(ctx, task, dependencies); err != nil {
			t.Fatal(err)
		}
		return id
	}
	requirementID := "req-map-" + workspace
	_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: requirementID, Title: "Map " + workspace}, core.RequirementVersion{Content: "# Map " + workspace, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The map counts tasks."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirementID, version.Version); err != nil {
		t.Fatal(err)
	}
	f := taskMapTestFixture{requirement: requirementID}
	f.depMerged = newTask("merged dependency", "app")
	f.depClosed = newTask("closed dependency", "conveyor")
	f.openConveyor = newTask("open conveyor", "conveyor", f.depMerged, f.depClosed)
	f.openApp = newTask("open app", "app")
	f.unrelated = newTask("unrelated merged", "conveyor")
	for _, id := range []string{f.openConveyor, f.openApp, f.depMerged, f.unrelated, f.depClosed} {
		if _, err = st.UpdateTaskContext(ctx, id, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirementID}}}); err != nil {
			t.Fatal(err)
		}
	}
	plane := taskops.New(st)
	for id, command := range map[string]core.TaskCommand{f.depMerged: core.TaskMergeRecover, f.unrelated: core.TaskMergeRecover, f.depClosed: core.TaskCancel} {
		if _, err = plane.Perform(ctx, id, taskops.Command{Kind: command}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

type taskMapWire struct {
	Tasks []struct {
		ID                   string             `json:"id"`
		Repo                 string             `json:"repo"`
		State                core.TaskState     `json:"state"`
		LastEventAt          time.Time          `json:"last_event_at"`
		NextAction           nextaction.Compact `json:"next_action"`
		DependsOn            []string           `json:"depends_on"`
		BlockingTaskIDs      []string           `json:"blocking_task_ids"`
		UnsatisfiableTaskIDs []string           `json:"unsatisfiable_task_ids"`
		Requirements         []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"requirements"`
	} `json:"tasks"`
	Requirements []struct {
		ID              string `json:"id"`
		Title           string `json:"title"`
		OpenTaskCount   int    `json:"open_task_count"`
		MergedTaskCount int    `json:"merged_task_count"`
		TotalTaskCount  int    `json:"total_task_count"`
	} `json:"requirements"`
}

// The task map answers one compact envelope per req-board-map AC-3.1: the
// strict field allowlist, both finished-task scopes, the repository filter
// with its alias, counts independent of the filters, and the shared compact
// next action (DEC-61).
func TestTaskMapEnvelopeScopesAndFilters(t *testing.T) {
	ctx := store.WithActor(t.Context(), store.SystemActor())
	st := store.NewMemory()
	f := seedTaskMap(t, ctx, st, "demo")
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	server.Credentials = staticCredentialVerifier{"token": {ID: "pat_1", OwnerUserID: "usr_1", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	call := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/task-map?workspace_id=demo"+query, nil)))
		return response
	}
	read := func(query string, want ...string) taskMapWire {
		t.Helper()
		response := call(query)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", query, response.Code, response.Body.String())
		}
		raw := response.Body.String()
		if strings.Contains(raw, taskMapTestBody) {
			t.Fatalf("%s response carried a task body: %s", query, raw)
		}
		var envelope map[string][]map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s decode envelope: %v body=%s", query, err, raw)
		}
		if keys := mapKeys(envelope); !slices.Equal(keys, []string{"requirements", "tasks"}) {
			t.Fatalf("%s envelope keys=%v", query, keys)
		}
		allowed := []string{"blocking_task_ids", "depends_on", "id", "last_event_at", "next_action", "repo", "requirements", "state", "title", "unsatisfiable_task_ids"}
		for _, item := range envelope["tasks"] {
			if keys := mapKeys(item); !slices.Equal(keys, allowed) {
				t.Fatalf("%s task keys=%v want %v", query, keys, allowed)
			}
		}
		var decoded taskMapWire
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, item := range decoded.Tasks {
			ids = append(ids, item.ID)
		}
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		sort.Strings(want)
		if !slices.Equal(sorted, want) {
			t.Fatalf("%s tasks=%v want %v", query, sorted, want)
		}
		for i := 1; i < len(decoded.Tasks); i++ {
			previous, current := decoded.Tasks[i-1], decoded.Tasks[i]
			if previous.LastEventAt.Before(current.LastEventAt) || (previous.LastEventAt.Equal(current.LastEventAt) && previous.ID > current.ID) {
				t.Fatalf("%s order %s@%s before %s@%s", query, previous.ID, previous.LastEventAt, current.ID, current.LastEventAt)
			}
		}
		if len(decoded.Requirements) != 1 || decoded.Requirements[0].ID != f.requirement || decoded.Requirements[0].OpenTaskCount != 2 || decoded.Requirements[0].MergedTaskCount != 2 || decoded.Requirements[0].TotalTaskCount != 4 {
			t.Fatalf("%s requirements=%+v", query, decoded.Requirements)
		}
		return decoded
	}

	all := read("", f.openConveyor, f.openApp, f.depMerged, f.depClosed)
	opts, err := server.nextActionOptionsFor(store.WithWorkspace(ctx, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range all.Tasks {
		one, err := store.ReadNextAction(store.WithWorkspace(ctx, "demo"), st, item.ID, opts)
		if err != nil {
			t.Fatal(err)
		}
		want := nextaction.CompactFrom(one)
		if item.NextAction.Kind != want.Kind || item.NextAction.Label != want.Label || item.NextAction.Actor != want.Actor || item.NextAction.Judgment != want.Judgment || !reflect.DeepEqual(item.NextAction.BlockedBy, want.BlockedBy) {
			t.Fatalf("task %s next_action=%+v want %+v", item.ID, item.NextAction, want)
		}
		if len(item.Requirements) != 1 || item.Requirements[0].ID != f.requirement || item.Requirements[0].Title != "Map demo" {
			t.Fatalf("task %s requirements=%+v", item.ID, item.Requirements)
		}
		if item.ID == f.openConveyor {
			wantDeps := []string{f.depMerged, f.depClosed}
			sort.Strings(wantDeps)
			if !slices.Equal(item.DependsOn, wantDeps) || !slices.Equal(item.BlockingTaskIDs, []string{f.depClosed}) || !slices.Equal(item.UnsatisfiableTaskIDs, []string{f.depClosed}) {
				t.Fatalf("dependency relations=%+v", item)
			}
		}
	}
	read("&repository=app", f.openApp)
	read("&repo=app", f.openApp)
	read("&repository=conveyor", f.openConveyor, f.depClosed)
	read("&repository=app&repository=conveyor", f.openConveyor, f.openApp, f.depMerged, f.depClosed)
	read("&include_finished=true", f.openConveyor, f.openApp, f.depMerged, f.depClosed, f.unrelated)
	read("&include_finished=true&repository=conveyor", f.openConveyor, f.depClosed, f.unrelated)
	read("&include_finished=false", f.openConveyor, f.openApp, f.depMerged, f.depClosed)
	empty := call("&repository=absent")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"tasks":[]`) {
		t.Fatalf("empty selection status=%d body=%s", empty.Code, empty.Body.String())
	}
	for _, query := range []string{"&include_finished=maybe", "&include_finished=", "&state=unknown"} {
		if response := call(query); response.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
}

// The map read requires the workspace view capability, answers an unbound
// workspace exactly as a missing one, and never returns another workspace's
// tasks or counts (DEC-19).
func TestTaskMapAuthorizationAndWorkspaceIsolation(t *testing.T) {
	ctx := store.WithActor(t.Context(), store.SystemActor())
	st := store.NewMemory()
	demo := seedTaskMap(t, ctx, st, "demo")
	other := seedTaskMap(t, ctx, st, "other")
	members := &membershipFixture{
		workspaces: []core.Workspace{{ID: "demo"}, {ID: "other"}, {ID: "empty"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"viewer":   {"demo": core.WorkspaceRoleViewer, "empty": core.WorkspaceRoleViewer},
			"operator": {"demo": core.WorkspaceRoleOperator, "other": core.WorkspaceRoleOperator},
		},
	}
	server := NewServer(st)
	server.Workspaces, server.Memberships = members, members
	server.Credentials = staticCredentialVerifier{
		"viewer":   {ID: "viewer", OwnerUserID: "viewer", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"operator": {ID: "operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	call := func(token, query string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/task-map"+query, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := call("", "?workspace_id=demo"); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("forged", "?workspace_id=demo"); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid credential status=%d body=%s", response.Code, response.Body.String())
	}
	unbound, missing := call("viewer", "?workspace_id=other"), call("viewer", "?workspace_id=absent")
	if unbound.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || unbound.Body.String() != missing.Body.String() {
		t.Fatalf("unbound=%d/%q missing=%d/%q", unbound.Code, unbound.Body.String(), missing.Code, missing.Body.String())
	}
	viewed := call("viewer", "?workspace_id=demo&include_finished=true")
	if viewed.Code != http.StatusOK {
		t.Fatalf("viewer status=%d body=%s", viewed.Code, viewed.Body.String())
	}
	for _, id := range []string{other.openConveyor, other.openApp, other.depMerged, other.unrelated, other.requirement} {
		if strings.Contains(viewed.Body.String(), id) {
			t.Fatalf("demo map leaked %s: %s", id, viewed.Body.String())
		}
	}
	for _, id := range []string{demo.openConveyor, demo.openApp, demo.unrelated, demo.requirement} {
		if !strings.Contains(viewed.Body.String(), id) {
			t.Fatalf("demo map missing %s: %s", id, viewed.Body.String())
		}
	}
	operated := call("operator", "?workspace_id=other")
	if operated.Code != http.StatusOK || !strings.Contains(operated.Body.String(), other.openApp) || strings.Contains(operated.Body.String(), demo.openApp) {
		t.Fatalf("other map status=%d body=%s", operated.Code, operated.Body.String())
	}
	empty := call("viewer", "?workspace_id=empty")
	if empty.Code != http.StatusOK || strings.TrimSpace(empty.Body.String()) != `{"tasks":[],"requirements":[]}` {
		t.Fatalf("empty workspace status=%d body=%s", empty.Code, empty.Body.String())
	}
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
