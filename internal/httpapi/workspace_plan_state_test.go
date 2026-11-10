package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestCreateWorkspaceResolvesDefaultPlanState proves workspace creation reads
// the legacy spec_approval only as default_plan_state's alias, stores only
// default_plan_state, defaults to needed, and answers 422 validation_failed
// naming the field for a bad value or a disagreeing pair
// (component-runtime invariant 39; req-task-plan-state AC-2.1).
func TestCreateWorkspaceResolvesDefaultPlanState(t *testing.T) {
	for _, test := range []struct {
		name      string
		execution map[string]any
		want      string
	}{
		{"absent keys", map[string]any{"merge_approval": true}, "needed"},
		{"alias false", map[string]any{"spec_approval": false}, "not_required"},
		{"alias true", map[string]any{"spec_approval": true}, "needed"},
		{"stated", map[string]any{"default_plan_state": "not_required"}, "not_required"},
		{"disagreeing pair", map[string]any{"default_plan_state": "needed", "spec_approval": false}, ""},
		{"invalid value", map[string]any{"default_plan_state": "awaiting_approval"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			control := &fakeWorkspaceControl{}
			create := NewServer(store.NewMemory())
			create.Workspaces, create.Deployment, create.BearerToken = control, legacyExecutorDeployment(), "token"
			create.Memberships = localOperatorInstanceAdmin()
			body, err := json.Marshal(map[string]any{"id": "engineering", "name": "Engineering", "document": map[string]any{"execution": test.execution}})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/workspaces", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer token")
			result := httptest.NewRecorder()
			create.Handler().ServeHTTP(result, request)
			if test.want == "" {
				if result.Code != http.StatusUnprocessableEntity || control.created != nil || !strings.Contains(result.Body.String(), `"validation_failed"`) || !strings.Contains(result.Body.String(), `"field":"execution.default_plan_state"`) {
					t.Fatalf("status=%d created=%t body=%s", result.Code, control.created != nil, result.Body)
				}
				return
			}
			if result.Code != http.StatusCreated || control.created == nil {
				t.Fatalf("status=%d body=%s", result.Code, result.Body)
			}
			if control.created.Execution.DefaultPlanState != test.want || control.created.Execution.SpecApproval != nil {
				t.Fatalf("created execution=%+v, want %s and no alias", control.created.Execution, test.want)
			}
			served, err := json.Marshal(control.created.PolicyDocument())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(served), "spec_approval") {
				t.Fatalf("created document serves spec_approval: %s", served)
			}
		})
	}
}

// TestWorkspaceWritesKeepMergeApprovalForEitherPlanKeySpelling proves PUT
// /v1/workspace/config (which config import sends) and workspace creation
// store the merge_approval a document states, false or true, whichever
// plan-state key spelling it carries, and that an absent merge_approval stays
// false, as these routes resolved it before the plan state (DEC-125(1);
// component-task-lifecycle invariant 18).
func TestWorkspaceWritesKeepMergeApprovalForEitherPlanKeySpelling(t *testing.T) {
	for _, plan := range []map[string]any{
		{"default_plan_state": "needed"},
		{"default_plan_state": "not_required"},
		{"spec_approval": true},
		{"spec_approval": false},
	} {
		for _, merge := range []any{false, true, nil} {
			execution := map[string]any{}
			for key, value := range plan {
				execution[key] = value
			}
			if merge != nil {
				execution["merge_approval"] = merge
			}
			want := merge == true
			name, _ := json.Marshal(execution)

			// PUT, the route config import calls.
			document := map[string]any{}
			raw, _ := json.Marshal(contextualWorkspaceDocument())
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			document["execution"] = execution
			backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: contextualWorkspaceDocument(), Version: 1}}
			put := NewServer(store.NewMemory())
			put.BearerToken = "token"
			put.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
			put.ConfigStore = backend
			body, _ := json.Marshal(map[string]any{"document": document})
			request := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer token")
			request.Header.Set("If-Match", "1")
			result := httptest.NewRecorder()
			put.Handler().ServeHTTP(result, request)
			if result.Code != http.StatusOK || backend.record.Document.Execution.MergeApproval != want {
				t.Fatalf("PUT %s: status=%d merge_approval=%t, want %t body=%s", name, result.Code, backend.record.Document.Execution.MergeApproval, want, result.Body)
			}

			// Workspace creation.
			control := &fakeWorkspaceControl{}
			create := NewServer(store.NewMemory())
			create.Workspaces, create.Deployment, create.BearerToken = control, legacyExecutorDeployment(), "token"
			create.Memberships = localOperatorInstanceAdmin()
			body, _ = json.Marshal(map[string]any{"id": "engineering", "name": "Engineering", "document": map[string]any{"execution": execution}})
			request = httptest.NewRequest(http.MethodPost, "/v1/workspaces", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer token")
			result = httptest.NewRecorder()
			create.Handler().ServeHTTP(result, request)
			if result.Code != http.StatusCreated || control.created == nil || control.created.Execution.MergeApproval != want {
				t.Fatalf("create %s: status=%d created=%+v, want merge_approval %t body=%s", name, result.Code, control.created, want, result.Body)
			}
		}
	}
}
