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
	ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	// Bootstrap never writes bindings (DEC-63(4)), and the fixture workspace
	// exists before the deployment owner. The fixture therefore grants the
	// owner's operator binding explicitly instead of relying on healing.
	if caller, err := x.Backend.GetCallerIdentity(ctx, owner.ID, x.Workspace); err != nil || caller.Role != core.WorkspaceRoleOperator {
		_, err = x.Backend.GrantWorkspaceRole(ctx, owner.Email, x.Workspace, core.WorkspaceRoleOperator)
		requireOK(t, err)
	}
	return ctx, owner
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
	// These cases rotate the deployment token and restore the conformance
	// token when they finish, so they run after the mixed-case cases.
	t.Run("BootstrapPreservesMembershipDecisions", func(t *testing.T) { runBootstrapPreservesMembershipDecisions(t, x) })
	t.Run("BootstrapWorkspaceUsesDeploymentOwner", func(t *testing.T) { runBootstrapWorkspaceUsesDeploymentOwner(t, x) })
	t.Run("BootstrapOwnershipStable", func(t *testing.T) { runBootstrapOwnershipStable(t, x) })
	t.Run("BootstrapMembershipSerialization", func(t *testing.T) { runBootstrapMembershipSerialization(t, x) })
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
	t.Run("InstanceAdministrationOwnerOnly", func(t *testing.T) { runInstanceAdministrationOwnerOnly(t, x) })
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
	// A grant that names another workspace is recorded in that workspace's
	// ledger, whatever workspace the call's context carries.
	_, err = st.GrantWorkspaceRole(ctx, "foreign-audit@example.test", foreign, core.WorkspaceRoleViewer)
	requireOK(t, err)
	foreignGranted := workspaceEventsOf(t, x, foreign, "workspace.membership_granted")
	if len(foreignGranted) == 0 {
		t.Fatal("foreign grant recorded no event in its workspace")
	}
	requirePayload(t, foreignGranted[len(foreignGranted)-1], map[string]any{"workspace_id": foreign, "email": "foreign-audit@example.test", "invitation": true})
	if got := len(workspaceEventsOf(t, x, x.Workspace, "workspace.membership_granted")) - grantedBefore; got != 2 {
		t.Fatalf("foreign grant leaked into the context workspace: granted=%d", got)
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

// Instance administration and membership preservation (DEC-63;
// component-identity-membership, Verification). The shared fixture keeps one
// deployment marker per backend binary, so these cases work against the
// existing marker and restore the conformance token before they return.

const conformanceBootstrapToken = "conformance-bootstrap"

func conformanceIdentity() config.FirstOperatorIdentity {
	return config.FirstOperatorIdentity{OrganizationName: "Conformance", Email: "owner@example.test", DisplayName: "Owner"}
}

func userContext(base context.Context, userID string) context.Context {
	ctx := store.WithCredential(base, core.AuthenticatedCredential{ID: "conformance-" + userID, OwnerUserID: userID, Kind: core.CredentialUser, Scope: core.CredentialScopeUser})
	return store.WithActor(ctx, store.Actor{ID: store.UserActorID(userID), Role: core.ActorUser})
}

// systemContext carries the explicit system actor and no credential, so a
// workspace it creates binds nobody (DEC-63(4)).
func systemContext(base context.Context) context.Context {
	return store.WithActor(base, store.Actor{ID: "system", Role: core.ActorSystem})
}

func roleIn(t *testing.T, st store.Backend, ctx context.Context, userID, workspaceID string) core.WorkspaceRole {
	t.Helper()
	caller, err := st.GetCallerIdentity(ctx, userID, workspaceID)
	if errors.Is(err, store.ErrNotFound) {
		return ""
	}
	requireOK(t, err)
	return caller.Role
}

func memberWith(t *testing.T, st store.Backend, ctx context.Context, email, workspaceID string, role core.WorkspaceRole) core.IdentityUser {
	t.Helper()
	user, err := st.ProvisionIdentityUser(ctx, email, "Member "+string(role))
	requireOK(t, err)
	_, err = st.GrantWorkspaceRole(ctx, user.Email, workspaceID, role)
	requireOK(t, err)
	return user
}

// runInstanceAdministrationOwnerOnly proves that only the marker owner is the
// instance-administration principal and that no workspace role confers it
// (DEC-63(1)).
func runInstanceAdministrationOwnerOnly(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, owner := bootstrapOwner(t, x)
	if allowed, err := st.AuthorizeInstanceAdministration(ctx, owner.ID); err != nil || !allowed {
		t.Fatalf("marker owner admitted=%t err=%v", allowed, err)
	}
	foreign := newForeignWorkspace(t, x, "instance-admin")
	foreignOperator := memberWith(t, st, ctx, "foreign-operator-"+x.Workspace+"@example.test", foreign, core.WorkspaceRoleOperator)
	if allowed, err := st.AuthorizeInstanceAdministration(ctx, foreignOperator.ID); err != nil || allowed {
		t.Fatalf("operator of another workspace admitted=%t err=%v", allowed, err)
	}
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator} {
		member := memberWith(t, st, ctx, "instance-"+string(role)+"-"+x.Workspace+"@example.test", x.Workspace, role)
		if allowed, err := st.AuthorizeInstanceAdministration(ctx, member.ID); err != nil || allowed {
			t.Fatalf("%s admitted=%t err=%v", role, allowed, err)
		}
		// The role table is unchanged: workspace authority still follows it.
		for _, capability := range []core.Capability{core.CapabilityViewWorkspace, core.CapabilityManageWorkspace, core.CapabilityManageMembership} {
			allowed, err := st.AuthorizeWorkspace(ctx, member.ID, x.Workspace, capability)
			requireOK(t, err)
			if allowed != core.RoleAllows(role, capability) {
				t.Fatalf("%s %s allowed=%t want %t", role, capability, allowed, !allowed)
			}
		}
	}
	for _, unknown := range []string{"", "usr_unknown_" + x.Workspace} {
		if allowed, err := st.AuthorizeInstanceAdministration(ctx, unknown); err != nil || allowed {
			t.Fatalf("unknown account %q admitted=%t err=%v", unknown, allowed, err)
		}
	}
}

// runBootstrapPreservesMembershipDecisions proves that an unchanged restart
// and a rotation keep a demotion, a missing binding, and an exclusion
// (DEC-63(3), DEC-63(4)).
func runBootstrapPreservesMembershipDecisions(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, owner := bootstrapOwner(t, x)
	t.Cleanup(func() { _, _ = st.BootstrapIdentity(x.Context, conformanceIdentity(), conformanceBootstrapToken) })
	second := memberWith(t, st, ctx, "preserve-second-"+x.Workspace+"@example.test", x.Workspace, core.WorkspaceRoleOperator)
	secondCtx := userContext(x.Context, second.ID)
	_, err := st.GrantWorkspaceRole(secondCtx, owner.Email, x.Workspace, core.WorkspaceRoleViewer)
	requireOK(t, err)
	excluded := x.Workspace + "-excluded"
	_, err = st.CreateWorkspace(systemContext(store.WithWorkspace(x.Context, excluded)), excluded, excluded, &config.Config{Workspace: excluded, Repos: x.Config.Repos})
	requireOK(t, err)
	missing := newForeignWorkspace(t, x, "missing")
	check := func(step string) {
		t.Helper()
		if role := roleIn(t, st, ctx, owner.ID, x.Workspace); role != core.WorkspaceRoleViewer {
			t.Fatalf("%s: demoted owner role=%q", step, role)
		}
		for _, workspace := range []string{excluded, missing} {
			if role := roleIn(t, st, ctx, owner.ID, workspace); role != "" {
				t.Fatalf("%s: owner gained %s in %s", step, role, workspace)
			}
		}
		listed, err := st.ListWorkspacesForUser(ctx, owner.ID)
		requireOK(t, err)
		for _, item := range listed {
			if item.ID == excluded || item.ID == missing {
				t.Fatalf("%s: owner lists excluded workspace %s", step, item.ID)
			}
		}
	}
	check("before restart")
	changed, err := st.BootstrapIdentity(x.Context, conformanceIdentity(), conformanceBootstrapToken)
	requireOK(t, err)
	if changed {
		t.Fatal("unchanged restart reported a change")
	}
	check("unchanged restart")
	changed, err = st.BootstrapIdentity(x.Context, conformanceIdentity(), "conformance-preserve-rotated")
	requireOK(t, err)
	if !changed {
		t.Fatal("rotation reported no change")
	}
	check("rotation")
	if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(x.Context, x.Workspace), x.Config); err != nil {
		t.Fatal(err)
	}
	check("repeated workspace initialization")
}

// runBootstrapWorkspaceUsesDeploymentOwner proves the populated-registry half
// of DEC-63(4): a later configured workspace binds nobody, and repeated
// initialization never rewrites a binding. The empty-registry half runs in
// RunFreshDeploymentBootstrap.
func runBootstrapWorkspaceUsesDeploymentOwner(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, owner := bootstrapOwner(t, x)
	later := x.Workspace + "-later"
	seeded, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(x.Context, later), &config.Config{Workspace: later, Repos: x.Config.Repos})
	requireOK(t, err)
	if !seeded {
		t.Fatal("later workspace was not seeded")
	}
	if role := roleIn(t, st, ctx, owner.ID, later); role != "" {
		t.Fatalf("populated registry bound the owner as %s", role)
	}
	members, err := st.ListWorkspaceMembers(ctx, owner.ID, later)
	if err == nil && len(members) != 0 {
		t.Fatalf("populated registry seeded members=%+v", members)
	}
}

// runBootstrapOwnershipStable proves that a changed configured identity and a
// rotation keep the marker owner and that an unchanged start is a no-op
// (DEC-63(3)).
func runBootstrapOwnershipStable(t *testing.T, x Fixture) {
	st := x.Backend
	_, owner := bootstrapOwner(t, x)
	t.Cleanup(func() { _, _ = st.BootstrapIdentity(x.Context, conformanceIdentity(), conformanceBootstrapToken) })
	changedIdentity := config.FirstOperatorIdentity{OrganizationName: "Renamed", Email: "renamed-" + x.Workspace + "@example.test", DisplayName: "Renamed"}
	changed, err := st.BootstrapIdentity(x.Context, changedIdentity, "conformance-stable-rotated")
	requireOK(t, err)
	if !changed {
		t.Fatal("rotation under a changed identity reported no change")
	}
	current, err := st.VerifyPersonalAccessToken(x.Context, "conformance-stable-rotated")
	requireOK(t, err)
	if current.ID != owner.ID || current.Email != owner.Email {
		t.Fatalf("changed configuration replaced the owner: %+v want %s", current, owner.ID)
	}
	if allowed, err := st.AuthorizeInstanceAdministration(x.Context, owner.ID); err != nil || !allowed {
		t.Fatalf("owner lost instance administration allowed=%t err=%v", allowed, err)
	}
	changed, err = st.BootstrapIdentity(x.Context, changedIdentity, "conformance-stable-rotated")
	requireOK(t, err)
	if changed {
		t.Fatal("unchanged start reported a change")
	}
}

// runBootstrapMembershipSerialization holds bootstrap at its locked and
// before-commit stages with channels while a demotion and an exclusion are
// issued, in both orders, and proves no binding is restored. An error
// injected before commit rolls back a rotation (DEC-63(4)).
func runBootstrapMembershipSerialization(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, owner := bootstrapOwner(t, x)
	t.Cleanup(func() { _, _ = st.BootstrapIdentity(x.Context, conformanceIdentity(), conformanceBootstrapToken) })
	second := memberWith(t, st, ctx, "serial-second-"+x.Workspace+"@example.test", x.Workspace, core.WorkspaceRoleOperator)
	secondCtx := userContext(x.Context, second.ID)
	exclusion := newForeignWorkspace(t, x, "serial")
	_, err := st.GrantWorkspaceRole(ctx, owner.Email, exclusion, core.WorkspaceRoleOperator)
	requireOK(t, err)
	_, err = st.GrantWorkspaceRole(ctx, second.Email, exclusion, core.WorkspaceRoleOperator)
	requireOK(t, err)

	for _, test := range []struct {
		stage  string
		token  string
		change func() error
		check  func() bool
	}{
		{store.IdentityHookBootstrapLocked, "conformance-serial-one", func() error {
			_, err := st.GrantWorkspaceRole(secondCtx, owner.Email, x.Workspace, core.WorkspaceRoleViewer)
			return err
		}, func() bool { return roleIn(t, st, ctx, owner.ID, x.Workspace) == core.WorkspaceRoleViewer }},
		{store.IdentityHookBootstrapBeforeCommit, "conformance-serial-two", func() error {
			return st.RevokeWorkspaceRole(store.WithWorkspace(secondCtx, exclusion), owner.ID, exclusion)
		}, func() bool { return roleIn(t, st, ctx, owner.ID, exclusion) == "" }},
	} {
		reached, release := make(chan struct{}), make(chan struct{})
		hooked := store.WithIdentityTestHook(x.Context, func(stage string) error {
			if stage == test.stage {
				close(reached)
				<-release
			}
			return nil
		})
		bootstrapDone := make(chan error, 1)
		go func() {
			_, err := st.BootstrapIdentity(hooked, conformanceIdentity(), test.token)
			bootstrapDone <- err
		}()
		<-reached
		// The membership change is issued while bootstrap holds its
		// serialization; backends that share the lock queue it behind
		// bootstrap, and the outcome must be the same either way.
		changeDone := make(chan error, 1)
		go func() { changeDone <- test.change() }()
		close(release)
		requireOK(t, <-bootstrapDone)
		requireOK(t, <-changeDone)
		if !test.check() {
			t.Fatalf("%s: bootstrap restored a membership change", test.stage)
		}
		// The opposite order: the change has committed and a later
		// bootstrap still leaves it in place.
		_, err := st.BootstrapIdentity(x.Context, conformanceIdentity(), test.token+"-after")
		requireOK(t, err)
		if !test.check() {
			t.Fatalf("%s: later bootstrap restored a membership change", test.stage)
		}
	}

	// An error injected after the rotation writes rolls them back.
	_, err = st.BootstrapIdentity(x.Context, conformanceIdentity(), conformanceBootstrapToken)
	requireOK(t, err)
	injected := errors.New("injected bootstrap failure")
	failing := store.WithIdentityTestHook(x.Context, func(stage string) error {
		if stage == store.IdentityHookBootstrapBeforeCommit {
			return injected
		}
		return nil
	})
	if _, err := st.BootstrapIdentity(failing, conformanceIdentity(), "conformance-serial-rolled-back"); !errors.Is(err, injected) {
		t.Fatalf("injected failure err=%v", err)
	}
	if _, err := st.VerifyPersonalAccessToken(x.Context, "conformance-serial-rolled-back"); err == nil {
		t.Fatal("rolled-back rotation authenticates")
	}
	if current, err := st.VerifyPersonalAccessToken(x.Context, conformanceBootstrapToken); err != nil || current.ID != owner.ID {
		t.Fatalf("rollback lost the previous deployment token: %+v err=%v", current, err)
	}
}

// FreshDeployment opens an empty backend: no organization, account, marker,
// or workspace. Deactivate marks an account deactivated through the
// backend's native seam, because deactivation is not part of store.Backend.
type FreshDeployment struct {
	Open       func(*testing.T) store.Backend
	Deactivate func(*testing.T, store.Backend, string)
	// DeploymentEvents counts deployment ledger rows of one kind, when the
	// backend exposes them to its test binary.
	DeploymentEvents func(*testing.T, store.Backend, string) int
}

// RunFreshDeploymentBootstrap proves the empty-deployment rules of DEC-63 on
// one backend: the configured identity becomes the owner without a marker,
// neither account age nor an operator binding selects it, the first workspace
// of an empty registry binds only the owner, an owner without bindings is
// the principal, a revoked deployment token fails while a new one is reissued
// to the same owner, and a deactivated owner fails closed.
func RunFreshDeploymentBootstrap(t *testing.T, fresh FreshDeployment) {
	t.Run("NoMarkerUsesConfiguredIdentity", func(t *testing.T) {
		st := fresh.Open(t)
		ctx := t.Context()
		older, err := st.ProvisionIdentityUser(ctx, "older@example.test", "Older Account")
		requireOK(t, err)
		if allowed, err := st.AuthorizeInstanceAdministration(ctx, older.ID); err != nil || allowed {
			t.Fatalf("deployment without a marker admitted=%t err=%v", allowed, err)
		}
		identity := config.FirstOperatorIdentity{OrganizationName: "Fresh", Email: "Configured@Example.test", DisplayName: "Configured"}
		changed, err := st.BootstrapIdentity(ctx, identity, "fresh-token")
		requireOK(t, err)
		if !changed {
			t.Fatal("first bootstrap reported no change")
		}
		owner, err := st.VerifyPersonalAccessToken(ctx, "fresh-token")
		requireOK(t, err)
		if owner.Email != "configured@example.test" || owner.ID == older.ID {
			t.Fatalf("owner=%+v older=%s", owner, older.ID)
		}
		if allowed, err := st.AuthorizeInstanceAdministration(ctx, owner.ID); err != nil || !allowed {
			t.Fatalf("owner without bindings admitted=%t err=%v", allowed, err)
		}
		if allowed, err := st.AuthorizeInstanceAdministration(ctx, older.ID); err != nil || allowed {
			t.Fatalf("older account admitted=%t err=%v", allowed, err)
		}
		listed, err := st.ListWorkspacesForUser(ctx, owner.ID)
		requireOK(t, err)
		if len(listed) != 0 {
			t.Fatalf("bootstrap bound the owner before any workspace: %+v", listed)
		}

		first := "fresh-first"
		firstCtx := store.WithWorkspace(ctx, first)
		seeded, err := st.BootstrapWorkspaceConfig(firstCtx, &config.Config{Workspace: first})
		requireOK(t, err)
		if !seeded {
			t.Fatal("first workspace not seeded")
		}
		if role := roleIn(t, st, ctx, owner.ID, first); role != core.WorkspaceRoleOperator {
			t.Fatalf("first workspace owner role=%q", role)
		}
		if role := roleIn(t, st, ctx, older.ID, first); role != "" {
			t.Fatalf("older account bound as %s", role)
		}
		ownerCtx := userContext(ctx, owner.ID)
		second := memberWith(t, st, ownerCtx, "fresh-second@example.test", first, core.WorkspaceRoleOperator)
		_, err = st.GrantWorkspaceRole(userContext(ctx, second.ID), owner.Email, first, core.WorkspaceRoleViewer)
		requireOK(t, err)
		if _, err := st.BootstrapWorkspaceConfig(firstCtx, &config.Config{Workspace: first}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.BootstrapIdentity(ctx, identity, "fresh-token"); err != nil {
			t.Fatal(err)
		}
		if role := roleIn(t, st, ctx, owner.ID, first); role != core.WorkspaceRoleViewer {
			t.Fatalf("repeated initialization rewrote the demotion: %q", role)
		}
		later := "fresh-later"
		if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, later), &config.Config{Workspace: later}); err != nil {
			t.Fatal(err)
		}
		if role := roleIn(t, st, ctx, owner.ID, later); role != "" {
			t.Fatalf("populated registry bound the owner as %s", role)
		}
	})
	t.Run("RevokedAndDeactivatedOwner", func(t *testing.T) {
		st := fresh.Open(t)
		ctx := t.Context()
		identity := config.FirstOperatorIdentity{OrganizationName: "Fresh", Email: "owner@example.test", DisplayName: "Owner"}
		_, err := st.BootstrapIdentity(ctx, identity, "fresh-one")
		requireOK(t, err)
		owner, err := st.VerifyPersonalAccessToken(ctx, "fresh-one")
		requireOK(t, err)
		tokens, err := st.ListOwnPersonalAccessTokens(ctx, owner.ID)
		requireOK(t, err)
		markerID := ""
		for _, token := range tokens {
			if token.DeploymentCredential {
				markerID = token.ID
			}
		}
		if markerID == "" {
			t.Fatalf("no deployment marker among %+v", tokens)
		}
		_, err = st.RevokeOwnPersonalAccessToken(userContext(ctx, owner.ID), owner.ID, markerID)
		requireOK(t, err)
		if _, err := st.BootstrapIdentity(ctx, identity, "fresh-one"); err == nil || !strings.Contains(err.Error(), "legacy token revoked") {
			t.Fatalf("revoked deployment token restart err=%v", err)
		}
		if _, err := st.VerifyPersonalAccessToken(ctx, "fresh-one"); err == nil {
			t.Fatal("revoked deployment token authenticates")
		}
		other := config.FirstOperatorIdentity{OrganizationName: "Other", Email: "other@example.test", DisplayName: "Other"}
		if _, err := st.BootstrapIdentity(ctx, other, "fresh-two"); err != nil {
			t.Fatal(err)
		}
		reissued, err := st.VerifyPersonalAccessToken(ctx, "fresh-two")
		requireOK(t, err)
		if reissued.ID != owner.ID {
			t.Fatalf("reissue selected %s, want owner %s", reissued.ID, owner.ID)
		}
		if fresh.DeploymentEvents != nil {
			if healed := fresh.DeploymentEvents(t, st, "identity.legacy_bindings_healed"); healed != 0 {
				t.Fatalf("healing events=%d", healed)
			}
		}
		fresh.Deactivate(t, st, owner.ID)
		if allowed, err := st.AuthorizeInstanceAdministration(ctx, owner.ID); err != nil || allowed {
			t.Fatalf("deactivated owner admitted=%t err=%v", allowed, err)
		}
		if _, err := st.BootstrapIdentity(ctx, other, "fresh-two"); err == nil || !strings.Contains(err.Error(), "deactivated") {
			t.Fatalf("deactivated owner restart err=%v", err)
		}
		if _, err := st.VerifyPersonalAccessToken(ctx, "fresh-two"); err == nil {
			t.Fatal("deactivated owner's deployment token authenticates")
		}
	})
}
