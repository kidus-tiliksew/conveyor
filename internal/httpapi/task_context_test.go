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
		Content: "# Context intent", Origin: core.RequirementOriginOperator,
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

func TestTaskContextProposalRESTAndTaskProjection(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-rest-proposal", Title: "REST proposal"}, core.RequirementVersion{
		Content: "# REST proposal", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Expose proposal decisions."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "rest-proposal-task", Workspace: "demo", Repo: "api", State: core.TaskRunning}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID,
		Source: core.TaskContextProposalTriage, Justification: "REQ-1 directly governs the requested endpoint."}); err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-rest-proposal", Title: "REST proposal design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# REST proposal\n\n```conveyor:governs\n- repo: api\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: core.TaskContextProposalSystemDesign, TargetID: design.ID,
		Source: core.TaskContextProposalPlanning, Justification: "The design is a candidate governor."}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	request := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	detail := request(http.MethodGet, "/v1/tasks/"+task.ID)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"justification":"REQ-1 directly governs`) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	activity := request(http.MethodGet, "/v1/tasks/"+task.ID+"/activity")
	if activity.Code != http.StatusOK || !strings.Contains(activity.Body.String(), `"target_id":"req-rest-proposal"`) ||
		!strings.Contains(activity.Body.String(), `"target_id":"design-rest-proposal"`) ||
		!strings.Contains(activity.Body.String(), `"justification":"REQ-1 directly governs`) {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body.String())
	}
	pending := request(http.MethodGet, "/v1/pending-proposals")
	if pending.Code != http.StatusOK || strings.Contains(pending.Body.String(), `"tier":"task_context"`) ||
		!strings.Contains(pending.Body.String(), `"pending_proposal_count":0`) || !strings.Contains(pending.Body.String(), `"task_count":1`) {
		t.Fatalf("pending status=%d body=%s", pending.Code, pending.Body.String())
	}
	unauthorizedRequest := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/context/proposals/requirement/"+requirement.ID+"/confirm", nil)
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	confirmed := request(http.MethodPost, "/v1/tasks/"+task.ID+"/context/proposals/requirement/"+requirement.ID+"/confirm")
	if confirmed.Code != http.StatusOK || !strings.Contains(confirmed.Body.String(), `"state":"confirmed"`) {
		t.Fatalf("confirm status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	dismissed := request(http.MethodPost, "/v1/tasks/"+task.ID+"/context/proposals/system_design/"+design.ID+"/dismiss")
	if dismissed.Code != http.StatusOK || !strings.Contains(dismissed.Body.String(), `"state":"dismissed"`) {
		t.Fatalf("dismiss status=%d body=%s", dismissed.Code, dismissed.Body.String())
	}
	detail = request(http.MethodGet, "/v1/tasks/"+task.ID)
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), `"proposals"`) || !strings.Contains(detail.Body.String(), `"requirements"`) {
		t.Fatalf("resolved detail status=%d body=%s", detail.Code, detail.Body.String())
	}
}

func TestArchivedContextErrorsAcrossRESTAndMCP(t *testing.T) {
	for _, kind := range []core.TaskContextProposalTargetKind{core.TaskContextProposalRequirement, core.TaskContextProposalSystemDesign} {
		t.Run(string(kind), func(t *testing.T) {
			st := store.NewMemory()
			ctx := store.WithWorkspace(t.Context(), "demo")
			id := "archive-context"
			code := "requirement_archived"
			field := "requirement_ids"
			archive := func() {}
			if kind == core.TaskContextProposalRequirement {
				_, v, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Archive"}, core.RequirementVersion{Content: "# Archive", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Reject archives."}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = st.ConfirmRequirementVersion(ctx, id, v.Version); err != nil {
					t.Fatal(err)
				}
				archive = func() {
					if err := st.ArchiveRequirement(ctx, id, "operator", nil); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				code, field = "system_design_archived", "system_design_ids"
				_, v, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Archive", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Archive\n\n```conveyor:governs\n- repo: api\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = st.ConfirmSystemDesignVersion(ctx, id, v.Version); err != nil {
					t.Fatal(err)
				}
				archive = func() {
					if err := st.ArchiveSystemDesign(ctx, id, "operator", nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			task := core.Task{ID: "context-task", Workspace: "demo", Repo: "api", State: core.TaskRunning}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: kind, TargetID: id, Source: core.TaskContextProposalTriage, Justification: "Relevant context."}); err != nil {
				t.Fatal(err)
			}
			session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "archive-session", Goal: core.PlanningGoalBundle})
			if err != nil {
				t.Fatal(err)
			}
			refs := store.TaskContextProposalInput(kind, id)
			decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Keep pending work.", Context: "Bundle fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := st.CreatePlanningBundle(ctx, core.PlanningBundle{ID: "archive-bundle", SessionID: session.ID, Title: "Bundle", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleDecision, ID: decision.ID}}, Tasks: []core.PlanningBundleTask{{MemberID: "one", Repo: "api", Title: "One", Body: "Context", Context: core.PlanningBundleTaskContext{RequirementIDs: refs.RequirementIDs, DesignIDs: refs.DesignIDs}}}})
			if err != nil {
				t.Fatal(err)
			}
			archive()
			server := NewServer(st)
			server.BearerToken, server.Workspace, server.Repos = "token", "demo", []string{"api"}
			server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Task", nil }
			request := func(path, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer token")
				w := httptest.NewRecorder()
				server.Handler().ServeHTTP(w, r)
				return w
			}
			for _, test := range []struct{ path, body string }{
				{"/v1/tasks/" + task.ID + "/context/proposals/" + string(kind) + "/" + id + "/confirm", ""},
				{"/v1/tasks/" + task.ID + "/context", `{"add":{"` + field + `":["` + id + `"]}}`},
				{"/v1/tasks", `{"body":"Context","repo":"api","` + field + `":["` + id + `"]}`},
				{"/v1/planning-bundles/" + bundle.ID + "/approve", ""},
			} {
				w := request(test.path, test.body)
				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"error":"`+code+`"`) {
					t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
				}
			}
			mcp := request("/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_task","arguments":{"body":"Context","repo":"api","idempotency_key":"archive-intake","`+field+`":["`+id+`"]}}}`)
			if !strings.Contains(mcp.Body.String(), `"isError":true`) || !strings.Contains(mcp.Body.String(), code) {
				t.Fatalf("MCP archive response: %d %s", mcp.Code, mcp.Body.String())
			}
			proposals, err := st.ListTaskContextProposals(ctx, task.ID, core.TaskContextProposalProposed)
			if err != nil || len(proposals) != 1 {
				t.Fatalf("proposal changed: %+v %v", proposals, err)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.Kind == "task.context_proposal_confirmed" || e.Kind == store.TaskContextRequirementAdded || e.Kind == store.TaskContextDesignAdded {
					t.Fatalf("partial write: %+v", e)
				}
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("partial intake/bundle: %+v %v", tasks, err)
			}
			dismissed := request("/v1/tasks/"+task.ID+"/context/proposals/"+string(kind)+"/"+id+"/dismiss", "")
			if dismissed.Code != http.StatusOK {
				t.Fatalf("dismissal: %d %s", dismissed.Code, dismissed.Body.String())
			}
		})
	}
}
