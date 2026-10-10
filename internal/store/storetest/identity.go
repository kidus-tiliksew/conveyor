package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func bootstrapOwner(t *testing.T, x Fixture) (context.Context, core.IdentityUser) {
	t.Helper()
	_, err := x.Backend.BootstrapIdentity(x.Context, config.FirstOperatorIdentity{OrganizationName: "Conformance", Email: "owner@example.test", DisplayName: "Owner"}, "conformance-bootstrap")
	requireOK(t, err)
	owner, err := x.Backend.VerifyPersonalAccessToken(x.Context, "conformance-bootstrap")
	requireOK(t, err)
	ctx := store.WithCredential(x.Context, core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	return store.WithActor(ctx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser}), owner
}

func runIdentity(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, owner := bootstrapOwner(t, x)
	identity := config.FirstOperatorIdentity{OrganizationName: "Conformance", Email: "owner@example.test", DisplayName: "Owner"}
	seeded, err := st.BootstrapIdentity(ctx, identity, "conformance-bootstrap")
	requireOK(t, err)
	if seeded {
		t.Fatal("unchanged bootstrap wrote identity")
	}
	seeded, err = st.BootstrapIdentity(ctx, identity, "conformance-rotated")
	requireOK(t, err)
	if !seeded {
		t.Fatal("rotation did not replace bootstrap credential")
	}
	if _, err := st.VerifyCredential(ctx, "conformance-bootstrap"); err == nil {
		t.Fatal("rotated credential still authenticates")
	}
	current, err := st.VerifyPersonalAccessToken(ctx, "conformance-rotated")
	requireOK(t, err)
	if current.ID != owner.ID {
		t.Fatal("rotation replaced owner")
	}
	caller, err := st.GetCallerIdentity(ctx, owner.ID, x.Workspace)
	requireOK(t, err)
	if caller.ID != owner.ID || caller.Role != core.WorkspaceRoleOperator {
		t.Fatal("bootstrap owner is not workspace operator")
	}
	if _, err := st.GetCallerIdentity(ctx, owner.ID, x.Workspace+"-absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign identity error=%v", err)
	}
	user, err := st.ProvisionIdentityUser(ctx, "member@example.test", "Member")
	requireOK(t, err)
	if user.Email != "member@example.test" {
		t.Fatal("provisioned email differs")
	}
	t.Run("MixedCaseEmailDedup", func(t *testing.T) { runMixedCaseEmailDedup(t, x, ctx, owner, identity) })
	t.Run("MixedCaseConcurrentDedup", func(t *testing.T) { runMixedCaseConcurrentDedup(t, x, ctx, owner) })
}

func runMembership(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, owner := bootstrapOwner(t, x)
	allowed, err := st.AuthorizeDeployment(ctx, owner.ID, core.CapabilityConfirmDocuments)
	requireOK(t, err)
	if !allowed {
		t.Fatal("bootstrap owner cannot administer deployment")
	}
	if err := st.RevokeWorkspaceRole(ctx, owner.ID, x.Workspace); !errors.Is(err, store.ErrLastWorkspaceOperator) {
		t.Fatalf("last operator guard=%v", err)
	}
	user, err := st.ProvisionIdentityUser(ctx, "member@example.test", "Member")
	requireOK(t, err)
	_, err = st.GrantWorkspaceRole(ctx, user.Email, x.Workspace, core.WorkspaceRoleExecutor)
	requireOK(t, err)
	allowed, err = st.AuthorizeWorkspace(ctx, user.ID, x.Workspace, core.CapabilityClaimWork)
	requireOK(t, err)
	if !allowed {
		t.Fatal("executor lacks claim capability")
	}
	allowed, err = st.AuthorizeWorkspace(ctx, user.ID, x.Workspace, core.CapabilityConfirmDocuments)
	requireOK(t, err)
	if allowed {
		t.Fatal("executor received operator capability")
	}
	workspaces, err := st.ListWorkspacesForUser(ctx, user.ID)
	requireOK(t, err)
	if len(workspaces) != 1 || workspaces[0].ID != x.Workspace {
		t.Fatal("membership workspace visibility differs")
	}
	members, err := st.ListWorkspaceMembers(ctx, owner.ID, x.Workspace)
	requireOK(t, err)
	if len(members) != 2 {
		t.Fatalf("members=%d", len(members))
	}
	requireOK(t, st.RevokeWorkspaceRole(ctx, user.ID, x.Workspace))
	allowed, err = st.AuthorizeWorkspace(ctx, user.ID, x.Workspace, core.CapabilityClaimWork)
	requireOK(t, err)
	if allowed {
		t.Fatal("revoked member can claim")
	}
	_, err = st.GrantWorkspaceRole(ctx, "invited@example.test", x.Workspace, core.WorkspaceRoleViewer)
	requireOK(t, err)
	invitations, err := st.ListWorkspaceInvitations(ctx, x.Workspace)
	requireOK(t, err)
	if len(invitations) != 1 {
		t.Fatalf("invitations=%d", len(invitations))
	}
	requireOK(t, st.RecordInvitationDelivery(ctx, "invited@example.test", "fallback"))
	requireOK(t, st.RevokeWorkspaceInvitation(ctx, "invited@example.test", x.Workspace))
	invitations, err = st.ListWorkspaceInvitations(ctx, x.Workspace)
	requireOK(t, err)
	if len(invitations) != 0 {
		t.Fatal("revoked invitation remains listed")
	}
	t.Run("MembershipAuditEvents", func(t *testing.T) { runMembershipAuditEvents(t, x, ctx, owner) })
}

func runInvitationSessions(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, _ := bootstrapOwner(t, x)
	if _, err := st.IssueSignInLink(ctx, "unknown@example.test"); err == nil {
		t.Fatal("sign-in created an uninvited account")
	}
	_, err := st.GrantWorkspaceRole(ctx, "invited@example.test", x.Workspace, core.WorkspaceRoleExecutor)
	requireOK(t, err)
	link, err := st.IssueSignInLink(ctx, "invited@example.test")
	requireOK(t, err)
	session, user, err := st.RedeemSignInLink(ctx, link.Value)
	requireOK(t, err)
	if _, _, err := st.RedeemSignInLink(ctx, link.Value); err == nil {
		t.Fatal("sign-in link was reusable")
	}
	auth, err := st.VerifyDashboardSession(ctx, session.Value)
	requireOK(t, err)
	if auth.OwnerUserID != user.ID {
		t.Fatal("session owner differs")
	}
	profile, err := st.SetOwnDisplayName(ctx, user.ID, session.ID, "Updated")
	requireOK(t, err)
	if profile.DisplayName != "Updated" {
		t.Fatal("display name did not change")
	}
	requireOK(t, st.SetOwnPassword(ctx, user.ID, session.ID, "", "conformance password one"))
	passwordSession, _, err := st.SignInWithPassword(ctx, user.Email, "conformance password one")
	requireOK(t, err)
	if err := st.SetOwnPassword(ctx, user.ID, passwordSession.ID, "wrong", "conformance password two"); !errors.Is(err, store.ErrInvalidCurrentPassword) {
		t.Fatalf("password proof error=%v", err)
	}
	requireOK(t, st.RevokeDashboardSession(ctx, user.ID, passwordSession.ID))
	if _, err := st.VerifyDashboardSession(ctx, passwordSession.Value); err == nil {
		t.Fatal("revoked session authenticates")
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "conformance-bootstrap")
	requireOK(t, err)
	t.Run("HostLocalIssueLinkAdmission", func(t *testing.T) { runHostLocalIssueLinkAdmission(t, x, ctx, owner) })
	t.Run("ScopedResend", func(t *testing.T) { runScopedResend(t, x, ctx, owner) })
	t.Run("RedemptionActor", func(t *testing.T) { runRedemptionActor(t, x, ctx, owner) })
	t.Run("ResendSerialization", func(t *testing.T) { runResendSerialization(t, x, ctx) })
}

func runTokens(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, owner := bootstrapOwner(t, x)
	binding := store.RunAgentCredentialBinding{WorkspaceID: x.Workspace, WorkOrderID: "fixture-order", SessionID: "fixture-session"}
	label, err := store.RunAgentCredentialLabel(binding)
	requireOK(t, err)
	agent, err := st.IssueAgentCredential(ctx, owner.ID, label)
	requireOK(t, err)
	auth, err := st.VerifyCredential(ctx, agent.Value)
	requireOK(t, err)
	if auth.Kind != core.CredentialAgent || auth.Scope != core.CredentialScopeUser {
		t.Fatal("agent credential gained human operator scope")
	}
	if _, err := st.VerifyPersonalAccessToken(ctx, agent.Value); err == nil {
		t.Fatal("agent credential accepted as a human PAT")
	}
	wrong := binding
	wrong.SessionID = "foreign"
	if err := st.RevokeRunAgentCredential(ctx, owner.ID, agent.ID, wrong); !errors.Is(err, store.ErrRunAgentCredentialBindingMismatch) {
		t.Fatalf("agent binding error=%v", err)
	}
	requireOK(t, st.RevokeRunAgentCredential(ctx, owner.ID, agent.ID, binding))
	if _, err := st.VerifyCredential(ctx, agent.Value); err == nil {
		t.Fatal("revoked agent authenticates")
	}
	issued, err := st.IssueOwnPersonalAccessToken(ctx, owner.ID, "conformance")
	requireOK(t, err)
	user, err := st.VerifyPersonalAccessToken(ctx, issued.Value)
	requireOK(t, err)
	if user.ID != owner.ID {
		t.Fatal("PAT owner differs")
	}
	listed, err := st.ListOwnPersonalAccessTokens(ctx, owner.ID)
	requireOK(t, err)
	if !slices.ContainsFunc(listed, func(token core.PersonalAccessToken) bool { return token.ID == issued.ID }) {
		t.Fatal("issued PAT absent")
	}
	if _, err := st.RevokeOwnPersonalAccessToken(ctx, "foreign", issued.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign PAT revocation=%v", err)
	}
	_, err = st.RevokeOwnPersonalAccessToken(ctx, owner.ID, issued.ID)
	requireOK(t, err)
	if _, err := st.VerifyPersonalAccessToken(ctx, issued.Value); err == nil {
		t.Fatal("revoked PAT authenticates")
	}
}

// workspaceEventsOf returns the workspace's taskless events of exactly kind.
func workspaceEventsOf(t *testing.T, x Fixture, workspace, kind string) []core.Event {
	t.Helper()
	if x.WorkspaceEvents == nil {
		t.Fatal("fixture does not expose workspace events")
	}
	events, err := x.WorkspaceEvents(store.WithWorkspace(x.Context, workspace), kind)
	requireOK(t, err)
	var out []core.Event
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func eventPayload(t *testing.T, event core.Event) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode %s payload: %v", event.Kind, err)
	}
	return payload
}

func requirePayload(t *testing.T, event core.Event, want map[string]any) map[string]any {
	t.Helper()
	payload := eventPayload(t, event)
	for key, value := range want {
		if fmt.Sprint(payload[key]) != fmt.Sprint(value) {
			t.Fatalf("%s payload %s=%v want %v (payload=%v)", event.Kind, key, payload[key], value, payload)
		}
	}
	return payload
}

// newForeignWorkspace bootstraps a second workspace for ledger isolation and
// cross-workspace refusals.
func newForeignWorkspace(t *testing.T, x Fixture, suffix string) string {
	t.Helper()
	foreign := x.Workspace + "-" + suffix
	_, err := x.Backend.BootstrapWorkspaceConfig(store.WithWorkspace(x.Context, foreign), &config.Config{Workspace: foreign, Repos: x.Config.Repos})
	requireOK(t, err)
	return foreign
}

// runMembershipAuditEvents checks the granted event and both revoked forms
// with workspace, actor, and payload on every backend
// (req-accounts-and-membership AC-2.1, AC-3.1).
func runMembershipAuditEvents(t *testing.T, x Fixture, ctx context.Context, owner core.IdentityUser) {
	st := x.Backend
	foreign := newForeignWorkspace(t, x, "audit")
	grantedBefore := len(workspaceEventsOf(t, x, x.Workspace, "workspace.membership_granted"))
	revokedBefore := len(workspaceEventsOf(t, x, x.Workspace, "workspace.membership_revoked"))
	foreignBefore := len(workspaceEventsOf(t, x, foreign, "workspace.membership_granted")) + len(workspaceEventsOf(t, x, foreign, "workspace.membership_revoked"))
	member, err := st.ProvisionIdentityUser(ctx, "audited@example.test", "Audited")
	requireOK(t, err)
	_, err = st.GrantWorkspaceRole(ctx, "Audited@Example.test", x.Workspace, core.WorkspaceRoleExecutor)
	requireOK(t, err)
	requireOK(t, st.RevokeWorkspaceRole(ctx, member.ID, x.Workspace))
	_, err = st.GrantWorkspaceRole(ctx, "audit-invitee@example.test", x.Workspace, core.WorkspaceRoleViewer)
	requireOK(t, err)
	requireOK(t, st.RevokeWorkspaceInvitation(ctx, "Audit-Invitee@example.test", x.Workspace))

	ownerActor := store.UserActorID(owner.ID)
	granted := workspaceEventsOf(t, x, x.Workspace, "workspace.membership_granted")[grantedBefore:]
	revoked := workspaceEventsOf(t, x, x.Workspace, "workspace.membership_revoked")[revokedBefore:]
	if len(granted) != 2 || len(revoked) != 2 {
		t.Fatalf("granted=%d revoked=%d, want 2 and 2", len(granted), len(revoked))
	}
	for _, event := range append(append([]core.Event{}, granted...), revoked...) {
		if event.ActorID != ownerActor || event.ActorRole != core.ActorUser {
			t.Fatalf("%s actor=%s/%s want %s/user", event.Kind, event.ActorID, event.ActorRole, ownerActor)
		}
	}
	requirePayload(t, granted[0], map[string]any{"workspace_id": x.Workspace, "email": "audited@example.test", "role": core.WorkspaceRoleExecutor, "invitation": false, "granted_by": owner.ID})
	requirePayload(t, granted[1], map[string]any{"workspace_id": x.Workspace, "email": "audit-invitee@example.test", "role": core.WorkspaceRoleViewer, "invitation": true, "granted_by": owner.ID})
	requirePayload(t, revoked[0], map[string]any{"workspace_id": x.Workspace, "user_id": member.ID})
	if _, ok := eventPayload(t, revoked[0])["invitation"]; ok {
		t.Fatalf("member revoke carries the invitation marker: %s", revoked[0].Payload)
	}
	requirePayload(t, revoked[1], map[string]any{"workspace_id": x.Workspace, "email": "audit-invitee@example.test", "invitation": true, "revoked_by": owner.ID})
	if after := len(workspaceEventsOf(t, x, foreign, "workspace.membership_granted")) + len(workspaceEventsOf(t, x, foreign, "workspace.membership_revoked")); after != foreignBefore {
		t.Fatalf("membership events leaked into another workspace: before=%d after=%d", foreignBefore, after)
	}
}

// runHostLocalIssueLinkAdmission keeps the deployment-wide admission of
// IssueSignInLink for the host-local act (req-invitations-and-sign-in REQ-3).
func runHostLocalIssueLinkAdmission(t *testing.T, x Fixture, ctx context.Context, owner core.IdentityUser) {
	st := x.Backend
	if _, err := st.IssueSignInLink(ctx, owner.Email); err != nil {
		t.Fatalf("active account refused: %v", err)
	}
	foreign := newForeignWorkspace(t, x, "hostlocal")
	_, err := st.GrantWorkspaceRole(ctx, "hostlocal-invitee@example.test", foreign, core.WorkspaceRoleViewer)
	requireOK(t, err)
	if _, err := st.IssueSignInLink(ctx, "HostLocal-Invitee@example.test"); err != nil {
		t.Fatalf("pending invitation refused: %v", err)
	}
	if _, err := st.IssueSignInLink(ctx, "hostlocal-unknown@example.test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown email error=%v", err)
	}
}

// runScopedResend checks that IssueInvitationSignInLink issues only for a
// pending invitation in the named workspace, records one resend event per
// successful resend, and rotates or records nothing on refusal.
func runScopedResend(t *testing.T, x Fixture, ctx context.Context, owner core.IdentityUser) {
	st := x.Backend
	foreign := newForeignWorkspace(t, x, "scoped")
	resentBefore := len(workspaceEventsOf(t, x, x.Workspace, "workspace.invitation_resent"))
	_, err := st.GrantWorkspaceRole(ctx, "scoped@example.test", x.Workspace, core.WorkspaceRoleViewer)
	requireOK(t, err)
	initial, err := st.IssueInvitationSignInLink(ctx, x.Workspace, "scoped@example.test", store.SignInLinkInvitation)
	requireOK(t, err)
	if got := len(workspaceEventsOf(t, x, x.Workspace, "workspace.invitation_resent")); got != resentBefore {
		t.Fatalf("initial invitation issuance recorded a resend: %d", got-resentBefore)
	}
	resent, err := st.IssueInvitationSignInLink(ctx, x.Workspace, "Scoped@Example.TEST", store.SignInLinkResend)
	requireOK(t, err)
	if resent.Email != "scoped@example.test" || resent.Value == "" || resent.Value == initial.Value {
		t.Fatalf("resent link=%+v", resent)
	}
	events := workspaceEventsOf(t, x, x.Workspace, "workspace.invitation_resent")[resentBefore:]
	if len(events) != 1 || events[0].ActorID != store.UserActorID(owner.ID) || events[0].ActorRole != core.ActorUser {
		t.Fatalf("resend events=%+v", events)
	}
	payload := requirePayload(t, events[0], map[string]any{"workspace_id": x.Workspace, "email": "scoped@example.test", "resent_by": owner.ID})
	if len(payload) != 3 {
		t.Fatalf("resend payload carries extra fields: %v", payload)
	}
	if strings.Contains(string(events[0].Payload), "cv_signin_") {
		t.Fatalf("resend payload carries a link value: %s", events[0].Payload)
	}

	_, err = st.GrantWorkspaceRole(ctx, "scoped-revoked@example.test", x.Workspace, core.WorkspaceRoleViewer)
	requireOK(t, err)
	requireOK(t, st.RevokeWorkspaceInvitation(ctx, "scoped-revoked@example.test", x.Workspace))
	for _, refused := range []struct{ name, workspace, email string }{
		{"wrong workspace", foreign, "scoped@example.test"},
		{"unknown email", x.Workspace, "scoped-unknown@example.test"},
		{"existing account", x.Workspace, owner.Email},
		{"revoked invitation", x.Workspace, "scoped-revoked@example.test"},
		{"malformed email", x.Workspace, "not an address"},
	} {
		for _, purpose := range []store.SignInLinkPurpose{store.SignInLinkResend, store.SignInLinkInvitation} {
			if _, err := st.IssueInvitationSignInLink(ctx, refused.workspace, refused.email, purpose); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s %s error=%v, want not found", refused.name, purpose, err)
			}
		}
	}
	if got := len(workspaceEventsOf(t, x, x.Workspace, "workspace.invitation_resent")) - resentBefore; got != 1 {
		t.Fatalf("refusals appended resend events: %d", got-1)
	}
	if got := len(workspaceEventsOf(t, x, foreign, "workspace.invitation_resent")); got != 0 {
		t.Fatalf("foreign workspace resend events=%d", got)
	}
	if _, err := st.IssueInvitationSignInLink(ctx, x.Workspace, "scoped@example.test", "operator-chosen"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown purpose error=%v", err)
	}
	// The refused foreign resend rotated nothing: the earlier link still
	// redeems, and the rotated initial link does not.
	if _, _, err := st.RedeemSignInLink(ctx, initial.Value); err == nil {
		t.Fatal("link rotated by the resend still redeems")
	}
	_, user, err := st.RedeemSignInLink(ctx, resent.Value)
	requireOK(t, err)
	if user.Email != "scoped@example.test" {
		t.Fatalf("redeemed user=%+v", user)
	}
	if _, err := st.IssueInvitationSignInLink(ctx, x.Workspace, "scoped@example.test", store.SignInLinkResend); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resend after redemption error=%v", err)
	}
	if got := len(workspaceEventsOf(t, x, x.Workspace, "workspace.invitation_resent")) - resentBefore; got != 1 {
		t.Fatalf("resend events after consumption=%d", got)
	}
}

// runRedemptionActor checks that redeeming a link attributes each consumed
// invitation's grant to the redeemed account, keeps the inviter as invited_by
// and granted_by, and leaves direct provisioning attributed to the inviter
// (req-accounts-and-membership AC-3.1).
func runRedemptionActor(t *testing.T, x Fixture, ctx context.Context, owner core.IdentityUser) {
	st := x.Backend
	foreign := newForeignWorkspace(t, x, "redeem")
	before := len(workspaceEventsOf(t, x, x.Workspace, "workspace.membership_granted"))
	foreignBefore := len(workspaceEventsOf(t, x, foreign, "workspace.membership_granted"))
	_, err := st.GrantWorkspaceRole(ctx, "redeemer@example.test", x.Workspace, core.WorkspaceRoleExecutor)
	requireOK(t, err)
	_, err = st.GrantWorkspaceRole(ctx, "redeemer@example.test", foreign, core.WorkspaceRoleViewer)
	requireOK(t, err)
	link, err := st.IssueInvitationSignInLink(ctx, x.Workspace, "redeemer@example.test", store.SignInLinkInvitation)
	requireOK(t, err)
	// The redeeming request carries a different actor and credential; the
	// recorded actor still comes from the redeemed link's account.
	spoofed := store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "spoof", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}), store.Actor{ID: "user:spoofed", Role: core.ActorUser})
	session, user, err := st.RedeemSignInLink(spoofed, link.Value)
	requireOK(t, err)
	if session.UserID != "" && session.UserID != user.ID {
		t.Fatalf("session owner=%s user=%s", session.UserID, user.ID)
	}
	auth, err := st.VerifyDashboardSession(ctx, session.Value)
	requireOK(t, err)
	if auth.OwnerUserID != user.ID || user.Email != "redeemer@example.test" {
		t.Fatalf("session owner=%s user=%+v", auth.OwnerUserID, user)
	}
	for _, scope := range []struct {
		workspace string
		before    int
		role      core.WorkspaceRole
	}{{x.Workspace, before, core.WorkspaceRoleExecutor}, {foreign, foreignBefore, core.WorkspaceRoleViewer}} {
		var redemptions []core.Event
		for _, event := range workspaceEventsOf(t, x, scope.workspace, "workspace.membership_granted")[scope.before:] {
			if eventPayload(t, event)["redemption"] == true {
				redemptions = append(redemptions, event)
			}
		}
		if len(redemptions) != 1 {
			t.Fatalf("%s redemption grants=%d", scope.workspace, len(redemptions))
		}
		if redemptions[0].ActorID != store.UserActorID(user.ID) || redemptions[0].ActorRole != core.ActorUser {
			t.Fatalf("%s redemption actor=%s/%s want user:%s", scope.workspace, redemptions[0].ActorID, redemptions[0].ActorRole, user.ID)
		}
		requirePayload(t, redemptions[0], map[string]any{"workspace_id": scope.workspace, "user_id": user.ID, "email": user.Email, "role": scope.role, "invitation": false, "redemption": true, "invited_by": owner.ID, "granted_by": owner.ID})
		caller, err := st.GetCallerIdentity(ctx, user.ID, scope.workspace)
		requireOK(t, err)
		if caller.Role != scope.role {
			t.Fatalf("%s binding role=%s want %s", scope.workspace, caller.Role, scope.role)
		}
	}
	invitations, err := st.ListWorkspaceInvitations(ctx, foreign)
	requireOK(t, err)
	if slices.ContainsFunc(invitations, func(item core.WorkspaceInvitation) bool { return item.Email == "redeemer@example.test" }) {
		t.Fatal("consumed invitation remains pending")
	}

	// Direct provisioning keeps attributing the consumed invitation to the
	// inviter.
	directBefore := len(workspaceEventsOf(t, x, x.Workspace, "workspace.membership_granted"))
	_, err = st.GrantWorkspaceRole(ctx, "direct@example.test", x.Workspace, core.WorkspaceRoleViewer)
	requireOK(t, err)
	direct, err := st.ProvisionIdentityUser(ctx, "direct@example.test", "Direct")
	requireOK(t, err)
	var directRedemption []core.Event
	for _, event := range workspaceEventsOf(t, x, x.Workspace, "workspace.membership_granted")[directBefore:] {
		if eventPayload(t, event)["redemption"] == true {
			directRedemption = append(directRedemption, event)
		}
	}
	if len(directRedemption) != 1 || directRedemption[0].ActorID != store.UserActorID(owner.ID) {
		t.Fatalf("direct provisioning redemption events=%+v", directRedemption)
	}
	payload := requirePayload(t, directRedemption[0], map[string]any{"user_id": direct.ID, "granted_by": owner.ID, "redemption": true})
	if _, ok := payload["invited_by"]; ok {
		t.Fatalf("direct provisioning payload gained invited_by: %v", payload)
	}
}

// runResendSerialization holds one side of each race at its email
// serialization through the identity test hook and channels, never sleeps,
// and checks that resend, revocation, and redemption never split a link from
// its audit event (component-identity-membership).
func runResendSerialization(t *testing.T, x Fixture, ctx context.Context) {
	st := x.Backend
	type outcome struct {
		link core.IssuedSignInLink
		err  error
	}
	holdAt := func(stage string) (context.Context, chan struct{}, chan struct{}) {
		locked, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		return store.WithIdentityTestHook(ctx, func(at string) error {
			if at == stage {
				once.Do(func() { close(locked) })
				<-release
			}
			return nil
		}), locked, release
	}
	resentFor := func(email string) int {
		count := 0
		for _, event := range workspaceEventsOf(t, x, x.Workspace, "workspace.invitation_resent") {
			if eventPayload(t, event)["email"] == email {
				count++
			}
		}
		return count
	}
	grant := func(email string) {
		t.Helper()
		_, err := st.GrantWorkspaceRole(ctx, email, x.Workspace, core.WorkspaceRoleViewer)
		requireOK(t, err)
	}

	t.Run("resend holds the lock before revoke", func(t *testing.T) {
		email := "race-resend-first@example.test"
		grant(email)
		hookCtx, locked, release := holdAt(store.IdentityHookInvitationLinkLocked)
		resent := make(chan outcome, 1)
		go func() {
			link, err := st.IssueInvitationSignInLink(hookCtx, x.Workspace, email, store.SignInLinkResend)
			resent <- outcome{link, err}
		}()
		<-locked
		revoked := make(chan error, 1)
		go func() { revoked <- st.RevokeWorkspaceInvitation(ctx, email, x.Workspace) }()
		close(release)
		first := <-resent
		if first.err != nil {
			t.Fatalf("resend holding the lock failed: %v", first.err)
		}
		if err := <-revoked; err != nil {
			t.Fatalf("revoke after resend: %v", err)
		}
		if got := resentFor(email); got != 1 {
			t.Fatalf("resend events=%d want 1 committed with the link", got)
		}
		if _, err := st.IssueInvitationSignInLink(ctx, x.Workspace, email, store.SignInLinkResend); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("resend after revoke error=%v", err)
		}
	})

	t.Run("revoke holds the lock before resend", func(t *testing.T) {
		email := "race-revoke-first@example.test"
		grant(email)
		hookCtx, locked, release := holdAt(store.IdentityHookInvitationRevokeLocked)
		revoked := make(chan error, 1)
		go func() { revoked <- st.RevokeWorkspaceInvitation(hookCtx, email, x.Workspace) }()
		<-locked
		resent := make(chan outcome, 1)
		go func() {
			link, err := st.IssueInvitationSignInLink(ctx, x.Workspace, email, store.SignInLinkResend)
			resent <- outcome{link, err}
		}()
		close(release)
		if err := <-revoked; err != nil {
			t.Fatalf("revoke holding the lock failed: %v", err)
		}
		if second := <-resent; !errors.Is(second.err, store.ErrNotFound) || second.link.Value != "" {
			t.Fatalf("resend after winning revoke link=%q err=%v", second.link.Value, second.err)
		}
		if got := resentFor(email); got != 0 {
			t.Fatalf("refused resend recorded %d events", got)
		}
	})

	t.Run("resend holds the lock before redemption", func(t *testing.T) {
		email := "race-resend-redeem@example.test"
		grant(email)
		earlier, err := st.IssueInvitationSignInLink(ctx, x.Workspace, email, store.SignInLinkInvitation)
		requireOK(t, err)
		hookCtx, locked, release := holdAt(store.IdentityHookInvitationLinkLocked)
		resent := make(chan outcome, 1)
		go func() {
			link, err := st.IssueInvitationSignInLink(hookCtx, x.Workspace, email, store.SignInLinkResend)
			resent <- outcome{link, err}
		}()
		<-locked
		redeemed := make(chan error, 1)
		go func() {
			_, _, err := st.RedeemSignInLink(ctx, earlier.Value)
			redeemed <- err
		}()
		close(release)
		first := <-resent
		if first.err != nil {
			t.Fatalf("resend holding the lock failed: %v", first.err)
		}
		if err := <-redeemed; err == nil {
			t.Fatal("link rotated by the winning resend still redeemed")
		}
		if got := resentFor(email); got != 1 {
			t.Fatalf("resend events=%d", got)
		}
		if _, _, err := st.RedeemSignInLink(ctx, first.link.Value); err != nil {
			t.Fatalf("resent link refused: %v", err)
		}
	})

	t.Run("redemption holds the lock before resend", func(t *testing.T) {
		email := "race-redeem-first@example.test"
		grant(email)
		link, err := st.IssueInvitationSignInLink(ctx, x.Workspace, email, store.SignInLinkInvitation)
		requireOK(t, err)
		hookCtx, locked, release := holdAt(store.IdentityHookSignInRedeemLocked)
		redeemed := make(chan error, 1)
		go func() {
			_, _, err := st.RedeemSignInLink(hookCtx, link.Value)
			redeemed <- err
		}()
		<-locked
		resent := make(chan outcome, 1)
		go func() {
			link, err := st.IssueInvitationSignInLink(ctx, x.Workspace, email, store.SignInLinkResend)
			resent <- outcome{link, err}
		}()
		close(release)
		if err := <-redeemed; err != nil {
			t.Fatalf("redemption holding the lock failed: %v", err)
		}
		if second := <-resent; !errors.Is(second.err, store.ErrNotFound) || second.link.Value != "" {
			t.Fatalf("resend after consuming redemption link=%q err=%v", second.link.Value, second.err)
		}
		if got := resentFor(email); got != 0 {
			t.Fatalf("refused resend recorded %d events", got)
		}
	})

	t.Run("audit failure rolls back the resend", func(t *testing.T) {
		email := "resend-audit-fault@example.test"
		grant(email)
		earlier, err := st.IssueInvitationSignInLink(ctx, x.Workspace, email, store.SignInLinkInvitation)
		requireOK(t, err)
		fault := errors.New("injected resend audit failure")
		faultCtx := store.WithIdentityTestHook(ctx, func(stage string) error {
			if stage == store.IdentityHookInvitationResentAudit {
				return fault
			}
			return nil
		})
		if link, err := st.IssueInvitationSignInLink(faultCtx, x.Workspace, email, store.SignInLinkResend); err == nil || link.Value != "" {
			t.Fatalf("resend with failing audit link=%q err=%v", link.Value, err)
		}
		if got := resentFor(email); got != 0 {
			t.Fatalf("failed resend recorded %d events", got)
		}
		// Nothing rotated: the earlier link still redeems.
		if _, _, err := st.RedeemSignInLink(ctx, earlier.Value); err != nil {
			t.Fatalf("earlier link rotated by a rolled-back resend: %v", err)
		}
	})
}

// runMixedCaseEmailDedup proves one normalized account across bootstrap,
// provisioning, grant, invitation, and sign-in link entry points
// (req-accounts-and-membership REQ-1).
func runMixedCaseEmailDedup(t *testing.T, x Fixture, ctx context.Context, owner core.IdentityUser, identity config.FirstOperatorIdentity) {
	st := x.Backend
	upper := identity
	upper.Email = "OWNER@Example.TEST"
	if changed, err := st.BootstrapIdentity(ctx, upper, "conformance-rotated"); err != nil || changed {
		t.Fatalf("mixed-case bootstrap retry changed=%v err=%v", changed, err)
	}
	resolved, err := st.ProvisionIdentityUser(ctx, " Owner@EXAMPLE.test ", "Ignored")
	requireOK(t, err)
	if resolved.ID != owner.ID {
		t.Fatalf("mixed-case owner resolved to %s want %s", resolved.ID, owner.ID)
	}

	first, err := st.ProvisionIdentityUser(ctx, "A.Mixed@Example.test", "Mixed")
	requireOK(t, err)
	second, err := st.ProvisionIdentityUser(ctx, "a.mixed@example.test", "Mixed Again")
	requireOK(t, err)
	if first.ID != second.ID || first.Email != "a.mixed@example.test" {
		t.Fatalf("mixed-case provisioning first=%+v second=%+v", first, second)
	}
	for _, email := range []string{"A.MIXED@example.test", "a.mixed@EXAMPLE.TEST"} {
		_, err = st.GrantWorkspaceRole(ctx, email, x.Workspace, core.WorkspaceRoleExecutor)
		requireOK(t, err)
	}
	members, err := st.ListWorkspaceMembers(ctx, owner.ID, x.Workspace)
	requireOK(t, err)
	matches := 0
	for _, member := range members {
		if strings.EqualFold(member.Email, "a.mixed@example.test") {
			matches++
			if member.UserID != first.ID || member.Email != "a.mixed@example.test" {
				t.Fatalf("mixed-case member=%+v", member)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("mixed-case grants produced %d bindings", matches)
	}

	for _, email := range []string{"New.Case@Example.test", "new.case@example.test"} {
		_, err = st.GrantWorkspaceRole(ctx, email, x.Workspace, core.WorkspaceRoleViewer)
		requireOK(t, err)
	}
	invitations, err := st.ListWorkspaceInvitations(ctx, x.Workspace)
	requireOK(t, err)
	pending := 0
	for _, invitation := range invitations {
		if strings.EqualFold(invitation.Email, "new.case@example.test") {
			pending++
			if invitation.Email != "new.case@example.test" {
				t.Fatalf("invitation email not normalized: %+v", invitation)
			}
		}
	}
	if pending != 1 {
		t.Fatalf("mixed-case invitations=%d", pending)
	}
	hostLink, err := st.IssueSignInLink(ctx, "NEW.CASE@example.test")
	requireOK(t, err)
	scopedLink, err := st.IssueInvitationSignInLink(ctx, x.Workspace, "New.Case@Example.TEST", store.SignInLinkResend)
	requireOK(t, err)
	if hostLink.Email != "new.case@example.test" || scopedLink.Email != "new.case@example.test" {
		t.Fatalf("issued emails host=%s scoped=%s", hostLink.Email, scopedLink.Email)
	}
	if _, _, err := st.RedeemSignInLink(ctx, hostLink.Value); err == nil {
		t.Fatal("mixed-case reissue did not rotate the earlier link")
	}
	_, redeemed, err := st.RedeemSignInLink(ctx, scopedLink.Value)
	requireOK(t, err)
	again, err := st.ProvisionIdentityUser(ctx, "NEW.CASE@EXAMPLE.TEST", "Ignored")
	requireOK(t, err)
	if again.ID != redeemed.ID {
		t.Fatalf("re-provisioned id=%s redeemed id=%s", again.ID, redeemed.ID)
	}
	caller, err := st.GetCallerIdentity(ctx, redeemed.ID, x.Workspace)
	requireOK(t, err)
	if caller.Role != core.WorkspaceRoleViewer || caller.Email != "new.case@example.test" {
		t.Fatalf("redeemed caller=%+v", caller)
	}
}

// runMixedCaseConcurrentDedup releases mixed-case provisioning and scoped
// issuance contenders together through a channel barrier and requires one
// account and one binding when all finish. Each scoped issuance either
// succeeds while the invitation is pending or refuses once it is consumed.
func runMixedCaseConcurrentDedup(t *testing.T, x Fixture, ctx context.Context, owner core.IdentityUser) {
	st := x.Backend
	_, err := st.GrantWorkspaceRole(ctx, "race.user@example.test", x.Workspace, core.WorkspaceRoleContributor)
	requireOK(t, err)
	variants := []string{"Race.User@Example.test", "race.user@example.test", "RACE.USER@EXAMPLE.TEST", "rAce.uSer@example.Test"}
	start := make(chan struct{})
	type result struct {
		kind string
		id   string
		err  error
	}
	results := make(chan result, 2*len(variants))
	var wg sync.WaitGroup
	for _, email := range variants {
		wg.Add(2)
		go func(email string) {
			defer wg.Done()
			<-start
			user, err := st.ProvisionIdentityUser(ctx, email, "Race User")
			results <- result{"provision", user.ID, err}
		}(email)
		go func(email string) {
			defer wg.Done()
			<-start
			link, err := st.IssueInvitationSignInLink(ctx, x.Workspace, email, store.SignInLinkResend)
			results <- result{"issue", link.Email, err}
		}(email)
	}
	close(start)
	wg.Wait()
	close(results)
	ids := map[string]bool{}
	for r := range results {
		switch {
		case r.kind == "provision" && r.err != nil:
			t.Fatalf("concurrent provisioning failed: %v", r.err)
		case r.kind == "provision":
			ids[r.id] = true
		case r.err != nil && !errors.Is(r.err, store.ErrNotFound):
			t.Fatalf("concurrent scoped issuance failed: %v", r.err)
		case r.err == nil && r.id != "race.user@example.test":
			t.Fatalf("concurrent scoped issuance email=%s", r.id)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("concurrent mixed-case provisioning created %d accounts", len(ids))
	}
	members, err := st.ListWorkspaceMembers(ctx, owner.ID, x.Workspace)
	requireOK(t, err)
	bound := 0
	for _, member := range members {
		if strings.EqualFold(member.Email, "race.user@example.test") {
			bound++
			if !ids[member.UserID] || member.Role != core.WorkspaceRoleContributor {
				t.Fatalf("concurrent binding=%+v", member)
			}
		}
	}
	if bound != 1 {
		t.Fatalf("concurrent mixed-case bindings=%d", bound)
	}
	if _, err := st.IssueInvitationSignInLink(ctx, x.Workspace, "race.user@example.test", store.SignInLinkResend); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("scoped issuance after consumption error=%v", err)
	}
}
