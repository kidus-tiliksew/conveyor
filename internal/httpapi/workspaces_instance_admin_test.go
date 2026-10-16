package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Instance administration and workspace creation
// (req-accounts-and-membership AC-1.3, AC-1.4, AC-1.5, AC-4.3; DEC-75;
// component-http-api, component-identity-membership). These cases mount the
// real router over store.NewVolatileBackend. Account provisioning admits only
// the deployment marker owner; workspace creation admits that owner or a
// workspace creator, an active account with an operator binding anywhere.

const instanceOwnerToken = "instance-owner-deployment-token"

type countingWorkspaceControl struct {
	store.WorkspaceControlStore
	creates int
	actors  []store.Actor
}

func (c *countingWorkspaceControl) CreateWorkspace(ctx context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	c.creates++
	c.actors = append(c.actors, store.ActorFromContext(ctx))
	return c.WorkspaceControlStore.CreateWorkspace(ctx, id, name, cfg)
}

type countingProvisioner struct {
	store.IdentityProvisioner
	calls int
	err   error
}

func (c *countingProvisioner) ProvisionIdentityUser(ctx context.Context, email, displayName string) (core.IdentityUser, error) {
	c.calls++
	if c.err != nil {
		return core.IdentityUser{}, c.err
	}
	return c.IdentityProvisioner.ProvisionIdentityUser(ctx, email, displayName)
}

// countingCreationAuthority wraps the real membership store, counts the two
// checks the workspace-creation predicate composes, and can fail either one.
type countingCreationAuthority struct {
	store.MembershipStore
	instanceErr, deploymentErr     error
	instanceCalls, deploymentCalls int
}

func (c *countingCreationAuthority) AuthorizeInstanceAdministration(ctx context.Context, userID string) (bool, error) {
	c.instanceCalls++
	if c.instanceErr != nil {
		return false, c.instanceErr
	}
	return c.MembershipStore.AuthorizeInstanceAdministration(ctx, userID)
}

func (c *countingCreationAuthority) AuthorizeDeployment(ctx context.Context, userID string, capability core.Capability) (bool, error) {
	c.deploymentCalls++
	if c.deploymentErr != nil {
		return false, c.deploymentErr
	}
	return c.MembershipStore.AuthorizeDeployment(ctx, userID, capability)
}

// deactivatingVerifier authenticates a live credential through the real store
// and then, synchronously and before returning it, deactivates the
// credential's owner. The request therefore passes authentication and reaches
// admission as an account that is no longer active, without weakening
// credential validation for an account that was already deactivated.
type deactivatingVerifier struct {
	CredentialVerifier
	deactivate func(userID string)
}

func (d deactivatingVerifier) VerifyCredential(ctx context.Context, value string) (core.AuthenticatedCredential, error) {
	credential, err := d.CredentialVerifier.VerifyCredential(ctx, value)
	if err == nil {
		d.deactivate(credential.OwnerUserID)
	}
	return credential, err
}

type instanceAdminHarness struct {
	t           *testing.T
	st          store.Backend
	server      *Server
	control     *countingWorkspaceControl
	provisioner *countingProvisioner
	queued      int
	owner       core.IdentityUser
	ownerPAT    string
	session     string
	// nonowners maps every nonowner account to its personal access token,
	// users to its account, and sessions to a dashboard session value.
	nonowners map[string]string
	users     map[string]core.IdentityUser
	sessions  map[string]string
	agent     string
	worker    string
	// operatorAgent and operatorWorker belong to alpha-operator, a workspace
	// creator, rather than to the deployment owner.
	operatorAgent  string
	operatorWorker string
}

// creatorNames are the nonowner accounts holding an operator binding: each is
// a workspace creator. lowerRoleNames hold only a lower binding anywhere.
var (
	creatorNames   = []string{"alpha-operator", "beta-operator"}
	lowerRoleNames = []string{"alpha-viewer", "alpha-executor", "alpha-contributor", "alpha-maintainer"}
)

// newInstanceAdminHarness builds a deployment whose owner holds no binding:
// both workspaces are created by the system actor without a credential.
func newInstanceAdminHarness(t *testing.T) *instanceAdminHarness {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	// Fixture setup writes events, so it binds the explicit system actor
	// (component-persistence, Actor context).
	ctx := store.WithActor(t.Context(), store.SystemActor())
	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Instance", Email: "owner@example.test", DisplayName: "Owner"}, instanceOwnerToken); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, instanceOwnerToken)
	if err != nil {
		t.Fatal(err)
	}
	system := store.WithActor(ctx, store.Actor{ID: "system", Role: core.ActorSystem})
	for _, workspace := range []string{"alpha", "beta"} {
		if _, err := st.CreateWorkspace(store.WithWorkspace(system, workspace), workspace, strings.ToUpper(workspace[:1])+workspace[1:], &config.Config{Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
	}
	if listed, err := st.ListWorkspacesForUser(ctx, owner.ID); err != nil || len(listed) != 0 {
		t.Fatalf("owner must start without bindings: %+v err=%v", listed, err)
	}
	granter := store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "fixture", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	h := &instanceAdminHarness{t: t, st: st, owner: owner, nonowners: map[string]string{}, users: map[string]core.IdentityUser{}, sessions: map[string]string{}}
	member := func(name, workspace string, role core.WorkspaceRole) {
		user, err := st.ProvisionIdentityUser(granter, name+"@example.test", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.GrantWorkspaceRole(granter, user.Email, workspace, role); err != nil {
			t.Fatal(err)
		}
		issued, err := st.IssueOwnPersonalAccessToken(ctx, user.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		link, err := st.IssueSignInLink(ctx, user.Email)
		if err != nil {
			t.Fatal(err)
		}
		session, _, err := st.RedeemSignInLink(ctx, link.Value)
		if err != nil {
			t.Fatal(err)
		}
		h.nonowners[name] = issued.Value
		h.users[name] = user
		h.sessions[name] = session.Value
	}
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator} {
		member("alpha-"+string(role), "alpha", role)
	}
	member("beta-operator", "beta", core.WorkspaceRoleOperator)
	pat, err := st.IssueOwnPersonalAccessToken(ctx, owner.ID, "owner user-scope token")
	if err != nil {
		t.Fatal(err)
	}
	h.ownerPAT = pat.Value
	link, err := st.IssueSignInLink(ctx, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := st.RedeemSignInLink(ctx, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	h.session = session.Value
	agent, err := st.IssueAgentCredential(ctx, owner.ID, "owner agent")
	if err != nil {
		t.Fatal(err)
	}
	h.agent = agent.Value
	operatorAgent, err := st.IssueAgentCredential(ctx, h.users["alpha-operator"].ID, "operator agent")
	if err != nil {
		t.Fatal(err)
	}
	h.operatorAgent = operatorAgent.Value
	for _, worker := range []struct{ id, secret, owner string }{
		{"worker-owner", "cv_wkr_instance_admin_fixture", owner.ID},
		{"worker-operator", "cv_wkr_instance_admin_operator", h.users["alpha-operator"].ID},
	} {
		sum := sha256.Sum256([]byte(worker.secret))
		if err := st.CreateWorker(store.WithWorkspace(system, "alpha"), core.Worker{ID: worker.id, Workspace: "alpha", OwnerUserID: worker.owner, Name: worker.id, CredentialHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	h.worker, h.operatorWorker = "cv_wkr_instance_admin_fixture", "cv_wkr_instance_admin_operator"
	h.control = &countingWorkspaceControl{WorkspaceControlStore: st}
	h.provisioner = &countingProvisioner{IdentityProvisioner: st}
	server := NewServer(st)
	server.Workspaces = h.control
	server.IdentityProvisioner = h.provisioner
	server.Deployment = &config.Config{}
	server.EnsureWorkspaceQueues = func(string, *config.Config) error { h.queued++; return nil }
	h.server = server
	return h
}

type instanceRequest struct {
	method, path, body string
	bearer, session    string
	headers            map[string]string
}

// sessionProof is the CSRF and origin proof a dashboard-session mutation
// must carry.
func sessionProof() map[string]string {
	return map[string]string{"X-Conveyor-CSRF": "1", "Origin": "http://example.com"}
}

// asCredential sends request under the named account's PAT or dashboard
// session; "owner" names the deployment owner.
func (h *instanceAdminHarness) asCredential(name, method string, request instanceRequest) instanceRequest {
	h.t.Helper()
	pat, session := h.nonowners[name], h.sessions[name]
	if name == "owner" {
		pat, session = h.ownerPAT, h.session
	}
	if pat == "" || session == "" {
		h.t.Fatalf("unknown fixture account %q", name)
	}
	if method == "pat" {
		request.bearer = pat
		return request
	}
	request.session = session
	headers := sessionProof()
	for key, value := range request.headers {
		headers[key] = value
	}
	request.headers = headers
	return request
}

func (h *instanceAdminHarness) do(request instanceRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
	r.Header.Set("Content-Type", "application/json")
	if request.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+request.bearer)
	}
	if request.session != "" {
		r.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: request.session})
	}
	for key, value := range request.headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(w, r)
	return w
}

func (h *instanceAdminHarness) sideEffects() int {
	return h.control.creates + h.provisioner.calls + h.queued
}

// role reads the account's binding in workspace, or "" without one.
func (h *instanceAdminHarness) role(userID, workspace string) core.WorkspaceRole {
	h.t.Helper()
	caller, err := h.st.GetCallerIdentity(h.t.Context(), userID, workspace)
	if errors.Is(err, store.ErrNotFound) {
		return ""
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return caller.Role
}

// deactivate closes an account through the volatile backend's native seam.
func (h *instanceAdminHarness) deactivate(userID string) {
	h.t.Helper()
	deactivator, ok := h.st.(interface {
		DeactivateIdentityUser(context.Context, string) (core.IdentityUser, error)
	})
	if !ok {
		h.t.Fatal("volatile backend lacks account deactivation")
	}
	if _, err := deactivator.DeactivateIdentityUser(store.WithActor(context.Background(), store.SystemActor()), userID); err != nil {
		h.t.Fatal(err)
	}
}

const uniformNotFound = "{\"error\":\"workspace_not_found\",\"message\":\"workspace not found\"}\n"

const workspaceConflict = "{\"error\":\"workspace_conflict\",\"message\":\"workspace could not be created\"}\n"

// refusedVariants are request shapes an authenticated refused caller sends:
// workspace context, a spoofed actor, malformed and colliding bodies, and an
// unused ID. None may change the uniform 404 or reach a handler.
func refusedVariants(ownerID string) []instanceRequest {
	return []instanceRequest{
		{},
		{path: "?workspace_id=alpha"},
		{headers: map[string]string{"X-Workspace-ID": "alpha"}},
		{headers: map[string]string{"X-Conveyor-Actor": "user:" + ownerID}},
		{body: "{"},
		{body: `{"email":"owner@example.test","display_name":"Owner"}`},
		{body: `{"id":"alpha","name":"Alpha"}`},
		{body: `{"id":"missing-` + core.NewTaskID()[:6] + `","name":"Missing"}`},
	}
}

func TestInstanceAdministrationRouteAdmission(t *testing.T) {
	routes := []struct{ path, body string }{
		{"/v1/users", `{"email":"new-person@example.test","display_name":"New Person"}`},
		{"/v1/workspaces", `{"id":"gamma","name":"Gamma"}`},
	}
	t.Run("OwnerCredentialsWithoutBindingsOrOperatorScope", func(t *testing.T) {
		for index, credential := range []string{"pat", "session"} {
			h := newInstanceAdminHarness(t)
			for _, route := range routes {
				request := h.asCredential("owner", credential, instanceRequest{method: http.MethodPost, path: route.path, body: strings.ReplaceAll(route.body, "gamma", "gamma"+string(rune('a'+index)))})
				response := h.do(request)
				want := http.StatusOK
				if route.path == "/v1/workspaces" {
					want = http.StatusCreated
				}
				if response.Code != want {
					t.Fatalf("%s %s status=%d body=%s", credential, route.path, response.Code, response.Body.String())
				}
			}
		}
	})
	t.Run("WorkspaceCreatorsCreateButCannotProvision", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		for _, name := range creatorNames {
			for _, credential := range []string{"pat", "session"} {
				// Neither the query nor the header names a workspace the caller
				// is bound in: creation reads no workspace context.
				id := "created-" + name + "-" + credential
				request := h.asCredential(name, credential, instanceRequest{method: http.MethodPost, path: "/v1/workspaces?workspace_id=gamma", body: `{"id":"` + id + `","name":"Created ` + name + " " + credential + `"}`,
					headers: map[string]string{"X-Workspace-ID": "gamma"}})
				response := h.do(request)
				if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"id":"`+id+`"`) {
					t.Fatalf("%s %s creation status=%d body=%s", name, credential, response.Code, response.Body.String())
				}
				if role := h.role(h.users[name].ID, id); role != core.WorkspaceRoleOperator {
					t.Fatalf("%s %s creator binding=%q", name, credential, role)
				}
				if role := h.role(h.owner.ID, id); role != "" {
					t.Fatalf("creation by %s bound the owner as %q", name, role)
				}
			}
		}
		creates, queued := h.control.creates, h.queued
		for _, name := range creatorNames {
			for _, credential := range []string{"pat", "session"} {
				for _, variant := range refusedVariants(h.owner.ID) {
					body := routes[0].body
					if variant.body != "" {
						body = variant.body
					}
					request := h.asCredential(name, credential, instanceRequest{method: http.MethodPost, path: "/v1/users" + variant.path, body: body, headers: variant.headers})
					if response := h.do(request); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
						t.Fatalf("%s %s provisioning%s body=%q status=%d response=%q", name, credential, variant.path, body, response.Code, response.Body.String())
					}
				}
			}
		}
		if h.provisioner.calls != 0 || h.control.creates != creates || h.queued != queued {
			t.Fatalf("refused provisioning reached the store: provisions=%d creates=%d queued=%d", h.provisioner.calls, h.control.creates-creates, h.queued-queued)
		}
	})
	t.Run("LowerRolesReceiveUniformNotFoundBeforeAnyEffect", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		for _, name := range lowerRoleNames {
			for _, credential := range []string{"pat", "session"} {
				for _, route := range routes {
					for _, variant := range refusedVariants(h.owner.ID) {
						body := route.body
						if variant.body != "" {
							body = variant.body
						}
						request := h.asCredential(name, credential, instanceRequest{method: http.MethodPost, path: route.path + variant.path, body: body, headers: variant.headers})
						if response := h.do(request); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
							t.Fatalf("%s %s %s%s body=%q status=%d response=%q", name, credential, route.path, variant.path, body, response.Code, response.Body.String())
						}
					}
				}
			}
		}
		if effects := h.sideEffects(); effects != 0 {
			t.Fatalf("refused lower roles caused %d store or queue calls", effects)
		}
	})
	t.Run("CredentialClassRefusals", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		for name, request := range map[string]instanceRequest{
			"missing":         {},
			"invalid":         {bearer: "not-a-credential"},
			"owner agent":     {bearer: h.agent},
			"owner worker":    {bearer: h.worker},
			"operator agent":  {bearer: h.operatorAgent},
			"operator worker": {bearer: h.operatorWorker},
		} {
			for _, route := range routes {
				for _, body := range []string{route.body, "{"} {
					request.method, request.path, request.body = http.MethodPost, route.path, body
					if response := h.do(request); response.Code != http.StatusUnauthorized {
						t.Fatalf("%s %s body=%q status=%d body=%s", name, route.path, body, response.Code, response.Body.String())
					}
				}
			}
		}
		for _, name := range []string{"owner", "alpha-operator"} {
			for proof, headers := range map[string]map[string]string{
				"missing CSRF":   {"Origin": "http://example.com"},
				"foreign origin": {"X-Conveyor-CSRF": "1", "Origin": "http://attacker.test"},
			} {
				session := h.session
				if name != "owner" {
					session = h.sessions[name]
				}
				for _, route := range routes {
					if response := h.do(instanceRequest{method: http.MethodPost, path: route.path, body: route.body, session: session, headers: headers}); response.Code != http.StatusForbidden {
						t.Fatalf("%s session %s %s status=%d body=%s", name, proof, route.path, response.Code, response.Body.String())
					}
				}
			}
		}
		if effects := h.sideEffects(); effects != 0 {
			t.Fatalf("credential refusals caused %d store or queue calls", effects)
		}
	})
	t.Run("DeactivatedOperatorReceivesNotFound", func(t *testing.T) {
		// A credential of an account that is already deactivated fails
		// authentication with 401; the admission 404 does not replace it.
		h := newInstanceAdminHarness(t)
		h.deactivate(h.users["beta-operator"].ID)
		for _, route := range routes {
			if response := h.do(instanceRequest{method: http.MethodPost, path: route.path, body: route.body, bearer: h.nonowners["beta-operator"]}); response.Code != http.StatusUnauthorized {
				t.Fatalf("already deactivated %s status=%d body=%s", route.path, response.Code, response.Body.String())
			}
		}
		// An operator deactivated after its credential authenticated reaches
		// admission as an inactive account whose remaining binding admits
		// nothing. Each route gets a fresh deployment, because the account
		// stays deactivated after its first request.
		for _, route := range routes {
			h := newInstanceAdminHarness(t)
			target := h.users["alpha-operator"].ID
			h.server.Credentials = deactivatingVerifier{CredentialVerifier: h.st, deactivate: func(userID string) {
				if userID == target {
					h.deactivate(userID)
				}
			}}
			if response := h.do(instanceRequest{method: http.MethodPost, path: route.path, body: route.body, bearer: h.nonowners["alpha-operator"]}); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
				t.Fatalf("deactivated former operator %s status=%d body=%q", route.path, response.Code, response.Body.String())
			}
			if effects := h.sideEffects(); effects != 0 {
				t.Fatalf("deactivated operator caused %d store or queue calls", effects)
			}
		}
	})
	t.Run("AuthorityFailsClosed", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		h.server.Memberships = nil
		for _, token := range []string{h.ownerPAT, h.nonowners["alpha-operator"]} {
			for _, route := range routes {
				if response := h.do(instanceRequest{method: http.MethodPost, path: route.path, body: route.body, bearer: token}); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
					t.Fatalf("missing authority %s status=%d body=%s", route.path, response.Code, response.Body.String())
				}
			}
		}
		// An owner-check error answers the generic 500 on both routes and
		// never falls through to the workspace-creator check.
		ownerFailure := &countingCreationAuthority{MembershipStore: h.st, instanceErr: errors.New("authority secret detail")}
		h.server.Memberships = ownerFailure
		for _, token := range []string{h.ownerPAT, h.nonowners["alpha-operator"]} {
			for _, route := range routes {
				response := h.do(instanceRequest{method: http.MethodPost, path: route.path, body: route.body, bearer: token})
				if response.Code != http.StatusInternalServerError || response.Body.String() != "internal server error\n" {
					t.Fatalf("erroring owner check %s status=%d body=%q", route.path, response.Code, response.Body.String())
				}
			}
		}
		if ownerFailure.instanceCalls != 4 || ownerFailure.deploymentCalls != 0 {
			t.Fatalf("owner-check error consulted instance=%d deployment=%d, want 4 and 0", ownerFailure.instanceCalls, ownerFailure.deploymentCalls)
		}
		// A workspace-creator-check error answers the generic 500 for a
		// nonowner; the owner short-circuits before that check.
		creatorFailure := &countingCreationAuthority{MembershipStore: h.st, deploymentErr: errors.New("authority secret detail")}
		h.server.Memberships = creatorFailure
		response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: routes[1].body, bearer: h.nonowners["alpha-operator"]})
		if response.Code != http.StatusInternalServerError || response.Body.String() != "internal server error\n" {
			t.Fatalf("erroring creator check status=%d body=%q", response.Code, response.Body.String())
		}
		if effects := h.sideEffects(); effects != 0 {
			t.Fatalf("failed authority caused %d store or queue calls", effects)
		}
		if response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: `{"id":"owner-short-circuit","name":"Owner Short Circuit"}`, bearer: h.ownerPAT}); response.Code != http.StatusCreated {
			t.Fatalf("owner with failing creator check status=%d body=%s", response.Code, response.Body.String())
		}
		if creatorFailure.instanceCalls != 2 || creatorFailure.deploymentCalls != 1 {
			t.Fatalf("checks consulted instance=%d deployment=%d, want 2 and 1", creatorFailure.instanceCalls, creatorFailure.deploymentCalls)
		}
	})
	t.Run("CredentialDerivedActorDespiteSpoofedHeader", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		creator := h.users["beta-operator"]
		for index, caller := range []struct{ token, userID string }{{h.ownerPAT, h.owner.ID}, {h.nonowners["beta-operator"], creator.ID}} {
			id := "spoofed-" + string(rune('a'+index))
			response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: `{"id":"` + id + `","name":"Spoofed ` + id + `"}`, bearer: caller.token,
				headers: map[string]string{"X-Conveyor-Actor": "user:someone-else"}})
			if response.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if len(h.control.actors) != index+1 || h.control.actors[index].ID != store.UserActorID(caller.userID) || h.control.actors[index].Role != core.ActorUser {
				t.Fatalf("creation actor=%+v want %s", h.control.actors, store.UserActorID(caller.userID))
			}
			if role := h.role(caller.userID, id); role != core.WorkspaceRoleOperator {
				t.Fatalf("creator binding=%q", role)
			}
		}
	})
}

func TestInstanceAdministrationProvisioningAcknowledgement(t *testing.T) {
	h := newInstanceAdminHarness(t)
	// Fixture setup writes events, so it binds the explicit system actor
	// (component-persistence, Actor context).
	ctx := store.WithActor(t.Context(), store.SystemActor())
	existing, err := h.st.ProvisionIdentityUser(ctx, "existing@example.test", "Existing Name")
	if err != nil {
		t.Fatal(err)
	}
	deactivated, err := h.st.ProvisionIdentityUser(ctx, "inactive@example.test", "Inactive")
	if err != nil {
		t.Fatal(err)
	}
	deactivator, ok := h.st.(interface {
		DeactivateIdentityUser(context.Context, string) (core.IdentityUser, error)
	})
	if !ok {
		t.Fatal("volatile backend lacks account deactivation")
	}
	system := store.WithActor(ctx, store.Actor{ID: "system", Role: core.ActorSystem})
	if _, err = deactivator.DeactivateIdentityUser(system, deactivated.ID); err != nil {
		t.Fatal(err)
	}
	granter := store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "fixture", OwnerUserID: h.owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(h.owner.ID), Role: core.ActorUser})
	// The deactivated account has a pending invitation the no-op must not redeem.
	if grant, err := h.st.GrantWorkspaceRole(granter, deactivated.Email, "alpha", core.WorkspaceRoleViewer); err != nil || !grant.Invitation {
		t.Fatalf("pending invitation grant=%+v err=%v", grant, err)
	}
	bodies := map[string]string{}
	for name, body := range map[string]string{
		"new":         `{"email":"  Fresh@Example.TEST ","display_name":"Fresh"}`,
		"active":      `{"email":"EXISTING@example.test","display_name":"Changed Name"}`,
		"deactivated": `{"email":"inactive@example.test","display_name":"Reactivate Me"}`,
	} {
		response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/users", body: body, bearer: h.ownerPAT})
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", name, response.Code, response.Body.String())
		}
		bodies[name] = response.Body.String()
	}
	for name, body := range bodies {
		if body != "{\"accepted\":true}\n" {
			t.Fatalf("%s acknowledgement=%q", name, body)
		}
		for _, leaked := range []string{existing.ID, deactivated.ID, "email", "display_name", "status", "created_at", "id"} {
			if strings.Contains(body, leaked) {
				t.Fatalf("%s acknowledgement leaks %q: %s", name, leaked, body)
			}
		}
	}
	if caller, err := h.st.GetCallerIdentity(ctx, existing.ID, ""); err != nil || caller.DisplayName != "Existing Name" {
		t.Fatalf("existing account changed: %+v err=%v", caller, err)
	}
	if _, err := h.st.GetCallerIdentity(ctx, deactivated.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deactivated account was reactivated: err=%v", err)
	}
	if role, err := h.st.GetCallerIdentity(ctx, deactivated.ID, "alpha"); err == nil {
		t.Fatalf("deactivated provisioning redeemed an invitation: %+v", role)
	}
	invitations, err := h.st.ListWorkspaceInvitations(ctx, "alpha")
	if err != nil || len(invitations) != 1 || invitations[0].Email != deactivated.Email {
		t.Fatalf("pending invitation after no-op=%+v err=%v", invitations, err)
	}
	calls := h.provisioner.calls
	for _, body := range []string{`{`, `{"email":"not-an-email","display_name":"X"}`, `{"email":"x@example.test","display_name":" "}`, `{"email":"x@example.test","display_name":"X","role":"operator"}`} {
		if response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/users", body: body, bearer: h.ownerPAT}); response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("validation body=%q status=%d", body, response.Code)
		}
	}
	if h.provisioner.calls != calls {
		t.Fatal("validation refusals reached the store")
	}
	h.provisioner.err = errors.New("database secret detail")
	if response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/users", body: `{"email":"x@example.test","display_name":"X"}`, bearer: h.ownerPAT}); response.Code != http.StatusInternalServerError || response.Body.String() != "internal server error\n" {
		t.Fatalf("infrastructure failure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestInstanceAdministrationWorkspaceCollisionResponse(t *testing.T) {
	h := newInstanceAdminHarness(t)
	if response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: `{"id":"taken","name":"Taken Name"}`, bearer: h.ownerPAT}); response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	// The owner and a nonowner workspace creator get the same opaque answer
	// for an ID collision, a normalized-name collision, and a collision with
	// a workspace hidden from them (AC-1.5; DEC-75(7)). beta-operator is bound
	// only in beta, so taken and alpha are hidden from it.
	for caller, token := range map[string]string{"owner": h.ownerPAT, "beta-operator": h.nonowners["beta-operator"]} {
		creates, queued := h.control.creates, h.queued
		workspaces, err := h.st.ListWorkspaces(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"id":                     `{"id":"taken","name":"Another Name"}`,
			"normalized name":        `{"id":"another","name":"  TAKEN NAME "}`,
			"hidden id":              `{"id":"alpha","name":"Not Alpha"}`,
			"hidden normalized name": `{"id":"not-alpha","name":" ALPHA"}`,
		} {
			response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: body, bearer: token})
			if response.Code != http.StatusConflict || response.Body.String() != workspaceConflict {
				t.Fatalf("%s %s collision status=%d body=%q", caller, name, response.Code, response.Body.String())
			}
		}
		after, err := h.st.ListWorkspaces(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(workspaces) {
			t.Fatalf("%s collisions added workspaces: before=%d after=%d", caller, len(workspaces), len(after))
		}
		// Each colliding request reached the store once and registered its
		// queue candidate, and none left a workspace, binding, or row behind.
		if h.control.creates-creates != 4 || h.queued-queued != 4 {
			t.Fatalf("%s collisions creates=%d queued=%d", caller, h.control.creates-creates, h.queued-queued)
		}
	}
	for _, userID := range []string{h.users["beta-operator"].ID, h.owner.ID} {
		for _, workspace := range []string{"another", "not-alpha"} {
			if role := h.role(userID, workspace); role != "" {
				t.Fatalf("collision left a %q binding in %s", role, workspace)
			}
		}
	}
	if role := h.role(h.users["beta-operator"].ID, "taken"); role != "" {
		t.Fatalf("collision granted the creator a %q binding in the hidden workspace", role)
	}
	// Lower roles are refused before the collision lookup and queue
	// registration, whether or not the ID exists.
	creates, queued := h.control.creates, h.queued
	for _, body := range []string{`{"id":"taken","name":"Hit"}`, `{"id":"never-created","name":"Miss"}`} {
		response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: body, bearer: h.nonowners["alpha-maintainer"]})
		if response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
			t.Fatalf("lower role body=%s status=%d response=%q", body, response.Code, response.Body.String())
		}
	}
	if h.control.creates != creates || h.queued != queued {
		t.Fatal("a lower role reached the collision lookup or queue registration")
	}
	if role := h.role(h.owner.ID, "taken"); role != core.WorkspaceRoleOperator {
		t.Fatalf("creator binding=%q", role)
	}
}

func TestInstanceAdministrationWorkspaceValidation(t *testing.T) {
	h := newInstanceAdminHarness(t)
	accepted := []string{
		`{"id":"a","name":"One Character Slug"}`,
		`{"id":"` + strings.Repeat("b", 63) + `","name":"Longest Slug"}`,
		`{"id":"long-name","name":"` + strings.Repeat("n", 200) + `"}`,
	}
	for _, body := range accepted {
		if response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: body, bearer: h.ownerPAT}); response.Code != http.StatusCreated {
			t.Fatalf("accepted body status=%d response=%s", response.Code, response.Body.String())
		}
	}
	creates, queued := h.control.creates, h.queued
	for _, body := range []string{
		`{"id":"` + strings.Repeat("c", 64) + `","name":"Too Long Slug"}`,
		`{"id":"too-long-name","name":"` + strings.Repeat("n", 201) + `"}`,
		`{"id":"malformed"`,
		`{"id":"routing","name":"Routing","routing":{}}`,
		`{"id":"nested","name":"Nested","document":{"repos":[{"name":"r","model":null}]}}`,
		`{"id":"empty","name":"Empty","document":{"execution":{"setups":""}}}`,
	} {
		if response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: body, bearer: h.ownerPAT}); response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("refused body=%s status=%d response=%s", body, response.Code, response.Body.String())
		}
	}
	if h.control.creates != creates || h.queued != queued {
		t.Fatalf("validation refusals persisted or registered queues: creates=%d queued=%d", h.control.creates-creates, h.queued-queued)
	}
}

// TestWorkspaceCreationProjectionMatchesAdmission proves that GET /v1/me
// can_create_workspaces and POST /v1/workspaces admission come from the same
// predicate: flipping one fixture authority between requests changes both
// together. While refused, malformed and colliding bodies get the uniform 404
// with no store or queue call; once admitted, a valid unique request creates
// the workspace. Account provisioning stays on AuthorizeInstanceAdministration
// (component-http-api, component-identity-membership).
func TestWorkspaceCreationProjectionMatchesAdmission(t *testing.T) {
	h := newInstanceAdminHarness(t)
	token := h.nonowners["alpha-operator"]
	caller, err := h.st.VerifyPersonalAccessToken(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	authority := &membershipFixture{instanceAdmins: map[string]bool{}}
	h.server.Memberships = authority
	creationRight := func() bool {
		t.Helper()
		return decodeCallerIdentityProjection(t, h.do(instanceRequest{method: http.MethodGet, path: "/v1/me", bearer: token})).creationRight(t)
	}
	post := func(path, body string) *httptest.ResponseRecorder {
		return h.do(instanceRequest{method: http.MethodPost, path: path, body: body, bearer: token})
	}

	if creationRight() {
		t.Fatal("refused caller read can_create_workspaces=true")
	}
	for _, body := range []string{"{", `{"id":"alpha","name":"Delta"}`, `{"id":"delta","name":"Alpha"}`, `{"id":"delta","name":"Delta"}`} {
		if response := post("/v1/workspaces", body); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
			t.Fatalf("refused create body=%q status=%d response=%q", body, response.Code, response.Body.String())
		}
	}
	if response := post("/v1/users", `{"email":"new-person@example.test","display_name":"New Person"}`); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
		t.Fatalf("refused provisioning status=%d response=%q", response.Code, response.Body.String())
	}
	if effects := h.sideEffects(); effects != 0 {
		t.Fatalf("refused caller caused %d store or queue calls", effects)
	}

	authority.instanceAdmins[caller.ID] = true
	if !creationRight() {
		t.Fatal("admitted caller read can_create_workspaces=false")
	}
	if response := post("/v1/workspaces", `{"id":"delta","name":"Delta"}`); response.Code != http.StatusCreated || h.control.creates != 1 {
		t.Fatalf("admitted create status=%d response=%s creates=%d", response.Code, response.Body.String(), h.control.creates)
	}
	if response := post("/v1/users", `{"email":"new-person@example.test","display_name":"New Person"}`); response.Code != http.StatusOK || h.provisioner.calls != 1 {
		t.Fatalf("provisioning status=%d response=%s calls=%d", response.Code, response.Body.String(), h.provisioner.calls)
	}

	authority.instanceAdmins[caller.ID] = false
	before := h.sideEffects()
	if creationRight() {
		t.Fatal("revoked caller read can_create_workspaces=true")
	}
	if response := post("/v1/workspaces", `{"id":"epsilon","name":"Epsilon"}`); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound || h.sideEffects() != before {
		t.Fatalf("revoked create status=%d response=%q effects=%d", response.Code, response.Body.String(), h.sideEffects()-before)
	}
	// Each request consulted the one authority exactly once: three projection
	// reads, four refused and one admitted creation, one refused and one
	// admitted provisioning, and the final refused creation.
	if authority.instanceCalls != 11 {
		t.Fatalf("instance authority calls=%d, want 11", authority.instanceCalls)
	}
}

// TestWorkspaceCreationFollowsOperatorBinding proves the creation right
// follows a real operator binding in any workspace, not the selected one.
// mixed is contributor in alpha and the second operator of beta: GET /v1/me
// serves true unscoped and scoped to alpha, and POST /v1/workspaces admits it
// (a malformed body reaches the 422 validation, so nothing is created and no
// new binding keeps the right alive). Revoking its beta binding turns the
// field false and the route's answer into the uniform 404, with no change to
// the persisted credential (DEC-75(2); component-http-api,
// component-identity-membership).
func TestWorkspaceCreationFollowsOperatorBinding(t *testing.T) {
	h := newInstanceAdminHarness(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	granter := store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "fixture", OwnerUserID: h.users["beta-operator"].ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(h.users["beta-operator"].ID), Role: core.ActorUser})
	mixed, err := h.st.ProvisionIdentityUser(granter, "mixed@example.test", "Mixed")
	if err != nil {
		t.Fatal(err)
	}
	for workspace, role := range map[string]core.WorkspaceRole{"alpha": core.WorkspaceRoleContributor, "beta": core.WorkspaceRoleOperator} {
		if _, err := h.st.GrantWorkspaceRole(granter, mixed.Email, workspace, role); err != nil {
			t.Fatal(err)
		}
	}
	issued, err := h.st.IssueOwnPersonalAccessToken(ctx, mixed.ID, "mixed")
	if err != nil {
		t.Fatal(err)
	}
	credentialBefore, err := h.st.VerifyCredential(ctx, issued.Value)
	if err != nil {
		t.Fatal(err)
	}
	creationRight := func(target string) bool {
		t.Helper()
		return decodeCallerIdentityProjection(t, h.do(instanceRequest{method: http.MethodGet, path: target, bearer: issued.Value})).creationRight(t)
	}
	malformed := func() *httptest.ResponseRecorder {
		return h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: `{"id":"malformed"`, bearer: issued.Value})
	}

	for _, target := range []string{"/v1/me", "/v1/me?workspace_id=alpha"} {
		if !creationRight(target) {
			t.Fatalf("%s: operator elsewhere read can_create_workspaces=false", target)
		}
	}
	if response := malformed(); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("admitted malformed create status=%d body=%s", response.Code, response.Body.String())
	}

	if err := h.st.RevokeWorkspaceRole(granter, mixed.ID, "beta"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/v1/me", "/v1/me?workspace_id=alpha"} {
		if creationRight(target) {
			t.Fatalf("%s: contributor-only account read can_create_workspaces=true", target)
		}
	}
	if response := malformed(); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
		t.Fatalf("revoked create status=%d body=%q", response.Code, response.Body.String())
	}
	credentialAfter, err := h.st.VerifyCredential(ctx, issued.Value)
	if err != nil {
		t.Fatal(err)
	}
	if credentialAfter.Kind != credentialBefore.Kind || credentialAfter.Scope != credentialBefore.Scope || credentialAfter.ID != credentialBefore.ID {
		t.Fatalf("credential changed: before=%+v after=%+v", credentialBefore, credentialAfter)
	}
	if effects := h.sideEffects(); effects != 0 {
		t.Fatalf("binding transition caused %d store or queue calls", effects)
	}
}
