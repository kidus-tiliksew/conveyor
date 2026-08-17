package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type fakeWorkspaceControl struct {
	items   []core.Workspace
	created *config.Config
}

func (f *fakeWorkspaceControl) ListWorkspaces(context.Context) ([]core.Workspace, error) {
	return append([]core.Workspace(nil), f.items...), nil
}
func (f *fakeWorkspaceControl) GetWorkspace(_ context.Context, id string) (core.Workspace, error) {
	for _, item := range f.items {
		if item.ID == id {
			return item, nil
		}
	}
	return core.Workspace{}, context.Canceled
}
func (f *fakeWorkspaceControl) CreateWorkspace(_ context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	f.created = cfg
	item := core.Workspace{ID: id, Name: name, ConfigVersion: 1, CreatedAt: time.Now()}
	f.items = append(f.items, item)
	return item, nil
}

type workspaceAwareStore struct {
	store.Store
	tasks map[string][]core.Task
}

func (s workspaceAwareStore) ListTasks(ctx context.Context) ([]core.Task, error) {
	id, _ := store.WorkspaceFromContext(ctx)
	return append([]core.Task(nil), s.tasks[id]...), nil
}
func (s workspaceAwareStore) ListActivityMarkers(context.Context) ([]store.ActivityMarker, error) {
	return nil, nil
}

func TestWorkspaceContextFailsClosedAndIsolatesLists(t *testing.T) {
	control := &fakeWorkspaceControl{items: []core.Workspace{{ID: "alpha", Name: "Alpha"}, {ID: "beta", Name: "Beta"}}}
	srv := NewServer(workspaceAwareStore{Store: store.NewMemory(), tasks: map[string][]core.Task{"alpha": {{ID: "a", Workspace: "alpha"}}, "beta": {{ID: "b", Workspace: "beta"}}}})
	memberships := &membershipFixture{
		workspaces: control.items,
		roles:      map[string]map[string]core.WorkspaceRole{"local-operator": {"alpha": core.WorkspaceRoleOperator, "beta": core.WorkspaceRoleOperator}},
	}
	srv.Workspaces, srv.Memberships, srv.BearerToken = control, memberships, "token"
	h := srv.Handler()

	ambiguous := httptest.NewRecorder()
	ambiguousReq := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	ambiguousReq.Header.Set("Authorization", "Bearer token")
	h.ServeHTTP(ambiguous, ambiguousReq)
	if ambiguous.Code != http.StatusConflict {
		t.Fatalf("ambiguous status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}
	selected := httptest.NewRecorder()
	selectedReq := httptest.NewRequest(http.MethodGet, "/v1/tasks?workspace_id=beta", nil)
	selectedReq.Header.Set("Authorization", "Bearer token")
	h.ServeHTTP(selected, selectedReq)
	if selected.Code != http.StatusOK {
		t.Fatalf("selected status=%d body=%s", selected.Code, selected.Body.String())
	}
	var tasks []core.Task
	if err := json.Unmarshal(selected.Body.Bytes(), &tasks); err != nil || len(tasks) != 1 || tasks[0].Workspace != "beta" {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	conflict := httptest.NewRequest(http.MethodGet, "/v1/tasks?workspace_id=alpha", nil)
	conflict.Header.Set("Authorization", "Bearer token")
	conflict.Header.Set("X-Workspace-ID", "beta")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, conflict)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("conflicting context status=%d", w.Code)
	}
	pathConflict := httptest.NewRequest(http.MethodGet, "/v1/workspaces/alpha?workspace_id=beta", nil)
	pathConflict.Header.Set("Authorization", "Bearer token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, pathConflict)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("conflicting path context status=%d", w.Code)
	}
}

func TestCreateWorkspaceValidatesAndUsesDefaults(t *testing.T) {
	control := &fakeWorkspaceControl{}
	deployment := &config.Config{Workspace: "demo", MaxBounces: 2, Database: config.Database{Backend: "memory"}, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", TimeoutText: "1h", Execution: config.ExecutionInProcess}, "spec": {Model: "gpt", TimeoutText: "1h", Execution: config.ExecutionInProcess}, "implement": {Model: "operator", TimeoutText: "1h", Execution: config.ExecutionMCP}, "review": {Model: "operator", TimeoutText: "1h", Execution: config.ExecutionMCP}}}, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	srv := NewServer(store.NewMemory())
	srv.Workspaces, srv.Deployment, srv.BearerToken = control, deployment, "token"
	queued := ""
	srv.EnsureWorkspaceQueues = func(id string) error { queued = id; return nil }
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces", bytes.NewBufferString(`{"id":"engineering","name":"Engineering"}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if control.created == nil || control.created.Workspace != "engineering" || queued != "engineering" {
		t.Fatalf("created=%+v queue=%q", control.created, queued)
	}
}

func TestCreateWorkspaceDoesNotPersistWhenQueueRegistrationFails(t *testing.T) {
	control := &fakeWorkspaceControl{}
	srv := NewServer(store.NewMemory())
	srv.Workspaces, srv.Deployment, srv.BearerToken = control, &config.Config{}, "token"
	srv.EnsureWorkspaceQueues = func(string) error { return context.Canceled }
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces", bytes.NewBufferString(`{"id":"engineering","name":"Engineering"}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if control.created != nil || len(control.items) != 0 {
		t.Fatalf("workspace persisted despite queue registration failure: created=%+v items=%+v", control.created, control.items)
	}
}
