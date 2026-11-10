package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// req-risk-based-review REQ-1, AC-1.1, AC-1.5: REST intake freezes the
// workspace risk policy into the task by value; a later workspace edit
// leaves the filed task's frozen policy unchanged. A task filed with no
// policy freezes none, and a policy named in the intake request is never
// read.
func TestRESTIntakeFreezesRiskPolicy(t *testing.T) {
	document := contextualWorkspaceDocument()
	backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 1}}
	st := store.NewMemory()
	s := NewServer(st)
	s.BearerToken = "token"
	s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
	s.ConfigStore = backend
	s.ConfigProvider = func(context.Context) (*config.Config, error) {
		saved := backend.record.Document
		return &config.Config{Workspace: "demo", Execution: saved.Execution, Repos: saved.Repos, RiskPolicy: saved.RiskPolicy.Clone(), Database: config.Database{Backend: "memory"}}, nil
	}
	s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Risk policy freeze", nil }
	h := s.Handler()
	serve := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		return response
	}
	createTask := func() core.Task {
		t.Helper()
		response := serve(http.MethodPost, "/v1/tasks", `{"body":"risk policy freeze","repo":"conveyor"}`, map[string]string{"Content-Type": "application/json"})
		if response.Code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", response.Code, response.Body)
		}
		var task core.Task
		if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		stored, err := st.GetTask(store.WithWorkspace(t.Context(), "demo"), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	save := func(risk string) {
		t.Helper()
		next := backend.record.Document
		next.RiskPolicy = nil
		data, err := json.Marshal(map[string]any{"document": next})
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(string(data), `"document":{`, `"document":{"risk_policy":`+risk+`,`, 1)
		if risk == "" {
			body = string(data)
		}
		put := serve(http.MethodPut, "/v1/workspace/config", body, map[string]string{"If-Match": jsonInt(backend.record.Version)})
		if put.Code != http.StatusOK {
			t.Fatalf("PUT status=%d body=%s", put.Code, put.Body)
		}
	}

	if plain := createTask(); plain.SetupContract.Risk != nil {
		t.Fatalf("task filed without a policy froze %+v", plain.SetupContract.Risk)
	}
	// Risk is no intake input: a request naming one freezes only the
	// workspace's value, here none.
	supplied := serve(http.MethodPost, "/v1/tasks", `{"body":"x","repo":"conveyor","risk_policy":{"mode":"enforce"},"risk":{"mode":"enforce"}}`, map[string]string{"Content-Type": "application/json"})
	if supplied.Code != http.StatusCreated || strings.Contains(supplied.Body.String(), `"risk"`) {
		t.Fatalf("intake read a supplied risk policy: status=%d body=%s", supplied.Code, supplied.Body)
	}

	save(`{"mode":"enforce"}`)
	filed := createTask()
	want := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}
	if filed.SetupContract.Risk == nil || *filed.SetupContract.Risk != want {
		t.Fatalf("filed task froze %+v, want %+v", filed.SetupContract.Risk, want)
	}
	save(`{"mode":"record","tiers":{"low":{"agent_reviews":2,"human_approvals":2},"medium":{"agent_reviews":2,"human_approvals":2},"high":{"agent_reviews":4,"human_approvals":3}}}`)
	unchanged, err := st.GetTask(store.WithWorkspace(t.Context(), "demo"), filed.ID)
	if err != nil || unchanged.SetupContract.Risk == nil || *unchanged.SetupContract.Risk != want {
		t.Fatalf("workspace edit changed the filed task's risk policy: %+v err=%v", unchanged.SetupContract.Risk, err)
	}
	if later := createTask(); later.SetupContract.Risk == nil || later.SetupContract.Risk.Mode != riskpolicy.ModeRecord || later.SetupContract.Risk.Tiers.High.AgentReviews != 4 {
		t.Fatalf("task filed after the edit froze %+v", later.SetupContract.Risk)
	}
}

func jsonInt(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}
