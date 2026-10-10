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

// Instance administration (req-accounts-and-membership AC-1.3, AC-4.3;
// DEC-63; component-http-api, component-identity-membership). These cases
// mount the real router over store.NewVolatileBackend.

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

type failingInstanceAuthority struct {
	store.MembershipStore
	err error
}

func (f failingInstanceAuthority) AuthorizeInstanceAdministration(context.Context, string) (bool, error) {
	return false, f.err
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
	nonowners   map[string]string
	agent       string
	worker      string
}

// newInstanceAdminHarness builds a deployment whose owner holds no binding:
// both workspaces are created by the system actor without a credential.
func newInstanceAdminHarness(t *testing.T) *instanceAdminHarness {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	ctx := t.Context()
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
	h := &instanceAdminHarness{t: t, st: st, owner: owner, nonowners: map[string]string{}}
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
		h.nonowners[name] = issued.Value
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
	h.worker = "cv_wkr_instance_admin_fixture"
	sum := sha256.Sum256([]byte(h.worker))
	if err := st.CreateWorker(store.WithWorkspace(system, "alpha"), core.Worker{ID: "worker-owner", Workspace: "alpha", OwnerUserID: owner.ID, Name: "worker-owner", CredentialHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
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

const uniformNotFound = "{\"error\":\"workspace_not_found\",\"message\":\"workspace not found\"}\n"

func TestInstanceAdministrationRouteAdmission(t *testing.T) {
	routes := []struct{ path, body string }{
		{"/v1/users", `{"email":"new-person@example.test","display_name":"New Person"}`},
		{"/v1/workspaces", `{"id":"gamma","name":"Gamma"}`},
	}
	t.Run("OwnerCredentialsWithoutBindingsOrOperatorScope", func(t *testing.T) {
		for index, credential := range []string{"pat", "session"} {
			h := newInstanceAdminHarness(t)
			for _, route := range routes {
				request := instanceRequest{method: http.MethodPost, path: route.path, body: strings.ReplaceAll(route.body, "gamma", "gamma"+string(rune('a'+index)))}
				if credential == "pat" {
					request.bearer = h.ownerPAT
				} else {
					request.session = h.session
					request.headers = map[string]string{"X-Conveyor-CSRF": "1", "Origin": "http://example.com"}
				}
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
	t.Run("NonownersReceiveUniformNotFoundBeforeAnyEffect", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		variants := []instanceRequest{
			{},
			{path: "?workspace_id=alpha"},
			{headers: map[string]string{"X-Workspace-ID": "alpha"}},
			{headers: map[string]string{"X-Conveyor-Actor": "user:" + h.owner.ID}},
			{body: "{"},
			{body: `{"email":"owner@example.test","display_name":"Owner"}`},
			{body: `{"id":"alpha","name":"Alpha"}`},
			{body: `{"id":"missing-` + core.NewTaskID()[:6] + `","name":"Missing"}`},
		}
		for name, token := range h.nonowners {
			for _, route := range routes {
				for _, variant := range variants {
					body := route.body
					if variant.body != "" {
						body = variant.body
					}
					response := h.do(instanceRequest{method: http.MethodPost, path: route.path + variant.path, body: body, bearer: token, headers: variant.headers})
					if response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
						t.Fatalf("%s %s%s body=%q status=%d response=%q", name, route.path, variant.path, body, response.Code, response.Body.String())
					}
				}
			}
		}
		if effects := h.sideEffects(); effects != 0 {
			t.Fatalf("refused nonowners caused %d store or queue calls", effects)
		}
	})
	t.Run("CredentialClassRefusals", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		for name, request := range map[string]instanceRequest{
			"missing":      {},
			"invalid":      {bearer: "not-a-credential"},
			"owner agent":  {bearer: h.agent},
			"owner worker": {bearer: h.worker},
		} {
			for _, route := range routes {
				request.method, request.path, request.body = http.MethodPost, route.path, route.body
				if response := h.do(request); response.Code != http.StatusUnauthorized {
					t.Fatalf("%s %s status=%d body=%s", name, route.path, response.Code, response.Body.String())
				}
			}
		}
		for name, headers := range map[string]map[string]string{
			"missing CSRF":   {"Origin": "http://example.com"},
			"foreign origin": {"X-Conveyor-CSRF": "1", "Origin": "http://attacker.test"},
		} {
			for _, route := range routes {
				if response := h.do(instanceRequest{method: http.MethodPost, path: route.path, body: route.body, session: h.session, headers: headers}); response.Code != http.StatusForbidden {
					t.Fatalf("owner session %s %s status=%d body=%s", name, route.path, response.Code, response.Body.String())
				}
			}
		}
		if effects := h.sideEffects(); effects != 0 {
			t.Fatalf("credential refusals caused %d store or queue calls", effects)
		}
	})
	t.Run("AuthorityFailsClosed", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		h.server.Memberships = nil
		for _, route := range routes {
			if response := h.do(instanceRequest{method: http.MethodPost, path: route.path, body: route.body, bearer: h.ownerPAT}); response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
				t.Fatalf("missing authority %s status=%d body=%s", route.path, response.Code, response.Body.String())
			}
		}
		h.server.Memberships = failingInstanceAuthority{MembershipStore: h.st, err: errors.New("authority secret detail")}
		for _, route := range routes {
			response := h.do(instanceRequest{method: http.MethodPost, path: route.path, body: route.body, bearer: h.ownerPAT})
			if response.Code != http.StatusInternalServerError || response.Body.String() != "internal server error\n" {
				t.Fatalf("erroring authority %s status=%d body=%q", route.path, response.Code, response.Body.String())
			}
		}
		if effects := h.sideEffects(); effects != 0 {
			t.Fatalf("failed authority caused %d store or queue calls", effects)
		}
	})
	t.Run("CredentialDerivedActorDespiteSpoofedHeader", func(t *testing.T) {
		h := newInstanceAdminHarness(t)
		response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: `{"id":"spoofed","name":"Spoofed"}`, bearer: h.ownerPAT,
			headers: map[string]string{"X-Conveyor-Actor": "user:someone-else"}})
		if response.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		if len(h.control.actors) != 1 || h.control.actors[0].ID != store.UserActorID(h.owner.ID) || h.control.actors[0].Role != core.ActorUser {
			t.Fatalf("creation actor=%+v want %s", h.control.actors, store.UserActorID(h.owner.ID))
		}
		caller, err := h.st.GetCallerIdentity(t.Context(), h.owner.ID, "spoofed")
		if err != nil || caller.Role != core.WorkspaceRoleOperator {
			t.Fatalf("creator binding=%+v err=%v", caller, err)
		}
	})
}

func TestInstanceAdministrationProvisioningAcknowledgement(t *testing.T) {
	h := newInstanceAdminHarness(t)
	ctx := t.Context()
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
	conflict := "{\"error\":\"workspace_conflict\",\"message\":\"workspace could not be created\"}\n"
	for name, body := range map[string]string{
		"id":              `{"id":"taken","name":"Another Name"}`,
		"normalized name": `{"id":"another","name":"  TAKEN NAME "}`,
		"hidden id":       `{"id":"beta","name":"Not Beta"}`,
	} {
		response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: body, bearer: h.ownerPAT})
		if response.Code != http.StatusConflict || response.Body.String() != conflict {
			t.Fatalf("%s collision status=%d body=%q", name, response.Code, response.Body.String())
		}
	}
	creates, queued := h.control.creates, h.queued
	for _, body := range []string{`{"id":"taken","name":"Hit"}`, `{"id":"never-created","name":"Miss"}`} {
		response := h.do(instanceRequest{method: http.MethodPost, path: "/v1/workspaces", body: body, bearer: h.nonowners["alpha-operator"]})
		if response.Code != http.StatusNotFound || response.Body.String() != uniformNotFound {
			t.Fatalf("nonowner body=%s status=%d response=%q", body, response.Code, response.Body.String())
		}
	}
	if h.control.creates != creates || h.queued != queued {
		t.Fatal("nonowner reached the collision lookup or queue registration")
	}
	if caller, err := h.st.GetCallerIdentity(t.Context(), h.owner.ID, "taken"); err != nil || caller.Role != core.WorkspaceRoleOperator {
		t.Fatalf("creator binding=%+v err=%v", caller, err)
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
