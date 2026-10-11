package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestTaskStartOverHTTP(t *testing.T) {
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator} {
		t.Run(string(role), func(t *testing.T) {
			st := store.NewMemory()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			old := core.Task{ID: "restart-http", Workspace: "demo", Repo: "conveyor", Body: "Start again", Title: "Restart", State: core.TaskRunning, Branch: "conveyor/task-restart-http"}
			if err := st.CreateTask(ctx, old); err != nil {
				t.Fatal(err)
			}
			requirement, pending, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-http", Title: "Pending"}, core.RequirementVersion{Content: "# Proposal", Origin: core.RequirementOriginImplementation, OriginTaskID: old.ID, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "A proposal."}}})
			if err != nil {
				t.Fatal(err)
			}
			membership := &membershipFixture{workspaces: []core.Workspace{{ID: "demo", Name: "Demo"}}, roles: map[string]map[string]core.WorkspaceRole{"usr_restart": {"demo": role}}}
			server := NewServer(st)
			server.Workspaces, server.Memberships = membership, membership
			server.Credentials = staticCredentialVerifier{"operator-token": {ID: "pat_restart", OwnerUserID: "usr_restart", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}, "agent-token": {ID: "agent_restart", OwnerUserID: "usr_restart", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}}
			handler := server.Handler()
			created := 0
			server.OnCreate = func(context.Context, string) { created++ }
			call := func(body, key, token string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+old.ID+"/restart", strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("X-Workspace-ID", "demo")
				r.Header.Set("X-Idempotency-Key", key)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			payload := `{"reason":"Fresh start","note":"Keep it small"}`
			agent := call(payload, "request-1", "agent-token")
			if agent.Code < 400 {
				t.Fatalf("agent admitted: %d %s", agent.Code, agent.Body.String())
			}
			response := call(payload, "request-1", "operator-token")
			if role == core.WorkspaceRoleViewer {
				if response.Code < 400 {
					t.Fatal("viewer admitted")
				}
				return
			}
			if role == core.WorkspaceRoleMaintainer {
				if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "confirm_documents") {
					t.Fatalf("missing capability: %d %s", response.Code, response.Body.String())
				}
				task, _ := st.GetTask(ctx, old.ID)
				if task.State != old.State || created != 0 {
					t.Fatal("refusal mutated task")
				}
				if _, _, err := st.DismissRequirementVersion(ctx, requirement.ID, pending.Version); err != nil {
					t.Fatal(err)
				}
				response = call(payload, "request-1", "operator-token")
			}
			if response.Code != http.StatusCreated {
				t.Fatalf("start: %d %s", response.Code, response.Body.String())
			}
			var got core.TaskStartOverResult
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Task.SupersededBy != got.Successor.ID || got.Successor.Supersedes != old.ID || created != 1 {
				t.Fatalf("response=%+v callbacks=%d", got, created)
			}
			response = call(payload, "request-1", "operator-token")
			if response.Code != http.StatusOK || created != 1 {
				t.Fatalf("replay: %d %s", response.Code, response.Body.String())
			}
			response = call(payload, "request-2", "operator-token")
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "POST /v1/tasks") {
				t.Fatalf("terminal: %d %s", response.Code, response.Body.String())
			}
			summary, _ := json.Marshal(summarizeActivityTask(got.Task))
			if !bytes.Contains(summary, []byte(`"superseded_by"`)) {
				t.Fatalf("activity=%s", summary)
			}
		})
	}
}

func restartHTTPFixture(t *testing.T, taskID string) http.Handler {
	t.Helper()
	st := store.NewMemory()
	if err := st.CreateTask(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.Credentials = staticCredentialVerifier{"token": {ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	return server.Handler()
}

func TestTaskStartOverValidation(t *testing.T) {
	handler := restartHTTPFixture(t, "validation-http")
	for _, body := range []string{`{"request_id":"r","reason":"why"} {}`, `{}`, `{"request_id":"r"}`, `{"reason":"why"}`, `{"request_id":"r","reason":" "}`, `{"request_id":"r","reason":"` + strings.Repeat("x", 201) + `"}`, `{"request_id":"r","reason":"why","note":"` + strings.Repeat("x", 2001) + `"}`, `{"request_id":"r","reason":"why","can_confirm_documents":true}`} {
		r := httptest.NewRequest(http.MethodPost, "/v1/tasks/validation-http/restart", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("validation %d: %s", w.Code, w.Body.String())
		}
		assertRestartError(t, w, http.StatusBadRequest, "invalid_restart_request")
	}
}

func TestTaskStartOverConcurrentHTTP(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if err := st.CreateTask(ctx, core.Task{ID: "concurrent-http", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.Credentials = staticCredentialVerifier{"token": {ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	var callbacks atomic.Int32
	server.OnCreate = func(context.Context, string) { callbacks.Add(1) }
	handler := server.Handler()
	responses := make([]*httptest.ResponseRecorder, 4)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/v1/tasks/concurrent-http/restart", strings.NewReader(`{"request_id":"same","reason":"fresh"}`))
			r.Header.Set("Authorization", "Bearer token")
			responses[i] = httptest.NewRecorder()
			handler.ServeHTTP(responses[i], r)
		}()
	}
	wg.Wait()
	var successor string
	for _, response := range responses {
		if response.Code != http.StatusCreated && response.Code != http.StatusOK {
			t.Fatalf("status=%d: %s", response.Code, response.Body.String())
		}
		var result core.TaskStartOverResult
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if successor != "" && successor != result.Successor.ID {
			t.Fatal("multiple successors")
		}
		successor = result.Successor.ID
	}
	if callbacks.Load() != 1 {
		t.Fatalf("callbacks=%d", callbacks.Load())
	}
}

func TestTaskStartOverEscapedUnicodeLimit(t *testing.T) {
	handler := restartHTTPFixture(t, "unicode-http")
	body := `{"request_id":"unicode","reason":"fresh","note":"` + strings.Repeat(`\uD83D\uDE00`, 2000) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/tasks/unicode-http/restart", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
}

type startOverErrorStore struct {
	store.Store
	err error
}

func (s startOverErrorStore) StartOverTaskCommand(ctx context.Context, lease taskops.TaskLease, r core.TaskStartOverRequest) (core.TaskStartOverResult, error) {
	if s.err != nil {
		return core.TaskStartOverResult{}, s.err
	}
	return s.Store.StartOverTaskCommand(ctx, lease, r)
}

func TestTaskStartOverErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
		retry  bool
	}{
		{store.ErrTaskTerminal, http.StatusConflict, "task_terminal", false},
		{store.ErrStartOverRequestConflict, http.StatusConflict, "start_over_request_conflict", false},
		{store.ErrStartOverConfirmDocuments, http.StatusForbidden, "start_over_confirm_documents", false},
		{store.NewTaskDependencyError("dep", "is not open"), http.StatusConflict, "task_dependency", false},
		{&core.ErrInvalidTransition{Space: core.TaskLifecycle, From: string(core.TaskRunning), Command: string(core.TaskStartOver)}, http.StatusConflict, "invalid_transition", false},
		{&store.RequirementArchivedError{RequirementID: "req-1"}, http.StatusConflict, "requirement_archived", false},
		{&store.SystemDesignArchivedError{DocumentID: "sd-1"}, http.StatusConflict, "system_design_archived", false},
		{&store.TaskContextReferenceError{Kind: "requirement", ID: "req-1", Reason: store.ContextReasonNotFound}, http.StatusConflict, "invalid_context_reference", false},
		{store.ErrTaskBranchConflict, http.StatusConflict, "task_branch_conflict", false},
		{store.ErrTaskAlreadyStartedOver, http.StatusConflict, "task_already_started_over", false},
		{store.ErrRetryable, http.StatusServiceUnavailable, "retryable", true},
		{store.ErrNotFound, http.StatusNotFound, "not_found", false},
		{errors.New("untyped boom"), http.StatusInternalServerError, "internal_error", false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			st := store.NewMemory()
			if err := st.CreateTask(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), core.Task{ID: "map-" + tc.code, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning}); err != nil {
				t.Fatal(err)
			}
			server := NewServer(startOverErrorStore{Store: st, err: tc.err})
			server.Workspace = "demo"
			server.Credentials = staticCredentialVerifier{"token": {ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
			r := httptest.NewRequest(http.MethodPost, "/v1/tasks/map-"+tc.code+"/restart", strings.NewReader(`{"request_id":"r","reason":"why"}`))
			r.Header.Set("Authorization", "Bearer token")
			r.Header.Set("X-Request-Id", "req-map-"+tc.code)
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			got := assertRestartError(t, w, tc.status, tc.code)
			if tc.retry && w.Header().Get("Retry-After") != "1" {
				t.Fatalf("Retry-After=%q", w.Header().Get("Retry-After"))
			}
			if tc.code == "internal_error" && (got["message"] != "internal server error" || w.Header().Get("X-Request-Id") != "req-map-"+tc.code) {
				t.Fatalf("internal message=%q request-id=%q", got["message"], w.Header().Get("X-Request-Id"))
			}
			if tc.code == "task_already_started_over" && got["message"] != "task already has a successor; read it with GET /v1/tasks/{id}" {
				t.Fatalf("already started over message=%q", got["message"])
			}
		})
	}
}

func TestTaskStartOverDroppedContextHTTP(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	old := core.Task{ID: "drop-http", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, Branch: "conveyor/task-drop-http"}
	if err := st.CreateTask(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: old.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "missing-restart-requirement"})}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.Credentials = staticCredentialVerifier{"token": {ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	r := httptest.NewRequest(http.MethodPost, "/v1/tasks/drop-http/restart", strings.NewReader(`{"request_id":"drop","reason":"retry"}`))
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	var got core.TaskStartOverResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.DroppedContext) != 1 || got.DroppedContext[0].Kind != "requirement" || got.DroppedContext[0].ID != "missing-restart-requirement" || got.DroppedContext[0].Reason != "missing" {
		t.Fatalf("dropped=%+v", got.DroppedContext)
	}
}

func assertRestartError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]string {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want %d body=%s", w.Code, status, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content-type=%q", w.Header().Get("Content-Type"))
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v body=%s", err, w.Body.String())
	}
	if body["error"] != code || strings.TrimSpace(body["message"]) == "" {
		t.Fatalf("envelope=%v", body)
	}
	return body
}
