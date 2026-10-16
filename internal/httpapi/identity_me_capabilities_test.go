package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	// storeCanCreate makes every read return CanCreateWorkspaces true, so a
	// test can prove the handler overwrites a store value.
	storeCanCreate bool
	displayName    string
}

func (f *boundCallerIdentityFixture) GetCallerIdentity(_ context.Context, userID, workspaceID string) (core.CallerIdentity, error) {
	f.calls++
	f.workspaces = append(f.workspaces, workspaceID)
	if f.beforeRead != nil {
		f.beforeRead(userID, workspaceID)
	}
	identity := core.CallerIdentity{ID: userID, Email: userID + "@example.test", DisplayName: "Caller", CanCreateWorkspaces: f.storeCanCreate}
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

func (f *boundCallerIdentityFixture) SetOwnDisplayName(_ context.Context, userID, _, displayName string) (core.CallerIdentity, error) {
	f.calls++
	f.displayName = displayName
	return core.CallerIdentity{ID: userID, Email: userID + "@example.test", DisplayName: displayName, CanCreateWorkspaces: f.storeCanCreate}, nil
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
	ID                  string   `json:"id"`
	Role                *string  `json:"role"`
	Capabilities        []string `json:"capabilities"`
	Roles               []string `json:"roles"`
	CanCreateWorkspaces *bool    `json:"can_create_workspaces"`
	raw                 map[string]json.RawMessage
}

// creationRight reads the explicit can_create_workspaces boolean; an absent
// or non-boolean field fails the test rather than reading as false.
func (b callerIdentityProjectionBody) creationRight(t *testing.T) bool {
	t.Helper()
	raw, present := b.raw["can_create_workspaces"]
	if !present || (string(raw) != "true" && string(raw) != "false") || b.CanCreateWorkspaces == nil {
		t.Fatalf("can_create_workspaces must be an explicit boolean: %v", b.raw)
	}
	return *b.CanCreateWorkspaces
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

// creationRightServer serves one caller under either human credential form.
// The session fixture authenticates every cookie as the same caller, so each
// caller and credential form gets its own server.
func creationRightServer(identities *boundCallerIdentityFixture, memberships *membershipFixture, userID, method string) *Server {
	server := NewServer(identities)
	server.CallerIdentities = identities
	server.OwnProfiles = identities
	if memberships != nil {
		server.Memberships = memberships
	}
	server.Credentials = staticCredentialVerifier{
		"human-secret": {ID: "pat-" + userID, OwnerUserID: userID, Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	server.InvitationSessions = &invitationSessionFixture{credential: core.AuthenticatedCredential{
		ID: "ses-" + userID, OwnerUserID: userID, Kind: core.CredentialUser,
		Scope: core.CredentialScopeUser, Method: core.CredentialMethodSession,
	}}
	return server
}

func creationRightRequest(server *Server, method, httpMethod, target, header, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(httpMethod, "http://conveyor.example"+target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if method == "pat" {
		request.Header.Set("Authorization", "Bearer human-secret")
	} else {
		request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-secret"})
		request.Header.Set("X-Conveyor-CSRF", "1")
		request.Header.Set("Origin", "http://conveyor.example")
	}
	if header != "" {
		request.Header.Set("X-Workspace-ID", header)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

// TestCallerIdentityCanCreateWorkspaces proves every successful /v1/me read
// serves the caller's own creation right: the deployment owner and an active
// operator of any workspace read true and a contributor-only account reads
// false, identically for unscoped, query-scoped, and header-scoped reads
// under a personal access token and a dashboard session. An account that is
// contributor in the selected workspace and operator in another reads true,
// so the right never follows the selected workspace. The role projection is
// unchanged (component-identity-membership; req-accounts-and-membership
// REQ-4, AC-1.4, AC-5.1; DEC-75(2)).
func TestCallerIdentityCanCreateWorkspaces(t *testing.T) {
	t.Parallel()
	bindings := map[string]map[string]core.WorkspaceRole{
		"usr-owner":         {"demo": core.WorkspaceRoleViewer},
		"usr-operator":      {"demo": core.WorkspaceRoleOperator},
		"usr-contributor":   {"demo": core.WorkspaceRoleContributor},
		"usr-mixed":         {"demo": core.WorkspaceRoleContributor, "other": core.WorkspaceRoleOperator},
		"usr-unbound-owner": {},
	}
	admins := map[string]bool{"usr-owner": true, "usr-unbound-owner": true}
	scopes := []struct{ name, target, header string }{
		{"unscoped", "/v1/me", ""},
		{"query", "/v1/me?workspace_id=demo", ""},
		{"header", "/v1/me", "demo"},
	}
	for _, caller := range []struct {
		id   string
		want bool
	}{{"usr-owner", true}, {"usr-operator", true}, {"usr-contributor", false}, {"usr-mixed", true}, {"usr-unbound-owner", true}} {
		for _, method := range []string{"pat", "session"} {
			t.Run(caller.id+"/"+method, func(t *testing.T) {
				t.Parallel()
				identities := &boundCallerIdentityFixture{Store: store.NewMemory(), roles: bindings}
				memberships := &membershipFixture{roles: bindings, instanceAdmins: admins}
				server := creationRightServer(identities, memberships, caller.id, method)
				for _, scope := range scopes {
					identityCalls, instanceCalls := identities.calls, memberships.instanceCalls
					response := creationRightRequest(server, method, http.MethodGet, scope.target, scope.header, "")
					role, bound := bindings[caller.id]["demo"]
					if scope.name != "unscoped" && !bound {
						// No binding: the uniform 404 precedes every identity and
						// creation read, so the right never leaks through context.
						if response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound || identities.calls != identityCalls || memberships.instanceCalls != instanceCalls {
							t.Fatalf("%s unbound status=%d body=%s identity_calls=%d instance_calls=%d", scope.name, response.Code, response.Body.String(), identities.calls-identityCalls, memberships.instanceCalls-instanceCalls)
						}
						continue
					}
					body := decodeCallerIdentityProjection(t, response)
					if got := body.creationRight(t); got != caller.want {
						t.Fatalf("%s can_create_workspaces=%v, want %v", scope.name, got, caller.want)
					}
					if scope.name == "unscoped" {
						for _, field := range []string{"role", "capabilities", "roles"} {
							if _, present := body.raw[field]; present {
								t.Fatalf("unscoped response carries %q: %v", field, body.raw)
							}
						}
						continue
					}
					if body.Role == nil || *body.Role != string(role) || !slices.Equal(body.Capabilities, expectedCapabilityNames(role)) || !slices.Equal(body.Roles, expectedRoleChain) {
						t.Fatalf("%s projection changed: %+v", scope.name, body)
					}
				}
			})
		}
	}
}

// TestCallerIdentityCanCreateWorkspacesFailsClosed proves the field is an
// explicit false without a membership authority, an owner-check error answers
// the generic 500 without its text and never reaches the workspace-creator
// check, a workspace-creator-check error answers the same 500, and a store
// value never grants the right (DEC-75(2), DEC-75(5)).
func TestCallerIdentityCanCreateWorkspacesFailsClosed(t *testing.T) {
	t.Parallel()
	bindings := map[string]map[string]core.WorkspaceRole{"usr-operator": {"demo": core.WorkspaceRoleOperator}}

	withoutAuthority := creationRightServer(&boundCallerIdentityFixture{Store: store.NewMemory(), roles: bindings, storeCanCreate: true}, nil, "usr-operator", "pat")
	if got := decodeCallerIdentityProjection(t, creationRightRequest(withoutAuthority, "pat", http.MethodGet, "/v1/me", "", "")).creationRight(t); got {
		t.Fatal("no membership authority served can_create_workspaces=true")
	}

	const detail = "instance authority unreachable: secret-dsn"
	for _, failing := range []struct {
		name      string
		authority *membershipFixture
	}{
		{"owner check", &membershipFixture{roles: bindings, instanceAdminErr: errors.New(detail)}},
		{"creator check", &membershipFixture{roles: bindings, instanceAdmins: map[string]bool{}, authorizeErrs: map[core.Capability]error{core.CapabilityManageWorkspace: errors.New(detail)}}},
	} {
		for _, method := range []string{"pat", "session"} {
			server := creationRightServer(&boundCallerIdentityFixture{Store: store.NewMemory(), roles: bindings}, failing.authority, "usr-operator", method)
			for _, target := range []string{"/v1/me", "/v1/me?workspace_id=demo"} {
				response := creationRightRequest(server, method, http.MethodGet, target, "", "")
				if response.Code != http.StatusInternalServerError || response.Body.String() != "internal server error\n" || strings.Contains(response.Body.String(), "secret") {
					t.Fatalf("%s %s %s status=%d body=%q", failing.name, method, target, response.Code, response.Body.String())
				}
			}
		}
	}
	// An owner-check error never falls through to the workspace-creator
	// check, which would otherwise grant this operator the right.
	ownerFailure := &membershipFixture{roles: bindings, instanceAdminErr: errors.New(detail)}
	server := creationRightServer(&boundCallerIdentityFixture{Store: store.NewMemory(), roles: bindings}, ownerFailure, "usr-operator", "pat")
	creationRightRequest(server, "pat", http.MethodGet, "/v1/me", "", "")
	if ownerFailure.instanceCalls != 1 || slices.Contains(ownerFailure.capabilityCalls, core.CapabilityManageWorkspace) {
		t.Fatalf("owner-check error consulted instance=%d capabilities=%v", ownerFailure.instanceCalls, ownerFailure.capabilityCalls)
	}

	contributor := map[string]map[string]core.WorkspaceRole{"usr-contributor": {"demo": core.WorkspaceRoleContributor}}
	stale := &boundCallerIdentityFixture{Store: store.NewMemory(), roles: contributor, storeCanCreate: true}
	server = creationRightServer(stale, &membershipFixture{roles: contributor, instanceAdmins: map[string]bool{}}, "usr-contributor", "session")
	for _, target := range []string{"/v1/me", "/v1/me?workspace_id=demo"} {
		if got := decodeCallerIdentityProjection(t, creationRightRequest(server, "session", http.MethodGet, target, "", "")).creationRight(t); got {
			t.Fatalf("%s served a store-returned creation right", target)
		}
	}
}

// TestOwnDisplayNameCanCreateWorkspaces proves PUT /v1/me, which returns the
// same CallerIdentity shape, computes the creation right with the same
// predicate before writing, and writes nothing on an authority error. The
// owner and a nonowner operator read true and a contributor-only account
// reads false (DEC-75(2)).
func TestOwnDisplayNameCanCreateWorkspaces(t *testing.T) {
	t.Parallel()
	roles := map[string]map[string]core.WorkspaceRole{
		"usr-operator":    {"demo": core.WorkspaceRoleOperator},
		"usr-contributor": {"demo": core.WorkspaceRoleContributor},
	}
	for _, caller := range []struct {
		id   string
		want bool
	}{{"usr-owner", true}, {"usr-operator", true}, {"usr-contributor", false}} {
		identities := &boundCallerIdentityFixture{Store: store.NewMemory(), storeCanCreate: !caller.want}
		server := creationRightServer(identities, &membershipFixture{roles: roles, instanceAdmins: map[string]bool{"usr-owner": true}}, caller.id, "session")
		response := creationRightRequest(server, "session", http.MethodPut, "/v1/me", "", `{"display_name":"Renamed"}`)
		body := decodeCallerIdentityProjection(t, response)
		if got := body.creationRight(t); got != caller.want || identities.displayName != "Renamed" {
			t.Fatalf("%s can_create_workspaces=%v want %v name=%q", caller.id, got, caller.want, identities.displayName)
		}
	}

	identities := &boundCallerIdentityFixture{Store: store.NewMemory()}
	server := creationRightServer(identities, &membershipFixture{instanceAdminErr: errors.New("authority down")}, "usr-owner", "session")
	response := creationRightRequest(server, "session", http.MethodPut, "/v1/me", "", `{"display_name":"Renamed"}`)
	if response.Code != http.StatusInternalServerError || response.Body.String() != "internal server error\n" || identities.calls != 0 || identities.displayName != "" {
		t.Fatalf("authority error status=%d body=%q writes=%d name=%q", response.Code, response.Body.String(), identities.calls, identities.displayName)
	}
}
