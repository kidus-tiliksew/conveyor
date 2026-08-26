package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type workspaceForgeTokenFixture struct {
	statuses                         map[string]core.ForgeTokenStatus
	tokens                           map[string]string
	storeErr                         error
	storeCalls, statusCalls, deletes int
	workspaces                       []string
}

func (f *workspaceForgeTokenFixture) StoreWorkspaceForgeToken(_ context.Context, workspaceID, token, login string) (core.ForgeTokenStatus, error) {
	f.storeCalls++
	f.workspaces = append(f.workspaces, workspaceID)
	if f.storeErr != nil {
		return core.ForgeTokenStatus{}, f.storeErr
	}
	if f.statuses == nil {
		f.statuses = map[string]core.ForgeTokenStatus{}
	}
	if f.tokens == nil {
		f.tokens = map[string]string{}
	}
	status := core.ForgeTokenStatus{Configured: true, ForgeLogin: login, StoredAt: time.Date(2026, 8, 26, 1, 2, 3, 0, time.UTC)}
	f.statuses[workspaceID], f.tokens[workspaceID] = status, token
	return status, nil
}

func (f *workspaceForgeTokenFixture) DeleteWorkspaceForgeToken(_ context.Context, workspaceID string) error {
	f.deletes++
	f.workspaces = append(f.workspaces, workspaceID)
	delete(f.statuses, workspaceID)
	delete(f.tokens, workspaceID)
	return nil
}

func (f *workspaceForgeTokenFixture) GetWorkspaceForgeTokenStatus(_ context.Context, workspaceID string) (core.ForgeTokenStatus, error) {
	f.statusCalls++
	f.workspaces = append(f.workspaces, workspaceID)
	return f.statuses[workspaceID], nil
}

func (f *workspaceForgeTokenFixture) GetWorkspaceForgeTokenForUse(_ context.Context, workspaceID string) (core.WorkspaceForgeTokenCredential, error) {
	status := f.statuses[workspaceID]
	return core.WorkspaceForgeTokenCredential{WorkspaceID: workspaceID, Token: f.tokens[workspaceID], ForgeTokenStatus: status}, nil
}

func (f *workspaceForgeTokenFixture) redactionValues() []string {
	values := make([]string, 0, len(f.tokens))
	for _, token := range f.tokens {
		values = append(values, token)
	}
	return values
}

func TestWorkspaceForgeTokenLifecycleRequiresWorkspaceOperator(t *testing.T) {
	fixture := &workspaceForgeTokenFixture{}
	memberships := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha", Name: "Alpha"}, {ID: "beta", Name: "Beta"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"operator":   {"alpha": core.WorkspaceRoleOperator, "beta": core.WorkspaceRoleOperator},
			"maintainer": {"alpha": core.WorkspaceRoleMaintainer},
		},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = memberships, memberships
	server.WorkspaceForgeTokens = fixture
	server.Credentials = staticCredentialVerifier{
		"operator-token":   {ID: "operator-pat", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"maintainer-token": {ID: "maintainer-pat", OwnerUserID: "maintainer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent-token":      {ID: "agent", OwnerUserID: "operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
	}
	server.ValidateForgeToken = func(_ context.Context, token string) (string, error) {
		if token == "github_pat_workspace-secret" {
			return "workspace-login", nil
		}
		return "", errors.New("provider detail containing " + token)
	}
	call := func(method, workspaceID, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "/v1/workspaces/"+workspaceID+"/forge-token", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		before := fixture.storeCalls + fixture.statusCalls + fixture.deletes
		if got := call(method, "alpha", "maintainer-token", `{"token":"github_pat_workspace-secret"}`); got.Code != http.StatusNotFound {
			t.Fatalf("maintainer %s status=%d body=%q", method, got.Code, got.Body.String())
		}
		if after := fixture.storeCalls + fixture.statusCalls + fixture.deletes; after != before {
			t.Fatalf("maintainer %s reached store: before=%d after=%d", method, before, after)
		}
		if got := call(method, "alpha", "agent-token", `{"token":"github_pat_workspace-secret"}`); got.Code != http.StatusUnauthorized {
			t.Fatalf("agent %s status=%d body=%q", method, got.Code, got.Body.String())
		}
	}
	if got := call(http.MethodGet, "beta", "maintainer-token", ""); got.Code != http.StatusNotFound || fixture.statusCalls != 0 {
		t.Fatalf("cross-workspace status=%d calls=%d body=%q", got.Code, fixture.statusCalls, got.Body.String())
	}

	invalidSecret := "github_pat_invalid-workspace-secret"
	invalid := call(http.MethodPut, "alpha", "operator-token", `{"token":"`+invalidSecret+`"}`)
	if invalid.Code != http.StatusUnprocessableEntity || invalid.Body.String() != forgeTokenValidationFailure+"\n" || fixture.storeCalls != 0 || strings.Contains(invalid.Body.String(), invalidSecret) {
		t.Fatalf("invalid status=%d stores=%d body=%q", invalid.Code, fixture.storeCalls, invalid.Body.String())
	}

	put := call(http.MethodPut, "alpha", "operator-token", `{"token":"github_pat_workspace-secret"}`)
	if put.Code != http.StatusOK || strings.Contains(put.Body.String(), "github_pat_workspace-secret") || !strings.Contains(put.Body.String(), `"forge_login":"workspace-login"`) {
		t.Fatalf("put status=%d body=%q", put.Code, put.Body.String())
	}
	if values := fixture.redactionValues(); len(values) != 1 || values[0] != "github_pat_workspace-secret" {
		t.Fatalf("redaction values=%v", values)
	}
	get := call(http.MethodGet, "alpha", "operator-token", "")
	if get.Code != http.StatusOK || strings.Contains(get.Body.String(), "github_pat_workspace-secret") || !strings.Contains(get.Body.String(), `"configured":true`) {
		t.Fatalf("get status=%d body=%q", get.Code, get.Body.String())
	}
	if got := call(http.MethodDelete, "alpha", "operator-token", ""); got.Code != http.StatusNoContent || got.Body.Len() != 0 {
		t.Fatalf("delete status=%d body=%q", got.Code, got.Body.String())
	}
	if got := call(http.MethodDelete, "alpha", "operator-token", ""); got.Code != http.StatusNoContent {
		t.Fatalf("idempotent delete status=%d body=%q", got.Code, got.Body.String())
	}
}

func TestWorkspaceForgeTokenStoreFailureDoesNotLogSecret(t *testing.T) {
	secret := "github_pat_must-not-enter-log"
	fixture := &workspaceForgeTokenFixture{storeErr: errors.New("persistence failed for " + secret)}
	memberships := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha", Name: "Alpha"}},
		roles:      map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = memberships, memberships
	server.WorkspaceForgeTokens = fixture
	server.Credentials = staticCredentialVerifier{"operator-token": {ID: "operator-pat", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	server.ValidateForgeToken = func(context.Context, string) (string, error) { return "workspace-login", nil }

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	request := httptest.NewRequest(http.MethodPut, "/v1/workspaces/alpha/forge-token", strings.NewReader(`{"token":"`+secret+`"}`))
	request.Header.Set("Authorization", "Bearer operator-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("status=%d body=%q logs=%q", response.Code, response.Body.String(), logs.String())
	}
}
