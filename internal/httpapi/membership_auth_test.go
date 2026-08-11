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
	workspaces []core.Workspace
	roles      map[string]map[string]core.WorkspaceRole
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
func (f *membershipFixture) RevokeWorkspaceRole(context.Context, string, string) error { return nil }

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
