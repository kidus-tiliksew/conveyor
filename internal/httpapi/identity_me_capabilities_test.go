package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// boundCallerIdentityFixture answers GetCallerIdentity from the same role map
// the membership fixture authorizes against, like the stores, which read the
// live binding. beforeRead runs between middleware authorization and the
// identity read so a test can change or revoke the binding deterministically.
type boundCallerIdentityFixture struct {
	store.Store
	roles      map[string]map[string]core.WorkspaceRole
	beforeRead func(userID, workspaceID string)
	calls      int
	workspaces []string
}

func (f *boundCallerIdentityFixture) GetCallerIdentity(_ context.Context, userID, workspaceID string) (core.CallerIdentity, error) {
	f.calls++
	f.workspaces = append(f.workspaces, workspaceID)
	if f.beforeRead != nil {
		f.beforeRead(userID, workspaceID)
	}
	identity := core.CallerIdentity{ID: userID, Email: userID + "@example.test", DisplayName: "Caller"}
	if workspaceID == "" {
		return identity, nil
	}
	role, ok := f.roles[userID][workspaceID]
	if !ok {
		return core.CallerIdentity{}, store.ErrNotFound
	}
	identity.Role = role
	return identity, nil
}

// governedCapabilities is the complete capability vocabulary used to compute
// expected bundles from the Go policy, independent of the enumeration helper.
var governedCapabilities = []core.Capability{
	core.CapabilityViewWorkspace,
	core.CapabilityClaimWork,
	core.CapabilityRequestChanges,
	core.CapabilityProposeDocuments,
	core.CapabilityConfirmDocuments,
	core.CapabilityManageMembership,
	core.CapabilityCreateTasks,
	core.CapabilitySetAssignee,
	core.CapabilityOperateGates,
	core.CapabilityRecoverWork,
	core.CapabilityManageWorkspace,
	core.CapabilityManageReferenceDocuments,
}

type callerIdentityProjectionBody struct {
	ID           string   `json:"id"`
	Role         *string  `json:"role"`
	Capabilities []string `json:"capabilities"`
	Roles        []string `json:"roles"`
	raw          map[string]json.RawMessage
}

func decodeCallerIdentityProjection(t *testing.T, response *httptest.ResponseRecorder) callerIdentityProjectionBody {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body callerIdentityProjectionBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body.raw); err != nil {
		t.Fatal(err)
	}
	return body
}

func expectedCapabilityNames(role core.WorkspaceRole) []string {
	var names []string
	for _, capability := range governedCapabilities {
		if core.RoleAllows(role, capability) {
			names = append(names, string(capability))
		}
	}
	slices.Sort(names)
	return names
}

var expectedRoleChain = []string{"viewer", "executor", "contributor", "maintainer", "operator"}

// TestCallerIdentityCapabilitiesForEveryRole proves that a scoped /v1/me
// serves exactly the caller's Go-policy bundle and the ordered role chain for
// every role, under both a personal access token and a dashboard session
// (req-accounts-and-membership AC-5.1).
func TestCallerIdentityCapabilitiesForEveryRole(t *testing.T) {
	t.Parallel()
	roles := []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator}
	for _, role := range roles {
		for _, method := range []string{"pat", "session"} {
			t.Run(string(role)+"/"+method, func(t *testing.T) {
				t.Parallel()
				userID := "usr-" + string(role)
				bindings := map[string]map[string]core.WorkspaceRole{userID: {"demo": role}}
				identities := &boundCallerIdentityFixture{Store: store.NewMemory(), roles: bindings}
				server := NewServer(identities)
				server.CallerIdentities = identities
				server.Memberships = &membershipFixture{roles: bindings}
				server.Credentials = staticCredentialVerifier{
					"human-secret": {ID: "pat-" + string(role), OwnerUserID: userID, Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
				}
				server.InvitationSessions = &invitationSessionFixture{credential: core.AuthenticatedCredential{
					ID: "ses-" + string(role), OwnerUserID: userID, Kind: core.CredentialUser,
					Scope: core.CredentialScopeUser, Method: core.CredentialMethodSession,
				}}

				request := httptest.NewRequest(http.MethodGet, "/v1/me?workspace_id=demo", nil)
				if method == "pat" {
					request.Header.Set("Authorization", "Bearer human-secret")
				} else {
					request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-secret"})
				}
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				body := decodeCallerIdentityProjection(t, response)

				if body.ID != userID || body.Role == nil || *body.Role != string(role) {
					t.Fatalf("identity=%s", response.Body.String())
				}
				if want := expectedCapabilityNames(role); !slices.Equal(body.Capabilities, want) {
					t.Fatalf("capabilities=%v, want %v", body.Capabilities, want)
				}
				if !slices.Equal(body.Roles, expectedRoleChain) {
					t.Fatalf("roles=%v, want %v", body.Roles, expectedRoleChain)
				}
			})
		}
	}
}

// TestCallerIdentityProjectionRequiresAuthorizedWorkspace proves the lists
// appear only after explicit workspace context is authorized, and that every
// existing refusal is unchanged (DEC-19; req-accounts-and-membership AC-4.2).
func TestCallerIdentityProjectionRequiresAuthorizedWorkspace(t *testing.T) {
	t.Parallel()
	bindings := map[string]map[string]core.WorkspaceRole{"usr-owner": {"visible": core.WorkspaceRoleMaintainer}}
	identities := &boundCallerIdentityFixture{Store: store.NewMemory(), roles: bindings}
	server := NewServer(identities)
	server.CallerIdentities = identities
	server.Memberships = &membershipFixture{roles: bindings}
	server.Credentials = staticCredentialVerifier{
		"human-secret": {ID: "pat-owner", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent-secret": {ID: "agt-owner", OwnerUserID: "usr-owner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
	}
	call := func(server *Server, token, target, header string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if header != "" {
			request.Header.Set("X-Workspace-ID", header)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	wantCapabilities := expectedCapabilityNames(core.WorkspaceRoleMaintainer)

	for name, scope := range map[string][2]string{
		"query":    {"/v1/me?workspace_id=visible", ""},
		"header":   {"/v1/me", "visible"},
		"matching": {"/v1/me?workspace_id=visible", "visible"},
	} {
		body := decodeCallerIdentityProjection(t, call(server, "human-secret", scope[0], scope[1]))
		if body.Role == nil || *body.Role != "maintainer" || !slices.Equal(body.Capabilities, wantCapabilities) || !slices.Equal(body.Roles, expectedRoleChain) {
			t.Fatalf("%s scope projection=%+v", name, body)
		}
	}

	unscoped := decodeCallerIdentityProjection(t, call(server, "human-secret", "/v1/me", ""))
	for _, field := range []string{"role", "capabilities", "roles"} {
		if _, present := unscoped.raw[field]; present {
			t.Fatalf("unscoped response carries %q: %v", field, unscoped.raw)
		}
	}
	if unscoped.ID != "usr-owner" {
		t.Fatalf("unscoped identity=%+v", unscoped)
	}

	before := identities.calls
	conflict := call(server, "human-secret", "/v1/me?workspace_id=visible", "other")
	if conflict.Code != http.StatusBadRequest || identities.calls != before {
		t.Fatalf("conflict status=%d body=%s identity_calls=%d", conflict.Code, conflict.Body.String(), identities.calls-before)
	}

	foreign := call(server, "human-secret", "/v1/me?workspace_id=foreign", "")
	missing := call(server, "human-secret", "/v1/me", "missing")
	if foreign.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() || identities.calls != before {
		t.Fatalf("foreign=%d %s missing=%d %s identity_calls=%d", foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String(), identities.calls-before)
	}
	var refusal map[string]string
	if err := json.Unmarshal(foreign.Body.Bytes(), &refusal); err != nil || refusal["error"] != "workspace_not_found" {
		t.Fatalf("foreign refusal=%s err=%v", foreign.Body.String(), err)
	}

	withoutAuthority := NewServer(identities)
	withoutAuthority.CallerIdentities = identities
	withoutAuthority.Credentials = server.Credentials
	if response := call(withoutAuthority, "human-secret", "/v1/me?workspace_id=visible", ""); response.Code != http.StatusNotFound || response.Body.String() != foreign.Body.String() || identities.calls != before {
		t.Fatalf("no membership authority status=%d body=%s identity_calls=%d", response.Code, response.Body.String(), identities.calls-before)
	}

	for _, token := range []string{"", "agent-secret", "cv_worker_unknown_secret"} {
		response := call(server, token, "/v1/me?workspace_id=visible", "")
		if response.Code != http.StatusUnauthorized || identities.calls != before {
			t.Fatalf("token=%q status=%d body=%s identity_calls=%d", token, response.Code, response.Body.String(), identities.calls-before)
		}
	}
}

// TestCallerIdentityProjectionUsesResolvedBinding proves the served lists
// follow the role returned by the identity read, not the middleware's earlier
// observation. The fixture hook runs synchronously between the two reads, so
// the interleaving is deterministic; it models the ordering only and does not
// claim database transaction behavior.
func TestCallerIdentityProjectionUsesResolvedBinding(t *testing.T) {
	t.Parallel()
	bindings := map[string]map[string]core.WorkspaceRole{"usr-owner": {"demo": core.WorkspaceRoleViewer}}
	memberships := &membershipFixture{roles: bindings}
	identities := &boundCallerIdentityFixture{Store: store.NewMemory(), roles: bindings}
	server := NewServer(identities)
	server.CallerIdentities = identities
	server.Memberships = memberships
	server.Credentials = staticCredentialVerifier{
		"human-secret": {ID: "pat-owner", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v1/me?workspace_id=demo", nil)
		request.Header.Set("Authorization", "Bearer human-secret")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	// Promotion after authorization: the viewer passed view_workspace, then
	// the binding became operator before the identity read.
	identities.beforeRead = func(userID, workspaceID string) {
		if len(memberships.capabilityCalls) != 1 || memberships.capabilityCalls[0] != core.CapabilityViewWorkspace {
			t.Errorf("identity read before authorization: calls=%v", memberships.capabilityCalls)
		}
		bindings[userID][workspaceID] = core.WorkspaceRoleOperator
	}
	promoted := decodeCallerIdentityProjection(t, call())
	if promoted.Role == nil || *promoted.Role != "operator" || !slices.Equal(promoted.Capabilities, expectedCapabilityNames(core.WorkspaceRoleOperator)) || !slices.Equal(promoted.Roles, expectedRoleChain) {
		t.Fatalf("promoted projection=%+v", promoted)
	}

	// Demotion after authorization: the operator passed, then became viewer.
	identities.beforeRead = func(userID, workspaceID string) {
		bindings[userID][workspaceID] = core.WorkspaceRoleViewer
	}
	demoted := decodeCallerIdentityProjection(t, call())
	if demoted.Role == nil || *demoted.Role != "viewer" || !slices.Equal(demoted.Capabilities, []string{"view_workspace"}) {
		t.Fatalf("demoted projection=%+v", demoted)
	}

	// Revocation after authorization: the identity read answers not found, so
	// no role, capability, or role-chain list is served.
	bindings["usr-owner"]["demo"] = core.WorkspaceRoleContributor
	identities.beforeRead = func(userID, workspaceID string) {
		delete(bindings[userID], workspaceID)
	}
	revoked := call()
	if revoked.Code != http.StatusNotFound || revoked.Body.String() != "caller identity unavailable\n" {
		t.Fatalf("revoked status=%d body=%s", revoked.Code, revoked.Body.String())
	}
}

// TestCallerIdentityProjectionClearsUnscopedLists proves the handler, not the
// store, decides the projection: a store result carrying lists or a role on an
// unscoped read serves none of them, and a scoped read without a role serves
// no lists.
func TestCallerIdentityProjectionClearsUnscopedLists(t *testing.T) {
	t.Parallel()
	stale := core.CallerIdentity{
		ID: "usr-owner", Role: core.WorkspaceRoleOperator,
		Capabilities: []core.Capability{core.CapabilityManageWorkspace},
		Roles:        []core.WorkspaceRole{core.WorkspaceRoleOperator},
	}
	if got := callerIdentityProjection(stale, ""); got.Role != "" || got.Capabilities != nil || got.Roles != nil {
		t.Fatalf("unscoped projection=%+v", got)
	}
	roleless := stale
	roleless.Role = ""
	if got := callerIdentityProjection(roleless, "demo"); got.Capabilities != nil || got.Roles != nil {
		t.Fatalf("roleless scoped projection=%+v", got)
	}
	future := callerIdentityProjection(core.CallerIdentity{ID: "usr-owner", Role: "owner"}, "demo")
	if len(future.Capabilities) != 0 || !slices.Equal(future.Roles, core.WorkspaceRoles()) {
		t.Fatalf("unknown role projection=%+v", future)
	}
}
