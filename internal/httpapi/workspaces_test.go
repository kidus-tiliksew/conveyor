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
	srv.Memberships = localOperatorInstanceAdmin()
	queued := ""
	srv.EnsureWorkspaceQueues = func(id string, candidate *config.Config) error {
		if candidate == nil || candidate.Workspace != id || candidate.Routing.Stages["triage"].TimeoutText != "1h" || control.created != nil {
			t.Fatalf("queue preflight must receive the candidate before persistence: %+v", candidate)
		}
		queued = id
		return nil
	}
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
	createWorkspaceAcceptsPolicyOnlyDocuments(t)
}

func TestCreateWorkspaceDoesNotPersistWhenQueueRegistrationFails(t *testing.T) {
	control := &fakeWorkspaceControl{}
	srv := NewServer(store.NewMemory())
	srv.Workspaces, srv.Deployment, srv.BearerToken = control, &config.Config{}, "token"
	srv.Memberships = localOperatorInstanceAdmin()
	srv.EnsureWorkspaceQueues = func(string, *config.Config) error { return context.Canceled }
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

// legacyExecutorDeployment is a deployment value that still carries execution
// detail, as a file loaded before DEC-56 did.
func legacyExecutorDeployment() *config.Config {
	harness := config.HarnessTemplates()[0].Harness
	return &config.Config{
		Workspace: "demo", MaxBounces: 2, Database: config.Database{Backend: "memory"},
		Harnesses:    []config.Harness{harness},
		Setups:       []config.ExecutionSetup{{Name: "default"}},
		DefaultSetup: "default",
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"triage":    {Model: "gpt", TimeoutText: "1h", Execution: config.ExecutionInProcess},
			"spec":      {Model: "gpt-sol", Harness: harness.Name, Effort: "high", TimeoutText: "30m", Execution: config.ExecutionMCP},
			"implement": {Model: "gpt-sol", Harness: harness.Name, Effort: "high", TimeoutText: "4h", Execution: config.ExecutionMCP},
			"review":    {Model: "gpt-terra", Harness: harness.Name, TimeoutText: "1h", Execution: config.ExecutionMCP},
		}},
		Review:         config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-terra", Harness: harness.Name, Effort: "high"}}},
		PlanningModels: []string{"gpt"},
		Repos:          []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}},
	}
}

func assertCandidateHasNoExecutionDetail(t *testing.T, candidate *config.Config) {
	t.Helper()
	if candidate.Harnesses != nil || candidate.Setups != nil || candidate.DefaultSetup != "" || candidate.ExecutionSettings != nil || candidate.PlanningModels != nil {
		t.Fatalf("candidate inherited execution detail: harnesses=%v setups=%v default=%q settings=%v planning=%v", candidate.Harnesses, candidate.Setups, candidate.DefaultSetup, candidate.ExecutionSettings, candidate.PlanningModels)
	}
	for _, stage := range []string{"spec", "implement", "review", "verify"} {
		route := candidate.Routing.Stages[stage]
		if route.Model != "" || route.Harness != "" || route.Effort != "" || route.ModelPolicy != "" {
			t.Fatalf("candidate inherited %s execution detail: %+v", stage, route)
		}
	}
	for i, seat := range candidate.Review.Seats {
		if seat != (config.ReviewSeat{}) {
			t.Fatalf("candidate inherited seat %d contents: %+v", i, seat)
		}
	}
}

// DEC-56(2); component-http-api "Workspace configuration and creation":
// creation refuses every client-local execution key with the configuration
// PUT's error shape, before queue registration or persistence.
func TestCreateWorkspaceRejectsExecutionDetailMatchesPUT(t *testing.T) {
	keys := []string{
		"verify_concurrency", "execution_settings", "routing", "harnesses", "setups", "default_setup",
		"planning_models", "model", "model_policy", "harness", "effort", "argv", "command",
		"model_args", "effort_args", "probe_command", "mcp_transport",
	}
	values := map[string]any{"null": nil, "empty string": "", "empty object": map[string]any{}, "empty list": []any{}, "value": "x"}
	positions := map[string]func(key string, value any) map[string]any{
		"top level": func(key string, value any) map[string]any { return map[string]any{key: value} },
		"nested execution": func(key string, value any) map[string]any {
			return map[string]any{"execution": map[string]any{"spec_approval": true, key: value}}
		},
		"review seat in array": func(key string, value any) map[string]any {
			return map[string]any{"review": map[string]any{"seats": []any{map[string]any{}, map[string]any{key: value}}}}
		},
		"repository in array": func(key string, value any) map[string]any {
			return map[string]any{"repos": []any{map[string]any{"name": "repo", "url": "https://example.test/repo", key: value}}}
		},
	}
	for _, key := range keys {
		for positionName, position := range positions {
			for valueName, value := range values {
				document := position(key, value)

				control := &fakeWorkspaceControl{}
				create := NewServer(store.NewMemory())
				create.Workspaces, create.Deployment, create.BearerToken = control, legacyExecutorDeployment(), "token"
				create.Memberships = localOperatorInstanceAdmin()
				queued := false
				create.EnsureWorkspaceQueues = func(string, *config.Config) error {
					queued = true
					return nil
				}
				postBody, _ := json.Marshal(map[string]any{"id": "engineering", "name": "Engineering", "document": document})
				post := httptest.NewRequest(http.MethodPost, "/v1/workspaces", bytes.NewReader(postBody))
				post.Header.Set("Authorization", "Bearer token")
				postResult := httptest.NewRecorder()
				create.Handler().ServeHTTP(postResult, post)

				backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: contextualWorkspaceDocument(), Version: 1}}
				update := NewServer(store.NewMemory())
				update.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
				update.ConfigStore, update.BearerToken = backend, "token"
				putBody, _ := json.Marshal(map[string]any{"document": document})
				put := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", bytes.NewReader(putBody))
				put.Header.Set("Authorization", "Bearer token")
				put.Header.Set("If-Match", "1")
				putResult := httptest.NewRecorder()
				update.Handler().ServeHTTP(putResult, put)

				label := key + "/" + positionName + "/" + valueName
				if postResult.Code != http.StatusUnprocessableEntity || putResult.Code != http.StatusUnprocessableEntity {
					t.Fatalf("%s: POST status=%d PUT status=%d body=%s", label, postResult.Code, putResult.Code, postResult.Body)
				}
				if postResult.Body.String() != putResult.Body.String() {
					t.Fatalf("%s: POST body %s differs from PUT body %s", label, postResult.Body, putResult.Body)
				}
				var refusal struct {
					Error  string             `json:"error"`
					Fields []configFieldError `json:"fields"`
				}
				if err := json.Unmarshal(postResult.Body.Bytes(), &refusal); err != nil || refusal.Error != "validation_failed" || len(refusal.Fields) != 1 || refusal.Fields[0].Field != key || refusal.Fields[0].Message != key+" is retired execution detail and must not be supplied" {
					t.Fatalf("%s: refusal=%+v err=%v", label, refusal, err)
				}
				if queued || control.created != nil || len(control.items) != 0 || backend.updates != 0 {
					t.Fatalf("%s: refusal had side effects: queued=%v created=%v updates=%d", label, queued, control.created != nil, backend.updates)
				}
			}
		}
	}
}

// A policy-only partial document, an omitted document, and a null document
// are accepted; none inherits execution detail from a legacy deployment.
func createWorkspaceAcceptsPolicyOnlyDocuments(t *testing.T) {
	for name, test := range map[string]struct {
		body  string
		check func(*testing.T, *config.Config)
	}{
		"partial policy": {
			body: `{"id":"engineering","name":"Engineering","document":{"workspace":"engineering","max_bounces":4,"work_order_queue_timeout":"12h","stage_timeouts":{"implement":"3h","verify":"2h"},"review":{"seats":[{},{}]},"execution":{"verify_stage":true,"spec_approval":false,"merge_approval":true,"implement_concurrency":2,"review_concurrency":1},"repos":[{"name":"app","url":"https://example.test/app","base":"trunk"}],"monitor":{"enabled":false}}}`,
			check: func(t *testing.T, candidate *config.Config) {
				if candidate.MaxBounces != 4 || candidate.WorkOrderQueueTimeoutText != "12h" || len(candidate.Review.Seats) != 2 {
					t.Fatalf("partial policy not applied: bounces=%d queue=%q seats=%d", candidate.MaxBounces, candidate.WorkOrderQueueTimeoutText, len(candidate.Review.Seats))
				}
				if candidate.Routing.Stages["implement"].TimeoutText != "3h" || candidate.Routing.Stages["verify"].TimeoutText != "2h" || candidate.Routing.Stages["spec"].TimeoutText != "30m" {
					t.Fatalf("stage timeouts = %+v", candidate.Routing.Stages)
				}
				if candidate.Execution.SpecApproval || !candidate.Execution.MergeApproval || !candidate.Execution.VerifyStage || candidate.Execution.ImplementConcurrency != 2 {
					t.Fatalf("execution policy = %+v", candidate.Execution)
				}
				if len(candidate.Repos) != 1 || candidate.Repos[0].Name != "app" {
					t.Fatalf("repos = %+v", candidate.Repos)
				}
			},
		},
		"omitted document": {
			body: `{"id":"engineering","name":"Engineering"}`,
			check: func(t *testing.T, candidate *config.Config) {
				if candidate.MaxBounces != 2 || len(candidate.Review.Seats) != 1 || candidate.Routing.Stages["implement"].TimeoutText != "4h" || len(candidate.Repos) != 1 {
					t.Fatalf("deployment policy defaults not applied: %+v", candidate)
				}
			},
		},
		"null document": {
			body: `{"id":"engineering","name":"Engineering","document":null}`,
			check: func(t *testing.T, candidate *config.Config) {
				if candidate.MaxBounces != 2 || candidate.Routing.Stages["review"].TimeoutText != "1h" {
					t.Fatalf("deployment policy defaults not applied: %+v", candidate)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			control := &fakeWorkspaceControl{}
			srv := NewServer(store.NewMemory())
			srv.Workspaces, srv.Deployment, srv.BearerToken = control, legacyExecutorDeployment(), "token"
			srv.Memberships = localOperatorInstanceAdmin()
			var preflight *config.Config
			srv.EnsureWorkspaceQueues = func(id string, candidate *config.Config) error {
				if control.created != nil {
					t.Fatal("queue preflight ran after persistence")
				}
				preflight = candidate
				return nil
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/workspaces", bytes.NewBufferString(test.body))
			req.Header.Set("Authorization", "Bearer token")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if control.created == nil || control.created != preflight || control.created.Workspace != "engineering" {
				t.Fatalf("created=%+v preflight=%+v", control.created, preflight)
			}
			if triage := control.created.Routing.Stages["triage"]; triage.Model != "gpt" || triage.Execution != config.ExecutionInProcess {
				t.Fatalf("deployment control-plane route lost for queue preflight: %+v", triage)
			}
			assertCandidateHasNoExecutionDetail(t, control.created)
			test.check(t, control.created)
		})
	}
	for name, test := range map[string]struct{ body, field string }{
		"workspace mismatch": {`{"id":"engineering","name":"Engineering","document":{"workspace":"other"}}`, "document.workspace"},
		"unknown field":      {`{"id":"engineering","name":"Engineering","document":{"unexpected":true}}`, "document"},
		"empty seats":        {`{"id":"engineering","name":"Engineering","document":{"review":{"seats":[]}}}`, "review"},
		"invalid bounces":    {`{"id":"engineering","name":"Engineering","document":{"max_bounces":-1}}`, "max_bounces"},
		"unknown top field":  {`{"id":"engineering","name":"Engineering","extra":1}`, "workspace"},
	} {
		t.Run(name, func(t *testing.T) {
			control := &fakeWorkspaceControl{}
			srv := NewServer(store.NewMemory())
			srv.Workspaces, srv.Deployment, srv.BearerToken = control, legacyExecutorDeployment(), "token"
			srv.Memberships = localOperatorInstanceAdmin()
			srv.EnsureWorkspaceQueues = func(string, *config.Config) error {
				t.Fatal("queue preflight ran for an invalid document")
				return nil
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/workspaces", bytes.NewBufferString(test.body))
			req.Header.Set("Authorization", "Bearer token")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnprocessableEntity || !bytes.Contains(w.Body.Bytes(), []byte(`"field":"`+test.field+`"`)) || control.created != nil {
				t.Fatalf("status=%d body=%s created=%v", w.Code, w.Body.String(), control.created != nil)
			}
		})
	}
}

// localOperatorInstanceAdmin admits the configured-token fallback user as the
// instance-administration principal; without an authority the creation route
// fails closed (DEC-63(5)).
func localOperatorInstanceAdmin() *membershipFixture {
	return &membershipFixture{instanceAdmins: map[string]bool{"local-operator": true}}
}
