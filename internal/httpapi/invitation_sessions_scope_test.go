package httpapi

// Scoped invitation resend, invitation-only grant links, and maintainer
// refusals for membership mutations (req-accounts-and-membership AC-2.1,
// AC-2.6, AC-3.1, AC-4.2, AC-4.3; req-invitations-and-sign-in REQ-1, AC-1.3;
// component-identity-membership).

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// countingInvitationSessions wraps a real store and counts every issuance and
// delivery the HTTP layer requests.
type countingInvitationSessions struct {
	store.InvitationSessionStore
	scoped     []string
	attempts   int
	unscoped   int
	deliveries []string
}

func (c *countingInvitationSessions) IssueSignInLink(ctx context.Context, email string) (core.IssuedSignInLink, error) {
	c.unscoped++
	return c.InvitationSessionStore.IssueSignInLink(ctx, email)
}

func (c *countingInvitationSessions) IssueInvitationSignInLink(ctx context.Context, workspaceID, email string, purpose store.SignInLinkPurpose) (core.IssuedSignInLink, error) {
	c.attempts++
	issued, err := c.InvitationSessionStore.IssueInvitationSignInLink(ctx, workspaceID, email, purpose)
	if err == nil {
		c.scoped = append(c.scoped, workspaceID+"|"+issued.Email+"|"+string(purpose))
	}
	return issued, err
}

func (c *countingInvitationSessions) RecordInvitationDelivery(ctx context.Context, email, outcome string) error {
	c.deliveries = append(c.deliveries, outcome)
	return c.InvitationSessionStore.RecordInvitationDelivery(ctx, email, outcome)
}

type scopedInvitationHarness struct {
	t        *testing.T
	st       store.Backend
	server   *Server
	sessions *countingInvitationSessions
	owner    core.IdentityUser
	ownerCtx context.Context
}

const scopedOwnerToken = "scoped-owner-legacy-token"

func newScopedInvitationHarness(t *testing.T) *scopedInvitationHarness {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	for _, workspace := range []string{"alpha", "beta"} {
		if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace), &config.Config{Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.BootstrapIdentity(store.WithActor(t.Context(), store.SystemActor()), config.FirstOperatorIdentity{OrganizationName: "Scoped", Email: "owner@example.test", DisplayName: "Owner"}, scopedOwnerToken); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(store.WithActor(t.Context(), store.SystemActor()), scopedOwnerToken)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := store.WithActor(store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	server := NewServer(st)
	server.Workspaces = st
	sessions := &countingInvitationSessions{InvitationSessionStore: st}
	server.InvitationSessions = sessions
	return &scopedInvitationHarness{t: t, st: st, server: server, sessions: sessions, owner: owner, ownerCtx: ownerCtx}
}

func (h *scopedInvitationHarness) grant(email, workspace string, role core.WorkspaceRole) {
	h.t.Helper()
	if _, err := h.st.GrantWorkspaceRole(h.ownerCtx, email, workspace, role); err != nil {
		h.t.Fatalf("grant %s in %s: %v", email, workspace, err)
	}
}

func (h *scopedInvitationHarness) call(method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+scopedOwnerToken)
	request.Header.Set("Content-Type", "application/json")
	for key, value := range header {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	return response
}

func (h *scopedInvitationHarness) workspaceEvents(workspace, kind string) []core.Event {
	h.t.Helper()
	events, err := h.st.ListEvents(store.WithWorkspace(store.WithActor(h.t.Context(), store.SystemActor()), workspace), "")
	if err != nil {
		h.t.Fatal(err)
	}
	var out []core.Event
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func TestWorkspaceInvitationResendScope(t *testing.T) {
	h := newScopedInvitationHarness(t)
	victim, err := h.st.ProvisionIdentityUser(h.ownerCtx, "victim@example.test", "Victim")
	if err != nil {
		t.Fatal(err)
	}
	h.grant(victim.Email, "beta", core.WorkspaceRoleOperator)
	if _, err := h.st.ProvisionIdentityUser(h.ownerCtx, "bound@example.test", "Bound"); err != nil {
		t.Fatal(err)
	}
	h.grant("bound@example.test", "alpha", core.WorkspaceRoleViewer)
	h.grant("betaonly@example.test", "beta", core.WorkspaceRoleViewer)
	h.grant("revoked@example.test", "alpha", core.WorkspaceRoleViewer)
	if err := h.st.RevokeWorkspaceInvitation(h.ownerCtx, "revoked@example.test", "alpha"); err != nil {
		t.Fatal(err)
	}
	h.grant("redeemed@example.test", "alpha", core.WorkspaceRoleViewer)
	redeemLink, err := h.st.IssueInvitationSignInLink(h.ownerCtx, "alpha", "redeemed@example.test", store.SignInLinkInvitation)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.st.RedeemSignInLink(store.WithActor(h.t.Context(), store.SystemActor()), redeemLink.Value); err != nil {
		t.Fatal(err)
	}

	for _, email := range []string{
		"victim@example.test",   // active account bound only in another workspace
		"bound@example.test",    // account already bound here, no invitation
		"betaonly@example.test", // invitation pending only in another workspace
		"unknown@example.test",  // no account and no invitation anywhere
		"revoked@example.test",  // invitation revoked
		"redeemed@example.test", // invitation consumed by redemption
		"owner%40example.test",  // the caller's own account
		"not-an-address",        // malformed address
	} {
		t.Run(email, func(t *testing.T) {
			before := len(h.sessions.scoped)
			deliveries := len(h.sessions.deliveries)
			attempts := h.sessions.attempts
			response := h.call(http.MethodPost, "/v1/workspaces/alpha/invitations/"+email+"/resend", "", nil)
			// The operator passed authorization; the scoped store refused.
			if h.sessions.attempts != attempts+1 {
				t.Fatalf("resend did not reach scoped issuance: attempts=%d", h.sessions.attempts)
			}
			if response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "sign_in_url") || len(h.sessions.scoped) != before || len(h.sessions.deliveries) != deliveries {
				t.Fatalf("refusal issued or delivered: scoped=%v deliveries=%v", h.sessions.scoped, h.sessions.deliveries)
			}
		})
	}
	if events := h.workspaceEvents("alpha", "workspace.invitation_resent"); len(events) != 0 {
		t.Fatalf("refused resends appended %d events", len(events))
	}
	if h.sessions.unscoped != 0 {
		t.Fatalf("HTTP resend reached unscoped issuance %d times", h.sessions.unscoped)
	}

	// A pending invitation in the request workspace succeeds through an
	// encoded, mixed-case path, and a spoofed actor header is ignored.
	h.grant("Pending@Example.test", "alpha", core.WorkspaceRoleExecutor)
	response := h.call(http.MethodPost, "/v1/workspaces/alpha/invitations/PENDING%40example.TEST/resend", "", map[string]string{"X-Conveyor-Actor": "user:" + victim.ID})
	if response.Code != http.StatusOK {
		t.Fatalf("pending resend status=%d body=%q", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["delivery"] != "fallback" || !strings.Contains(result["sign_in_url"].(string), "/sign-in#token=cv_signin_") || result["email"] != "pending@example.test" {
		t.Fatalf("pending resend result=%v", result)
	}
	if got := h.sessions.scoped; len(got) == 0 || got[len(got)-1] != "alpha|pending@example.test|resend" {
		t.Fatalf("scoped issuance=%v", got)
	}
	events := h.workspaceEvents("alpha", "workspace.invitation_resent")
	if len(events) != 1 {
		t.Fatalf("resent events=%d", len(events))
	}
	if events[0].ActorID != store.UserActorID(h.owner.ID) || events[0].ActorRole != core.ActorUser {
		t.Fatalf("resent actor=%s/%s want credential owner %s", events[0].ActorID, events[0].ActorRole, h.owner.ID)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["workspace_id"] != "alpha" || payload["email"] != "pending@example.test" || payload["resent_by"] != h.owner.ID || len(payload) != 3 {
		t.Fatalf("resent payload=%v", payload)
	}
	if strings.Contains(string(events[0].Payload), "cv_signin_") || strings.Contains(string(events[0].Payload), "sign-in") {
		t.Fatalf("resent payload carries link material: %s", events[0].Payload)
	}
}

func TestWorkspaceGrantExistingAccountDoesNotIssueLink(t *testing.T) {
	for _, smtp := range []bool{false, true} {
		name := "without mail relay"
		if smtp {
			name = "with mail relay configured"
		}
		t.Run(name, func(t *testing.T) {
			h := newScopedInvitationHarness(t)
			if smtp {
				// Any send to this closed port would fail; the assertion is
				// that no issuance or delivery is attempted at all.
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				host, port, _ := net.SplitHostPort(listener.Addr().String())
				listener.Close()
				h.server.InvitationDelivery = config.InvitationDelivery{Host: host, Port: port, From: "conveyor@example.test", PublicURL: "https://conveyor.example"}
			}
			existing, err := h.st.ProvisionIdentityUser(h.ownerCtx, "existing@example.test", "Existing")
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct {
				email string
				role  core.WorkspaceRole
			}{
				{"Existing@Example.test", core.WorkspaceRoleViewer},   // first binding in alpha
				{"existing@example.test", core.WorkspaceRoleExecutor}, // role change
			} {
				response := h.call(http.MethodPost, "/v1/workspaces/alpha/members", `{"email":"`+step.email+`","role":"`+string(step.role)+`"}`, nil)
				if response.Code != http.StatusCreated {
					t.Fatalf("grant status=%d body=%q", response.Code, response.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if _, ok := body["sign_in_url"]; ok {
					t.Fatalf("existing-account grant returned a link: %v", body)
				}
				if _, ok := body["delivery"]; ok {
					t.Fatalf("existing-account grant reported delivery: %v", body)
				}
				if _, ok := body["invitation"]; ok || body["email"] != "existing@example.test" || body["role"] != string(step.role) || len(body) != 2 {
					t.Fatalf("grant body=%v", body)
				}
				caller, err := h.st.GetCallerIdentity(store.WithActor(h.t.Context(), store.SystemActor()), existing.ID, "alpha")
				if err != nil || caller.Role != step.role {
					t.Fatalf("binding role=%q err=%v want %q", caller.Role, err, step.role)
				}
			}
			if len(h.sessions.scoped) != 0 || h.sessions.unscoped != 0 || len(h.sessions.deliveries) != 0 {
				t.Fatalf("existing-account grants issued or delivered: scoped=%v unscoped=%d deliveries=%v", h.sessions.scoped, h.sessions.unscoped, h.sessions.deliveries)
			}

			// An unknown email still receives its initial invitation link,
			// which is not recorded as a resend.
			response := h.call(http.MethodPost, "/v1/workspaces/alpha/members", `{"email":"newcomer@example.test","role":"viewer"}`, nil)
			if response.Code != http.StatusCreated {
				t.Fatalf("invitation grant status=%d body=%q", response.Code, response.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			wantDelivery := "fallback"
			if smtp {
				wantDelivery = "failed"
			}
			if body["delivery"] != wantDelivery {
				t.Fatalf("invitation grant body=%v want delivery %s", body, wantDelivery)
			}
			if _, hasURL := body["sign_in_url"]; hasURL == smtp {
				t.Fatalf("invitation grant URL presence=%v with smtp=%v: %v", hasURL, smtp, body)
			}
			if got := h.sessions.scoped; len(got) != 1 || got[0] != "alpha|newcomer@example.test|invitation" {
				t.Fatalf("scoped issuance=%v", got)
			}
			if events := h.workspaceEvents("alpha", "workspace.invitation_resent"); len(events) != 0 {
				t.Fatalf("initial invitation recorded %d resend events", len(events))
			}
		})
	}
}

// mutationRecordingMemberships fails the test if a refused request reaches a
// membership mutation.
type mutationRecordingMemberships struct {
	*membershipFixture
	mutations []string
}

func (m *mutationRecordingMemberships) GrantWorkspaceRole(ctx context.Context, email, workspaceID string, role core.WorkspaceRole) (core.MembershipGrant, error) {
	m.mutations = append(m.mutations, "grant")
	return m.membershipFixture.GrantWorkspaceRole(ctx, email, workspaceID, role)
}

func (m *mutationRecordingMemberships) RevokeWorkspaceRole(ctx context.Context, userID, workspaceID string) error {
	m.mutations = append(m.mutations, "revoke-member")
	return m.membershipFixture.RevokeWorkspaceRole(ctx, userID, workspaceID)
}

func (m *mutationRecordingMemberships) RevokeWorkspaceInvitation(ctx context.Context, email, workspaceID string) error {
	m.mutations = append(m.mutations, "revoke-invitation")
	return m.membershipFixture.RevokeWorkspaceInvitation(ctx, email, workspaceID)
}

func (m *mutationRecordingMemberships) ListWorkspaceInvitations(ctx context.Context, workspaceID string) ([]core.WorkspaceInvitation, error) {
	m.mutations = append(m.mutations, "list-invitations")
	return m.membershipFixture.ListWorkspaceInvitations(ctx, workspaceID)
}

func TestMaintainerMembershipMutationRefusals(t *testing.T) {
	fixture := &mutationRecordingMemberships{membershipFixture: &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"maintainer": {"alpha": core.WorkspaceRoleMaintainer},
			"operator":   {"alpha": core.WorkspaceRoleOperator},
		},
		invitations: []core.WorkspaceInvitation{{WorkspaceID: "alpha", Email: "invitee@example.test"}},
	}}
	sessions := &invitationSessionFixture{}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture.membershipFixture, fixture
	server.InvitationSessions = sessions
	server.Credentials = staticCredentialVerifier{
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
	routes := []struct {
		name, method, path, body, mutation string
	}{
		{"grant", http.MethodPost, "/v1/workspaces/alpha/members", `{"email":"invitee@example.test","role":"viewer"}`, "grant"},
		{"member revoke", http.MethodDelete, "/v1/workspaces/alpha/members/usr_member", "", "revoke-member"},
		{"invitation revoke", http.MethodDelete, "/v1/workspaces/alpha/invitations/invitee@example.test", "", "revoke-invitation"},
		{"invitation resend", http.MethodPost, "/v1/workspaces/alpha/invitations/invitee@example.test/resend", "", ""},
		{"invitation listing", http.MethodGet, "/v1/workspaces/alpha/invitations", "", "list-invitations"},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			fixture.capabilityCalls, fixture.mutations, sessions.scoped, sessions.issued, sessions.deliveries = nil, nil, nil, nil, nil
			response := call(route.method, route.path, "maintainer-token", route.body)
			if response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
				t.Fatalf("maintainer status=%d body=%q", response.Code, response.Body.String())
			}
			if len(fixture.capabilityCalls) == 0 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != core.CapabilityManageMembership {
				t.Fatalf("maintainer capability calls=%v", fixture.capabilityCalls)
			}
			if len(fixture.mutations) != 0 || len(sessions.scoped) != 0 || len(sessions.issued) != 0 || len(sessions.deliveries) != 0 {
				t.Fatalf("maintainer reached mutation=%v issuance=%v deliveries=%v", fixture.mutations, sessions.scoped, sessions.deliveries)
			}

			// The operator passes the same capability check and reaches the
			// handler.
			fixture.capabilityCalls, fixture.mutations, sessions.scoped = nil, nil, nil
			response = call(route.method, route.path, "operator-token", route.body)
			if response.Code == http.StatusNotFound && response.Body.String() == canonicalWorkspaceNotFoundBody() && route.mutation != "" {
				t.Fatalf("operator refused: status=%d body=%q", response.Code, response.Body.String())
			}
			if len(fixture.capabilityCalls) == 0 || fixture.capabilityCalls[len(fixture.capabilityCalls)-1] != core.CapabilityManageMembership {
				t.Fatalf("operator capability calls=%v", fixture.capabilityCalls)
			}
			if route.mutation != "" && (len(fixture.mutations) != 1 || fixture.mutations[0] != route.mutation) {
				t.Fatalf("operator mutations=%v want %s", fixture.mutations, route.mutation)
			}
			if route.mutation == "" && (len(sessions.scoped) != 1 || sessions.scoped[0] != "alpha|invitee@example.test|resend") {
				t.Fatalf("operator resend issuance=%v", sessions.scoped)
			}
		})
	}
}
