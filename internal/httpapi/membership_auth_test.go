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
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type membershipFixture struct {
	workspaces      []core.Workspace
	roles           map[string]map[string]core.WorkspaceRole
	capabilityCalls []core.Capability
	grantErr        error
	revokeErr       error
	invitationErr   error
	authorizeErrs   map[core.Capability]error
	invitations     []core.WorkspaceInvitation
	invitationList  []string
	invitationsErr  error
}

type identityProvisioningFixture struct {
	store.Store
	calls        int
	provisionErr error
}

func (f *identityProvisioningFixture) ProvisionIdentityUser(_ context.Context, email, displayName string) (core.IdentityUser, error) {
	f.calls++
	if f.provisionErr != nil {
		return core.IdentityUser{}, f.provisionErr
	}
	return core.IdentityUser{ID: "usr_provisioned", Email: email, DisplayName: displayName, Status: "active"}, nil
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
	if err := f.authorizeErrs[capability]; err != nil {
		return false, err
	}
	return core.RoleAllows(f.roles[userID][workspaceID], capability), nil
}
func (f *membershipFixture) AuthorizeDeployment(_ context.Context, userID string, capability core.Capability) (bool, error) {
	f.capabilityCalls = append(f.capabilityCalls, capability)
	if err := f.authorizeErrs[capability]; err != nil {
		return false, err
	}
	for _, role := range f.roles[userID] {
		if role == core.WorkspaceRoleOperator {
			return true, nil
		}
	}
	return false, nil
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
func (f *membershipFixture) ListWorkspaceInvitations(_ context.Context, workspaceID string) ([]core.WorkspaceInvitation, error) {
	f.invitationList = append(f.invitationList, workspaceID)
	return f.invitations, f.invitationsErr
}
func (f *membershipFixture) GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error) {
	return core.MembershipGrant{}, f.grantErr
}
func (f *membershipFixture) RevokeWorkspaceInvitation(context.Context, string, string) error {
	return f.invitationErr
}
func (f *membershipFixture) RevokeWorkspaceRole(context.Context, string, string) error {
	return f.revokeErr
}

func TestProvisionIdentityUserRequiresHumanDeploymentOperator(t *testing.T) {
	fixture := &identityProvisioningFixture{Store: store.NewMemory()}
	server := NewServer(fixture)
	server.Credentials = staticCredentialVerifier{
		"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"agent-token":    {ID: "agt_operator", OwnerUserID: "operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
		"user-token":     {ID: "pat_user", OwnerUserID: "user", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	call := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(`{"email":"invited@example.test","display_name":"Invited User"}`))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	for _, token := range []string{"", "agent-token", "user-token"} {
		if response := call(token); response.Code != http.StatusUnauthorized {
			t.Fatalf("token %q status=%d body=%s", token, response.Code, response.Body.String())
		}
	}
	response := call("operator-token")
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"id":"usr_provisioned"`) || fixture.calls != 1 {
		t.Fatalf("operator status=%d body=%s calls=%d", response.Code, response.Body.String(), fixture.calls)
	}
}

func TestProvisionIdentityUserClassifiesValidationAndStoreFailures(t *testing.T) {
	fixture := &identityProvisioningFixture{Store: store.NewMemory()}
	server := NewServer(fixture)
	server.Credentials = staticCredentialVerifier{
		"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	call := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer operator-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	for _, test := range []struct {
		body  string
		field string
	}{
		{body: `{`, field: "user"},
		{body: `{"email":"not-an-email","display_name":"User"}`, field: "email"},
		{body: `{"email":"user@example.test","display_name":" "}`, field: "display_name"},
	} {
		response := call(test.body)
		if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"field":"`+test.field+`"`) {
			t.Fatalf("body=%q status=%d response=%q", test.body, response.Code, response.Body.String())
		}
	}
	if fixture.calls != 0 {
		t.Fatalf("invalid requests reached provisioner calls=%d", fixture.calls)
	}

	fixture.provisionErr = errors.New(provisionedAccountDeactivatedError)
	response := call(`{"email":"user@example.test","display_name":"User"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error":"account_deactivated"`) || !strings.Contains(response.Body.String(), "reactivate") {
		t.Fatalf("deactivated status=%d body=%q", response.Code, response.Body.String())
	}

	rawError := "database connection failed with secret detail"
	fixture.provisionErr = errors.New(rawError)
	response = call(`{"email":"user@example.test","display_name":"User"}`)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), rawError) || response.Body.String() != "internal server error\n" {
		t.Fatalf("infrastructure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestRevokeWorkspaceInvitationClassifiesNotFoundAndStoreFailures(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{
		"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodDelete, "/v1/workspaces/alpha/invitations/missing@example.test", nil)
		request.Header.Set("Authorization", "Bearer operator-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	fixture.invitationErr = store.ErrNotFound
	response := call()
	if response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
		t.Fatalf("not-found status=%d body=%q", response.Code, response.Body.String())
	}

	rawError := "delete failed with secret detail"
	fixture.invitationErr = errors.New(rawError)
	response = call()
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), rawError) || response.Body.String() != "internal server error\n" {
		t.Fatalf("infrastructure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestAuthorizationStoreFailuresDoNotLeakErrorText(t *testing.T) {
	rawError := "authorize failed with secret detail"
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{
		"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	call := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer operator-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	assertGeneric := func(site string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), rawError) || response.Body.String() != "internal server error\n" {
			t.Fatalf("%s status=%d body=%q", site, response.Code, response.Body.String())
		}
	}

	// Deployment-scoped mutation: requireMutationCapability's AuthorizeDeployment branch.
	fixture.authorizeErrs = map[core.Capability]error{core.CapabilityManageWorkspace: errors.New(rawError)}
	assertGeneric("deployment mutation", call(http.MethodPost, "/v1/users"))

	// Workspace-scoped mutation: requireMutationCapability's AuthorizeWorkspace branch.
	assertGeneric("workspace mutation", call(http.MethodPost, "/v1/lineage/rebuild"))

	// Workspace read gate: requireWorkspaceCapability.
	fixture.authorizeErrs = map[core.Capability]error{core.CapabilityViewWorkspace: errors.New(rawError)}
	assertGeneric("workspace capability", call(http.MethodGet, "/v1/activity"))
}

func TestMembershipScopesWorkspaceListAndNotFoundSurfaces(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha", Name: "Alpha"}, {ID: "beta", Name: "Beta"}},
		roles:      map[string]map[string]core.WorkspaceRole{"usr_1": {"alpha": core.WorkspaceRoleContributor}},
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
		roles:      map[string]map[string]core.WorkspaceRole{"usr_1": {"alpha": core.WorkspaceRoleContributor}},
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
	if len(fixture.capabilityCalls) < 2 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != core.CapabilityConfirmDocuments {
		t.Fatalf("reference supersession capability calls=%v", fixture.capabilityCalls)
	}
}

func TestTaskRunRoutesAlsoRequireClaimWork(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"user": {"alpha": core.WorkspaceRoleContributor}},
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

func TestTaskRunRoutesRejectDashboardSessionsAndAcceptBearerPATs(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"user": {"alpha": core.WorkspaceRoleExecutor}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"user-token": {ID: "pat_user", OwnerUserID: "user", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
	server.InvitationSessions = &invitationSessionFixture{credential: core.AuthenticatedCredential{
		ID: "session_user", OwnerUserID: "user", Kind: core.CredentialUser,
		Scope: core.CredentialScopeUser, Method: core.CredentialMethodSession,
	}}
	handler := server.Handler()
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/tasks/task/run-order"},
		{http.MethodPost, "/v1/tasks/task/run-orders/order/claim"},
		{http.MethodPost, "/v1/tasks/task/run-orders/order/renew"},
		{http.MethodGet, "/v1/tasks/task/run-orders/order/reconcile"},
		{http.MethodPost, "/v1/tasks/task/run-orders/order/attempt-checkpoint"},
		{http.MethodPost, "/v1/tasks/task/run-orders/order/release"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			sessionRequest := httptest.NewRequest(route.method, route.path+"?workspace_id=alpha", strings.NewReader(`{}`))
			sessionRequest.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-secret"})
			sessionRequest.Header.Set("X-Conveyor-CSRF", "1")
			sessionRequest.Header.Set("Origin", "http://example.com")
			sessionResponse := httptest.NewRecorder()
			handler.ServeHTTP(sessionResponse, sessionRequest)
			if sessionResponse.Code != http.StatusUnauthorized || sessionResponse.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("session status=%d headers=%v body=%q", sessionResponse.Code, sessionResponse.Header(), sessionResponse.Body.String())
			}

			patRequest := httptest.NewRequest(route.method, route.path+"?workspace_id=alpha", strings.NewReader(`{}`))
			patRequest.Header.Set("Authorization", "Bearer user-token")
			patResponse := httptest.NewRecorder()
			handler.ServeHTTP(patResponse, patRequest)
			if patResponse.Code == http.StatusUnauthorized {
				t.Fatalf("PAT was rejected by %s %s: body=%q", route.method, route.path, patResponse.Body.String())
			}
		})
	}
}

func TestViewerReadsWorkspaceAndAllMutationsUseCapabilityRefusal(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"viewer": {"alpha": core.WorkspaceRoleViewer}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"viewer-token": {ID: "pat_viewer", OwnerUserID: "viewer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
	handler := server.Handler()
	call := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		fixture.capabilityCalls = nil
		request := httptest.NewRequest(method, path+"?workspace_id=alpha", strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer viewer-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	for _, path := range []string{"/v1/activity", "/v1/tasks", "/v1/work-orders", "/v1/monitor", "/v1/pending-proposals", "/v1/workspace/config", "/v1/artifacts", "/v1/workers", "/v1/workspaces/alpha/members"} {
		response := call(http.MethodGet, path)
		if response.Code == http.StatusNotFound && response.Body.String() == canonicalWorkspaceNotFoundBody() {
			t.Fatalf("viewer read %s was capability-refused", path)
		}
		for _, capability := range fixture.capabilityCalls {
			if capability != core.CapabilityViewWorkspace {
				t.Fatalf("viewer read %s requested capability %q (calls=%v)", path, capability, fixture.capabilityCalls)
			}
		}
	}

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/v1/tasks"},
		{http.MethodPut, "/v1/tasks/task/assignee"},
		{http.MethodPost, "/v1/tasks/task/request-changes"},
		{http.MethodPost, "/v1/requirements"},
		{http.MethodPost, "/v1/system-designs"},
		{http.MethodPost, "/v1/decisions"},
		{http.MethodPost, "/v1/work-orders/order/recover"},
		{http.MethodPost, "/v1/workers/pairings"},
	} {
		response := call(route.method, route.path)
		if response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
			t.Fatalf("viewer mutation %s %s status=%d body=%q", route.method, route.path, response.Code, response.Body.String())
		}
	}
}

func TestViewerMCPToolsRefuseNamedCapabilities(t *testing.T) {
	st := store.NewMemory()
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"viewer": {"alpha": core.WorkspaceRoleViewer}},
	}
	server := NewServer(st)
	server.Workspaces, server.Memberships = fixture, fixture
	server.WorkOrders = &workorder.Service{Store: st}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "pat_viewer", OwnerUserID: "viewer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
	fixture.capabilityCalls = nil
	if _, err := server.callMCPTool(request, "list_work_orders", map[string]any{"workspace_id": "alpha"}); err != nil {
		t.Fatalf("viewer list_work_orders: %v", err)
	}
	if len(fixture.capabilityCalls) == 0 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != core.CapabilityViewWorkspace {
		t.Fatalf("viewer list_work_orders calls=%v", fixture.capabilityCalls)
	}
	for _, tool := range []string{"claim_work_order", "set_assignee", "create_task"} {
		fixture.capabilityCalls = nil
		_, err := server.callMCPTool(request, tool, map[string]any{"workspace_id": "alpha"})
		if err == nil {
			t.Fatalf("viewer MCP tool %s was allowed", tool)
		}
		want := mcpCapability(tool)
		if len(fixture.capabilityCalls) == 0 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != want {
			t.Fatalf("viewer MCP tool %s calls=%v want %q", tool, fixture.capabilityCalls, want)
		}
	}
	for _, tool := range []string{"request_plan_revision", "propose_system_design_revision", "propose_decision"} {
		fixture.capabilityCalls = nil
		_, err := server.callMCPTool(request, tool, map[string]any{"workspace_id": "alpha", "work_order_id": "missing", "session_id": "missing"})
		if !errors.Is(err, store.ErrWorkOrderClaimLost) {
			t.Fatalf("viewer MCP governance tool %s error=%v, want claim loss", tool, err)
		}
		if len(fixture.capabilityCalls) == 0 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != core.CapabilityViewWorkspace {
			t.Fatalf("viewer MCP governance tool %s calls=%v", tool, fixture.capabilityCalls)
		}
	}
}

func TestMCPGovernanceToolsRemainClaimGatedForEveryRole(t *testing.T) {
	roles := []core.WorkspaceRole{
		core.WorkspaceRoleViewer,
		core.WorkspaceRoleExecutor,
		core.WorkspaceRoleContributor,
		core.WorkspaceRoleMaintainer,
		core.WorkspaceRoleOperator,
	}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			st := store.NewMemory()
			fixture := &membershipFixture{
				workspaces: []core.Workspace{{ID: "alpha"}},
				roles:      map[string]map[string]core.WorkspaceRole{"user": {"alpha": role}},
			}
			server := NewServer(st)
			server.Workspaces, server.Memberships = fixture, fixture
			server.WorkOrders = &workorder.Service{Store: st}
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "pat", OwnerUserID: "user", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
			for _, tool := range []string{"request_plan_revision", "propose_system_design_revision", "propose_decision"} {
				fixture.capabilityCalls = nil
				_, err := server.callMCPTool(request, tool, map[string]any{"workspace_id": "alpha", "work_order_id": "missing", "session_id": "missing"})
				if !errors.Is(err, store.ErrWorkOrderClaimLost) {
					t.Fatalf("%s error=%v, want claim loss", tool, err)
				}
				if len(fixture.capabilityCalls) != 1 || fixture.capabilityCalls[0] != core.CapabilityViewWorkspace {
					t.Fatalf("%s capability calls=%v", tool, fixture.capabilityCalls)
				}
			}
		})
	}
}

func TestExecutorAndMaintainerRouteBoundaries(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"executor":   {"alpha": core.WorkspaceRoleExecutor},
			"maintainer": {"alpha": core.WorkspaceRoleMaintainer},
			"operator":   {"alpha": core.WorkspaceRoleOperator},
		},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{
		"executor-token":   {ID: "pat_executor", OwnerUserID: "executor", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"maintainer-token": {ID: "pat_maintainer", OwnerUserID: "maintainer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"operator-token":   {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	if response := call(http.MethodPost, "/v1/requirements?workspace_id=alpha", "executor-token", `{}`); response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
		t.Fatalf("executor freestanding proposal status=%d body=%q", response.Code, response.Body.String())
	}
	for _, route := range []struct {
		method, path, body string
		capability         core.Capability
	}{
		{http.MethodPost, "/v1/requirements/req/versions/1/confirm?workspace_id=alpha", "", core.CapabilityConfirmDocuments},
		{http.MethodGet, "/v1/workspaces/alpha/invitations?workspace_id=alpha", "", core.CapabilityManageMembership},
		{http.MethodPost, "/v1/lineage/rebuild?workspace_id=alpha", `{}`, core.CapabilityManageWorkspace},
		{http.MethodPost, "/v1/requirements/req/staleness/signal/acknowledge?workspace_id=alpha", `{}`, core.CapabilityManageWorkspace},
		{http.MethodPost, "/v1/requirements/req/staleness/signal/follow-up?workspace_id=alpha", `{}`, core.CapabilityManageWorkspace},
		{http.MethodPost, "/v1/reference-documents?workspace_id=alpha", `{}`, core.CapabilityConfirmDocuments},
		{http.MethodPost, "/v1/reference-documents/ref/versions?workspace_id=alpha", `{}`, core.CapabilityConfirmDocuments},
		{http.MethodDelete, "/v1/reference-documents/ref?workspace_id=alpha", "", core.CapabilityConfirmDocuments},
		{http.MethodPost, "/v1/decisions/DEC-1/dismiss?workspace_id=alpha", `{}`, core.CapabilityConfirmDocuments},
		{http.MethodPost, "/v1/monitor/observations?workspace_id=alpha", `{}`, core.CapabilityManageWorkspace},
		{http.MethodPost, "/v1/monitor/drift/drift/resolve?workspace_id=alpha", `{}`, core.CapabilityManageWorkspace},
		{http.MethodPost, "/v1/workers/pairings?workspace_id=alpha", `{}`, core.CapabilityManageWorkspace},
		{http.MethodDelete, "/v1/workers/worker?workspace_id=alpha", "", core.CapabilityManageWorkspace},
	} {
		fixture.capabilityCalls = nil
		response := call(route.method, route.path, "maintainer-token", route.body)
		if response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
			t.Fatalf("maintainer forbidden route %s status=%d body=%q", route.path, response.Code, response.Body.String())
		}
		if len(fixture.capabilityCalls) == 0 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != route.capability {
			t.Fatalf("maintainer forbidden route %s calls=%v want=%q", route.path, fixture.capabilityCalls, route.capability)
		}
		fixture.capabilityCalls = nil
		_ = call(route.method, route.path, "operator-token", route.body)
		if len(fixture.capabilityCalls) == 0 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != route.capability {
			t.Fatalf("operator route %s calls=%v want=%q", route.path, fixture.capabilityCalls, route.capability)
		}
	}
	for _, route := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/tasks?workspace_id=alpha", `{}`},
		{http.MethodPut, "/v1/tasks/task/assignee?workspace_id=alpha", `{}`},
		{http.MethodPut, "/v1/tasks/task/hold?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/tasks/task/review?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/tasks/task/close?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/tasks/task/merge?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/tasks/task/merge-conflict-fix?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/tasks/task/redispatch?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/work-orders/order/recover?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/work-orders/order/preempt?workspace_id=alpha", `{}`},
	} {
		response := call(route.method, route.path, "maintainer-token", route.body)
		if response.Code == http.StatusNotFound && response.Body.String() == canonicalWorkspaceNotFoundBody() {
			t.Fatalf("maintainer allowed route %s was capability-refused", route.path)
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
			"user":     {"alpha": core.WorkspaceRoleContributor},
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

func TestSoleOperatorDemotionIsStructuredAndActionable(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}},
		grantErr:   store.ErrLastWorkspaceOperator,
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/alpha/members", strings.NewReader(`{"email":"operator@example.test","role":"viewer"}`))
	request.Header.Set("Authorization", "Bearer operator-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error":"last_workspace_operator"`) ||
		!strings.Contains(response.Body.String(), "cannot demote the sole workspace operator; grant another operator first") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestPendingInvitationListingIsGatedOnManageMembership(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"operator": {"alpha": core.WorkspaceRoleOperator},
			"member":   {"alpha": core.WorkspaceRoleContributor},
		},
		invitations: []core.WorkspaceInvitation{
			{WorkspaceID: "alpha", Email: "invited@example.test", Role: core.WorkspaceRoleContributor, InvitedBy: "operator", InvitedByDisplayName: "Ops"},
		},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{
		"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"member-token":   {ID: "pat_member", OwnerUserID: "member", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	call := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/workspaces/alpha/invitations", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	fixture.capabilityCalls = nil
	operator := call("operator-token")
	if operator.Code != http.StatusOK || !strings.Contains(operator.Body.String(), "invited@example.test") {
		t.Fatalf("operator listing status=%d body=%q", operator.Code, operator.Body.String())
	}
	if last := fixture.capabilityCalls[len(fixture.capabilityCalls)-1]; last != core.CapabilityManageMembership {
		t.Fatalf("invitation listing capability calls=%v", fixture.capabilityCalls)
	}
	if len(fixture.invitationList) != 1 || fixture.invitationList[0] != "alpha" {
		t.Fatalf("invitation listing workspaces=%v", fixture.invitationList)
	}

	member := call("member-token")
	if member.Code != http.StatusNotFound || member.Body.String() != canonicalWorkspaceNotFoundBody() {
		t.Fatalf("member listing status=%d body=%q", member.Code, member.Body.String())
	}
	if len(fixture.invitationList) != 1 {
		t.Fatalf("member reached the invitation store: %v", fixture.invitationList)
	}
}

func TestPendingInvitationListingEmitsAnArrayAndHidesStoreFailures(t *testing.T) {
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	call := func() *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/workspaces/alpha/invitations", nil)
		request.Header.Set("Authorization", "Bearer operator-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	empty := call()
	if empty.Code != http.StatusOK || empty.Body.String() != "[]\n" {
		t.Fatalf("empty listing status=%d body=%q", empty.Code, empty.Body.String())
	}

	rawError := "invitation query failed with secret detail"
	fixture.invitationsErr = errors.New(rawError)
	failed := call()
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), rawError) || failed.Body.String() != "internal server error\n" {
		t.Fatalf("infrastructure status=%d body=%q", failed.Code, failed.Body.String())
	}
}

func canonicalWorkspaceNotFoundBody() string {
	response := httptest.NewRecorder()
	writeWorkspaceNotFound(response)
	return response.Body.String()
}
