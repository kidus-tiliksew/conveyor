package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTaskIntakeAndOpenTaskContextAction(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	requirement, requirementVersion, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-context", Title: "Context intent"}, core.RequirementVersion{
		Content: "Context intent", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Use task context."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, requirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-context", Title: "Context design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Context design\n\n```conveyor:governs\n- repo: api\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.BearerToken, server.Workspace, server.Repos = "token", "demo", []string{"api"}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Contextual task", nil }
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	created := request(http.MethodPost, "/v1/tasks", `{"body":"context","repo":"api","requirement_ids":["req-context"],"system_design_ids":["design-context"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", created.Code, created.Body.String())
	}
	var task core.Task
	if err = json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if len(task.Context.Requirements) != 1 || len(task.Context.Designs) != 1 {
		t.Fatalf("task context=%+v", task.Context)
	}

	detail := request(http.MethodGet, "/v1/tasks/"+task.ID+"/activity", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"req-context"`) || !strings.Contains(detail.Body.String(), `"design-context"`) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	removed := request(http.MethodPost, "/v1/tasks/"+task.ID+"/context", `{"remove":{"requirement_ids":["req-context"],"system_design_ids":["design-context"]}}`)
	if removed.Code != http.StatusOK || removed.Body.String() != "{}\n" {
		t.Fatalf("remove status=%d body=%s", removed.Code, removed.Body.String())
	}
}

func TestTaskIntakeRejectsUnknownContextWithoutPartialTask(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.BearerToken, server.Workspace, server.Repos = "token", "demo", []string{"api"}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Never persisted", nil }
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"body":"context","repo":"api","requirement_ids":["req-missing"]}`))
	req.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "req-missing") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	tasks, err := st.ListTasks(store.WithWorkspace(t.Context(), "demo"))
	if err != nil || len(tasks) != 0 {
		t.Fatalf("partial tasks=%+v err=%v", tasks, err)
	}
}
