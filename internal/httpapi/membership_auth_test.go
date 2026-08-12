package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type membershipFixture struct {
	workspaces      []core.Workspace
	roles           map[string]map[string]core.WorkspaceRole
	capabilityCalls []core.Capability
	revokeErr       error
}

func (f *membershipFixture) ListWorkspaces(context.Context) ([]core.Workspace, error) {
	return f.workspaces, nil
}
func (f *membershipFixture) GetWorkspace(_ context.Context, id string) (core.Workspace, error) {
	for _, item := range f.workspaces {
		if item.ID == id {
			return item, nil
		}
	}
	return core.Workspace{}, store.ErrNotFound
}
func (f *membershipFixture) CreateWorkspace(context.Context, string, string, *config.Config) (core.Workspace, error) {
	return core.Workspace{}, errors.New("not implemented")
}
func (f *membershipFixture) AuthorizeWorkspace(_ context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	f.capabilityCalls = append(f.capabilityCalls, capability)
	return core.RoleAllows(f.roles[userID][workspaceID], capability), nil
}
func (f *membershipFixture) ListWorkspacesForUser(_ context.Context, userID string) ([]core.Workspace, error) {
	var result []core.Workspace
	for _, item := range f.workspaces {
		if _, ok := f.roles[userID][item.ID]; ok {
			result = append(result, item)
		}
	}
	return result, nil
}
func (f *membershipFixture) ListWorkspaceMembers(context.Context, string, string) ([]core.WorkspaceMembership, error) {
	return nil, nil
}
func (f *membershipFixture) GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error) {
	return core.MembershipGrant{}, nil
}
func (f *membershipFixture) RevokeWorkspaceRole(context.Context, string, string) error {
	return f.revokeErr
}

func TestMembershipScopesWorkspaceListAndNotFoundSurfaces(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha", Name: "Alpha"}, {ID: "beta", Name: "Beta"}},
		roles:      map[string]map[string]core.WorkspaceRole{"usr_1": {"alpha": core.WorkspaceRoleUser}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"member-token": {ID: "pat_1", OwnerUserID: "usr_1", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	handler := server.Handler()

	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer member-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	listed := call("/v1/workspaces")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"alpha"`) || strings.Contains(listed.Body.String(), `"id":"beta"`) {
		t.Fatalf("workspace list status=%d body=%s", listed.Code, listed.Body.String())
	}

	for _, route := range []string{"/v1/tasks", "/v1/activity", "/v1/work-orders", "/v1/monitor", "/v1/pending-proposals", "/v1/tasks/task-id/events/stream"} {
		unbound := call(route + "?workspace_id=beta")
		missing := call(route + "?workspace_id=does-not-exist")
		if unbound.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || unbound.Body.String() != missing.Body.String() {
			t.Fatalf("%s unbound=%d/%q missing=%d/%q", route, unbound.Code, unbound.Body.String(), missing.Code, missing.Body.String())
		}
	}
}

func TestMCPUnboundWorkspaceMatchesMissingWorkspace(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}, {ID: "beta"}},
		roles:      map[string]map[string]core.WorkspaceRole{"usr_1": {"alpha": core.WorkspaceRoleUser}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "agt_1", OwnerUserID: "usr_1", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}))
	_, unbound := server.callMCPTool(request, "list_work_orders", map[string]any{"workspace_id": "beta"})
	_, missing := server.callMCPTool(request, "list_work_orders", map[string]any{"workspace_id": "does-not-exist"})
	if unbound == nil || missing == nil || unbound.Error() != missing.Error() {
		t.Fatalf("unbound=%v missing=%v", unbound, missing)
	}
}

func TestMutationRoutesNameCapabilitiesExplicitly(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"operator": {"alpha": core.WorkspaceRoleOperator},
		},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}

	request := httptest.NewRequest(http.MethodPut, "/v1/tasks/missing/assignee?workspace_id=alpha", strings.NewReader(`{"assignee_user_id":""}`))
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if len(fixture.capabilityCalls) < 2 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != core.CapabilitySetAssignee {
		t.Fatalf("assignee capability calls=%v", fixture.capabilityCalls)
	}
	if got := mcpCapability("set_assignee"); got != core.CapabilitySetAssignee {
		t.Fatalf("MCP assignee capability=%q", got)
	}

	fixture.capabilityCalls = nil
	request = httptest.NewRequest(http.MethodPost, "/v1/reference-documents/missing/versions?workspace_id=alpha", strings.NewReader("invalid multipart"))
	request.Header.Set("Authorization", "Bearer operator-token")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if len(fixture.capabilityCalls) < 2 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != core.CapabilityOperateGates {
		t.Fatalf("reference supersession capability calls=%v", fixture.capabilityCalls)
	}
}

func TestTaskRunRoutesAlsoRequireClaimWork(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"user": {"alpha": core.WorkspaceRoleUser}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"user-token": {ID: "pat_user", OwnerUserID: "user", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
	handler := server.Handler()
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/tasks/task/run-order"},
		{http.MethodPost, "/v1/tasks/task/run-orders/order/claim"},
		{http.MethodPost, "/v1/tasks/task/run-orders/order/renew"},
		{http.MethodGet, "/v1/tasks/task/run-orders/order/reconcile"},
		{http.MethodPost, "/v1/tasks/task/run-orders/order/attempt-checkpoint"},
		{http.MethodPost, "/v1/tasks/task/run-orders/order/release"},
	} {
		fixture.capabilityCalls = nil
		request := httptest.NewRequest(route.method, route.path+"?workspace_id=alpha", strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer user-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if len(fixture.capabilityCalls) < 2 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != core.CapabilityClaimWork {
			t.Fatalf("%s %s capability calls=%v", route.method, route.path, fixture.capabilityCalls)
		}
	}
}

func TestReferenceDocumentSupersessionRequiresOperator(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "alpha")
	document, _, err := st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: "ref-overview", Name: "Overview"}, core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# One"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"user":     {"alpha": core.WorkspaceRoleUser},
			"operator": {"alpha": core.WorkspaceRoleOperator},
		},
	}
	server := NewServer(st)
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{
		"user-token":     {ID: "pat_user", OwnerUserID: "user", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	call := func(token string) *httptest.ResponseRecorder {
		request := referenceDocumentUploadRequest(t, "/v1/reference-documents/"+document.ID+"/versions", "", "overview.md", "text/markdown", []byte("# Two"))
		request.URL.RawQuery = "workspace_id=alpha"
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := call("user-token"); response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
		t.Fatalf("user supersession status=%d body=%q", response.Code, response.Body.String())
	}
	if response := call("operator-token"); response.Code != http.StatusCreated {
		t.Fatalf("operator supersession status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestDurableWorkspaceAuthorizationWithoutMembershipsFailsClosed(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.Workspaces = &fakeWorkspaceControl{items: []core.Workspace{{ID: "alpha"}, {ID: "beta"}}}
	server.BearerToken = "operator-token"
	handler := server.Handler()
	for _, path := range []string{"/v1/workspaces", "/v1/tasks?workspace_id=alpha"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer operator-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
			t.Fatalf("%s status=%d body=%q", path, response.Code, response.Body.String())
		}
	}
	ctx := store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "pat", OwnerUserID: "operator", Kind: core.CredentialUser})
	if _, err := server.resolveMCPWorkspace(ctx, "alpha"); err == nil || err.Error() != "workspace_not_found: workspace not found" {
		t.Fatalf("MCP fail-closed error=%v", err)
	}
}

func TestWorkerMCPWorkspaceIsBoundToEnrollment(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.Workspaces = &fakeWorkspaceControl{items: []core.Workspace{{ID: "alpha"}, {ID: "beta"}}}
	ctx := context.WithValue(t.Context(), workerContextKey{}, core.Worker{ID: "worker-alpha", Workspace: "alpha"})
	if got, err := server.resolveMCPWorkspace(ctx, ""); err != nil || got != "alpha" {
		t.Fatalf("omitted workspace=%q err=%v", got, err)
	}
	if got, err := server.resolveMCPWorkspace(ctx, "alpha"); err != nil || got != "alpha" {
		t.Fatalf("matching workspace=%q err=%v", got, err)
	}
	if got, err := server.resolveMCPWorkspace(ctx, "beta"); err == nil || got != "" || err.Error() != "workspace_not_found: workspace not found" {
		t.Fatalf("mismatched workspace=%q err=%v", got, err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
	result, err := server.callMCPTool(request, "list_work_orders", map[string]any{"workspace_id": "beta"})
	if err == nil || err.Error() != "workspace_not_found: workspace not found" || result != nil {
		t.Fatalf("mismatched tool call result=%v err=%v", result, err)
	}
}

func TestSoleOperatorRevocationIsActionable(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}},
		revokeErr:  store.ErrLastWorkspaceOperator,
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	request := httptest.NewRequest(http.MethodDelete, "/v1/workspaces/alpha/members/operator", nil)
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "grant another operator first") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func canonicalWorkspaceNotFoundBody() string {
	response := httptest.NewRecorder()
	writeWorkspaceNotFound(response)
	return response.Body.String()
}
