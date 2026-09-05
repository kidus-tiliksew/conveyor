package storetest

import (
	"bytes"
	"context"
	"errors"
	"slices"
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
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{1}, 32))
	_, err = st.StoreForgeToken(ctx, owner.ID, "conformance-user-forge", "owner")
	requireOK(t, err)
	status, err := st.GetForgeTokenStatus(ctx, owner.ID)
	requireOK(t, err)
	if !status.Configured || status.ForgeLogin != "owner" {
		t.Fatal("forge metadata differs")
	}
	credential, err := st.GetForgeTokenForUse(ctx, owner.ID)
	requireOK(t, err)
	if credential.Token != "conformance-user-forge" {
		t.Fatal("forge token round trip differs")
	}
	_, err = st.StoreWorkspaceForgeToken(ctx, x.Workspace, "conformance-workspace-forge", "workspace")
	requireOK(t, err)
	status, err = st.GetWorkspaceForgeTokenStatus(ctx, x.Workspace)
	requireOK(t, err)
	if !status.Configured {
		t.Fatal("workspace token metadata missing")
	}
	workspaceToken, err := st.GetWorkspaceForgeTokenForUse(ctx, x.Workspace)
	requireOK(t, err)
	if workspaceToken.Token != "conformance-workspace-forge" {
		t.Fatal("workspace token round trip differs")
	}
	redaction, err := st.ListForgeTokensForRedaction(ctx)
	requireOK(t, err)
	if !slices.Contains(redaction, "conformance-user-forge") || !slices.Contains(redaction, "conformance-workspace-forge") {
		t.Fatal("redaction list misses configured tokens")
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{2}, 32))
	if _, err := st.GetForgeTokenForUse(ctx, owner.ID); !errors.Is(err, store.ErrForgeTokenDecrypt) {
		t.Fatalf("wrong encryption key error=%v", err)
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{1}, 32))
	requireOK(t, st.DeleteForgeToken(ctx, owner.ID))
	requireOK(t, st.DeleteWorkspaceForgeToken(ctx, x.Workspace))
	redaction, err = st.ListForgeTokensForRedaction(ctx)
	requireOK(t, err)
	if len(redaction) != 0 {
		t.Fatal("deleted forge tokens remain in redaction list")
	}
}
