package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// allowedChoices maps each projected choice ID to whether the caller may take it.
func allowedChoices(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	var action nextaction.NextAction
	if err := json.Unmarshal(raw, &action); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	got := map[string]bool{}
	for _, choice := range action.Choices {
		got[choice.ID] = choice.Allowed
	}
	return got
}

// TestNextActionChoicesFollowWorkspaceRole pins that, with a membership store
// configured, a caller's next-action choices are allowed exactly when their
// workspace role admits the choice's route, on REST and MCP alike (NA-3/NA-4,
// DEC-61).
func TestNextActionChoicesFollowWorkspaceRole(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	f.gateTask("gov-task-role-plan", time.Now().UTC())
	if err := f.store.CreateTask(f.ctx, core.Task{ID: "gov-task-role-merge", Workspace: governanceWorkspace, Repo: "conveyor", Branch: "gov-task-role-merge", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// A pending task-context proposal withholds the claim; its confirm and
	// dismiss choices need confirm_documents, which only the operator holds.
	requirement, version, err := f.store.CreateRequirement(f.ctx, core.Requirement{ID: "req-role-proposal", Title: "Role proposal"}, core.RequirementVersion{Content: "# Role proposal", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Gate confirmation on role."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.ConfirmRequirementVersion(f.ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateTask(f.ctx, core.Task{ID: "gov-task-role-proposal", Workspace: governanceWorkspace, Repo: "conveyor", Branch: "gov-task-role-proposal", State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, suppressed, err := f.store.ProposeTaskContext(f.ctx, core.TaskContextProposalInput{TaskID: "gov-task-role-proposal", TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID, Source: core.TaskContextProposalTriage, Justification: "The task implements REQ-1."}); err != nil || suppressed {
		t.Fatalf("proposal suppressed=%t err=%v", suppressed, err)
	}
	cases := []struct {
		role core.WorkspaceRole
		plan map[string]bool
		// merge approval mixes operate_gates (approve) with request_changes.
		merge    map[string]bool
		proposal map[string]bool
	}{
		{core.WorkspaceRoleOperator, map[string]bool{"approve": true, "redirect": true, "reject": true}, map[string]bool{"approve": true, "request_changes": true}, map[string]bool{"confirm": true, "dismiss": true}},
		{core.WorkspaceRoleMaintainer, map[string]bool{"approve": true, "redirect": true, "reject": true}, map[string]bool{"approve": true, "request_changes": true}, map[string]bool{"confirm": false, "dismiss": false}},
		{core.WorkspaceRoleContributor, map[string]bool{"approve": false, "redirect": false, "reject": false}, map[string]bool{"approve": false, "request_changes": true}, map[string]bool{"confirm": false, "dismiss": false}},
		{core.WorkspaceRoleViewer, map[string]bool{"approve": false, "redirect": false, "reject": false}, map[string]bool{"approve": false, "request_changes": false}, map[string]bool{"confirm": false, "dismiss": false}},
	}
	var inboxes []string
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			token, _ := f.user("usr_role_"+string(tc.role), tc.role)
			for task, want := range map[string]map[string]bool{"gov-task-role-plan": tc.plan, "gov-task-role-merge": tc.merge, "gov-task-role-proposal": tc.proposal} {
				rest := allowedChoices(t, f.rest(token, "/v1/tasks/"+task+"/next-action?workspace_id="+governanceWorkspace))
				if !reflect.DeepEqual(rest, want) {
					t.Errorf("REST %s choices=%v want %v", task, rest, want)
				}
				text, isErr := f.call(token, "get_task_status", map[string]any{"workspace_id": governanceWorkspace, "task_id": task})
				if isErr {
					t.Fatalf("MCP %s refused: %s", task, text)
				}
				if mcp := allowedChoices(t, []byte(text)); !reflect.DeepEqual(mcp, want) {
					t.Errorf("MCP %s choices=%v want %v", task, mcp, want)
				}
			}
			// The inbox is admitted for every role and lists the same work: the
			// role only decides which choices are allowed, never what is shown.
			text, isErr := f.call(token, "list_operator_inbox", map[string]any{"workspace_id": governanceWorkspace})
			if isErr {
				t.Fatalf("MCP inbox refused: %s", text)
			}
			if !strings.Contains(text, "gov-task-role-plan") || !strings.Contains(text, "gov-task-role-merge") {
				t.Fatalf("MCP inbox misses the gate decisions: %s", text)
			}
			var page struct {
				Items json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal([]byte(text), &page); err != nil {
				t.Fatalf("decode inbox %s: %v", text, err)
			}
			inboxes = append(inboxes, string(page.Items))
		})
	}
	if len(inboxes) != len(cases) {
		t.Fatalf("read %d inboxes, want %d", len(inboxes), len(cases))
	}
	for _, inbox := range inboxes[1:] {
		if inbox != inboxes[0] {
			t.Fatalf("inbox differs by role:\n%s\n%s", inboxes[0], inbox)
		}
	}
}

// TestNextActionChoicesWithoutMembershipsFollowCredentialScope pins the
// singleton/dev fallback: with no membership store an operator-scoped user
// credential holds the choices, while a user-scoped one and an agent
// credential (even one carrying operator scope) hold none.
func TestNextActionChoicesWithoutMembershipsFollowCredentialScope(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	f.gateTask("gov-task-scope-plan", time.Now().UTC())
	f.server.Workspaces, f.server.Memberships = nil, nil
	handler := f.server.Handler()
	f.credentials["pat-scope-operator"] = core.AuthenticatedCredential{ID: "pat_scope_operator", OwnerUserID: "usr_scope_operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	f.credentials["pat-scope-user"] = core.AuthenticatedCredential{ID: "pat_scope_user", OwnerUserID: "usr_scope_user", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	for token, allowed := range map[string]bool{"pat-scope-operator": true, "pat-scope-user": false} {
		request := httptest.NewRequest(http.MethodGet, "/v1/tasks/gov-task-scope-plan/next-action", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", token, response.Code, response.Body.String())
		}
		want := map[string]bool{"approve": allowed, "redirect": allowed, "reject": allowed}
		if got := allowedChoices(t, response.Body.Bytes()); !reflect.DeepEqual(got, want) {
			t.Errorf("%s choices=%v want %v", token, got, want)
		}
	}
	agent := store.WithCredential(f.ctx, core.AuthenticatedCredential{ID: "agent_scope", OwnerUserID: "usr_scope_operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeOperator})
	opts, err := f.server.nextActionOptionsFor(agent)
	if err != nil {
		t.Fatal(err)
	}
	action, err := store.ReadNextAction(agent, f.store, "gov-task-scope-plan", opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, choice := range action.Choices {
		if choice.Allowed {
			t.Errorf("agent credential holds choice %q", choice.ID)
		}
	}
	if len(action.Choices) == 0 {
		t.Fatal("plan gate projected no choices to deny")
	}
}

// TestNextActionMembershipFailureIsNotAnEmptyChoiceSet pins that a membership
// lookup failure surfaces as an error on REST and MCP rather than a next
// action that silently offers no choices.
func TestNextActionMembershipFailureIsNotAnEmptyChoiceSet(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	f.gateTask("gov-task-fail-plan", time.Now().UTC())
	token, _ := f.user("usr_fail", core.WorkspaceRoleOperator)
	f.members.mu.Lock()
	f.members.inner.authorizeErrs = map[core.Capability]error{core.CapabilityConfirmDocuments: errors.New("membership store down")}
	f.members.mu.Unlock()
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/gov-task-fail-plan/next-action?workspace_id="+governanceWorkspace, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("REST status=%d body=%s, want 500", response.Code, response.Body.String())
	}
	text, isErr := f.call(token, "get_task_status", map[string]any{"workspace_id": governanceWorkspace, "task_id": "gov-task-fail-plan"})
	if !isErr || !strings.Contains(text, "membership store down") {
		t.Fatalf("MCP isError=%v text=%q, want the membership failure", isErr, text)
	}
	text, isErr = f.call(token, "list_operator_inbox", map[string]any{"workspace_id": governanceWorkspace})
	if !isErr || !strings.Contains(text, "membership store down") {
		t.Fatalf("MCP inbox isError=%v text=%q, want the membership failure", isErr, text)
	}
}
