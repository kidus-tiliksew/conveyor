package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type callerIdentityFixture struct {
	store.Store
	calls       int
	userID      string
	workspaceID string
	sessionID   string
	displayName string
	err         error
}

func (f *callerIdentityFixture) GetCallerIdentity(_ context.Context, userID, workspaceID string) (core.CallerIdentity, error) {
	f.calls++
	f.userID, f.workspaceID = userID, workspaceID
	identity := core.CallerIdentity{ID: userID, Email: "owner@example.test", DisplayName: "Owner"}
	if workspaceID != "" {
		identity.Role = core.WorkspaceRoleContributor
	}
	return identity, f.err
}

func (f *callerIdentityFixture) SetOwnDisplayName(_ context.Context, userID, sessionID, displayName string) (core.CallerIdentity, error) {
	f.calls++
	f.userID, f.sessionID, f.displayName = userID, sessionID, displayName
	return core.CallerIdentity{ID: userID, Email: "owner@example.test", DisplayName: displayName}, f.err
}

func TestCallerIdentityMapsMissingBindingToNotFound(t *testing.T) {
	t.Parallel()
	identities := &callerIdentityFixture{Store: store.NewMemory(), err: fmt.Errorf("deactivated user: %w", store.ErrNotFound)}
	server := NewServer(identities)
	server.CallerIdentities = identities
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "pat-missing", OwnerUserID: "usr-missing", Kind: core.CredentialUser}))
	response := httptest.NewRecorder()
	server.getCallerIdentity(response, request)

	if response.Code != http.StatusNotFound || response.Body.String() != "caller identity unavailable\n" || strings.Contains(response.Body.String(), identities.err.Error()) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCallerIdentityIsCredentialDerivedAndWorkspaceScoped(t *testing.T) {
	t.Parallel()
	identities := &callerIdentityFixture{Store: store.NewMemory()}
	memberships := &membershipFixture{
		roles: map[string]map[string]core.WorkspaceRole{
			"usr-owner": {"visible": core.WorkspaceRoleContributor},
		},
	}
	server := NewServer(identities)
	server.CallerIdentities = identities
	server.Memberships = memberships
	server.Credentials = staticCredentialVerifier{
		"human-secret": {ID: "pat-owner", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent-secret": {ID: "agt-owner", OwnerUserID: "usr-owner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
	}
	call := func(token, target string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		request.Header.Set("X-Conveyor-Actor", "user:usr-attacker")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	unscoped := call("human-secret", "/v1/me?user_id=usr-attacker")
	if unscoped.Code != http.StatusOK {
		t.Fatalf("unscoped status=%d body=%s", unscoped.Code, unscoped.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(unscoped.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "usr-owner" || got["email"] != "owner@example.test" || got["display_name"] != "Owner" || got["role"] != nil || identities.userID != "usr-owner" || identities.workspaceID != "" {
		t.Fatalf("unscoped identity=%v requested=%q/%q", got, identities.userID, identities.workspaceID)
	}

	scoped := call("human-secret", "/v1/me?workspace_id=visible&user_id=usr-attacker")
	if scoped.Code != http.StatusOK {
		t.Fatalf("scoped status=%d body=%s", scoped.Code, scoped.Body.String())
	}
	if err := json.Unmarshal(scoped.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "usr-owner" || got["role"] != string(core.WorkspaceRoleContributor) || identities.workspaceID != "visible" {
		t.Fatalf("scoped identity=%v requested=%q/%q", got, identities.userID, identities.workspaceID)
	}
	if len(memberships.capabilityCalls) != 1 || memberships.capabilityCalls[0] != core.CapabilityViewWorkspace {
		t.Fatalf("capability calls=%v", memberships.capabilityCalls)
	}

	before := identities.calls
	unauthorizedWorkspace := call("human-secret", "/v1/me?workspace_id=hidden")
	if unauthorizedWorkspace.Code != http.StatusNotFound || identities.calls != before {
		t.Fatalf("hidden status=%d body=%s identity_calls=%d", unauthorizedWorkspace.Code, unauthorizedWorkspace.Body.String(), identities.calls)
	}
	for _, token := range []string{"", "agent-secret", "worker-secret"} {
		response := call(token, "/v1/me")
		if response.Code != http.StatusUnauthorized || identities.calls != before {
			t.Fatalf("token=%q status=%d body=%s identity_calls=%d", token, response.Code, response.Body.String(), identities.calls)
		}
	}
}

func TestPutOwnDisplayNameRequiresSessionAndUsesCredentialOwner(t *testing.T) {
	t.Parallel()
	profiles := &callerIdentityFixture{Store: store.NewMemory()}
	server := NewServer(profiles)
	server.OwnProfiles = profiles
	server.Credentials = staticCredentialVerifier{
		"human-secret": {ID: "pat-owner", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	server.InvitationSessions = &invitationSessionFixture{credential: core.AuthenticatedCredential{
		ID: "ses-owner", OwnerUserID: "usr-owner", Kind: core.CredentialUser,
		Scope: core.CredentialScopeUser, Method: core.CredentialMethodSession,
	}}
	call := func(body, bearer string, session bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPut, "http://conveyor.example/v1/me", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		if session {
			request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-secret"})
			request.Header.Set("X-Conveyor-CSRF", "1")
			request.Header.Set("Origin", "http://conveyor.example")
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	if response := call(`{"display_name":"  Chosen Name  "}`, "", true); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"display_name":"Chosen Name"`) {
		t.Fatalf("session status=%d body=%s", response.Code, response.Body.String())
	}
	if profiles.userID != "usr-owner" || profiles.sessionID != "ses-owner" || profiles.displayName != "Chosen Name" {
		t.Fatalf("profile target=%q session=%q name=%q", profiles.userID, profiles.sessionID, profiles.displayName)
	}
	before := profiles.calls
	if response := call(`{"display_name":"Bearer Name"}`, "human-secret", false); response.Code != http.StatusBadRequest || profiles.calls != before {
		t.Fatalf("bearer status=%d body=%s calls=%d", response.Code, response.Body.String(), profiles.calls)
	}
	for _, body := range []string{
		`{"display_name":" "}`,
		`{"display_name":"Name","user_id":"usr-attacker"}`,
		`{"display_name":"Name"} {}`,
		`{"display_name":"` + strings.Repeat("x", maxDisplayNameBytes+1) + `"}`,
	} {
		if response := call(body, "", true); response.Code != http.StatusUnprocessableEntity || profiles.calls != before {
			t.Fatalf("body=%q status=%d response=%s calls=%d", body, response.Code, response.Body.String(), profiles.calls)
		}
	}
}
