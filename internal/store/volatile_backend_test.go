package store

import (
	"bytes"
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestVolatileCapabilitiesPreserveCredentialAndMembershipBoundaries(t *testing.T) {
	st := NewVolatileBackend()
	ctx := WithWorkspace(t.Context(), "one")
	first := config.FirstOperatorIdentity{OrganizationName: "Test", Email: "owner@example.test", DisplayName: "Owner"}
	if _, err := st.BootstrapIdentity(ctx, first, "fixture-token"); err != nil {
		t.Fatal(err)
	}
	if seeded, err := st.BootstrapIdentity(ctx, first, "fixture-token"); err != nil || seeded {
		t.Fatalf("repeat bootstrap: %v %v", seeded, err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithCredential(ctx, core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	ctx = WithActor(ctx, Actor{ID: UserActorID(owner.ID), Role: core.ActorUser})
	cfg := &config.Config{Workspace: "one", Repos: []config.Repo{{Name: "repo", Base: "main"}}}
	if _, err := st.CreateWorkspace(ctx, "one", "One", cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeWorkspaceRole(ctx, owner.ID, "one"); !errors.Is(err, ErrLastWorkspaceOperator) {
		t.Fatalf("sole operator revoked: %v", err)
	}
	if _, err := st.GetCallerIdentity(ctx, owner.ID, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbound identity exposed: %v", err)
	}
	if _, err := st.GrantWorkspaceRole(ctx, "invitee@example.test", "one", core.WorkspaceRoleExecutor); err != nil {
		t.Fatal(err)
	}
	link, err := st.IssueSignInLink(ctx, "invitee@example.test")
	if err != nil {
		t.Fatal(err)
	}
	session, invitee, err := st.RedeemSignInLink(ctx, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.AuthorizeWorkspace(ctx, invitee.ID, "one", core.CapabilityClaimWork); err != nil || !allowed {
		t.Fatalf("invitation binding: %v %v", allowed, err)
	}
	if err := st.SetOwnPassword(ctx, invitee.ID, session.ID, "", "a long fixture password"); err != nil {
		t.Fatal(err)
	}
	passwordSession, _, err := st.SignInWithPassword(ctx, invitee.Email, "a long fixture password")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetOwnPassword(ctx, invitee.ID, passwordSession.ID, "wrong", "a changed fixture password"); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("password proof bypassed: %v", err)
	}
	token, err := st.IssueOwnPersonalAccessToken(ctx, invitee.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeOwnPersonalAccessToken(ctx, owner.ID, token.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's token was addressable: %v", err)
	}
	agent, err := st.IssueAgentCredential(ctx, owner.ID, "test agent")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := st.VerifyCredential(ctx, agent.Value)
	if err != nil || auth.Scope != core.CredentialScopeUser || auth.Kind != core.CredentialAgent {
		t.Fatalf("agent scope: %+v %v", auth, err)
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{1}, 32))
	if _, err := st.StoreForgeToken(ctx, invitee.ID, "fixture-forge-token", "invitee"); err != nil {
		t.Fatal(err)
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{2}, 32))
	if _, err := st.GetForgeTokenForUse(ctx, invitee.ID); !errors.Is(err, ErrForgeTokenDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{1}, 32))
	if _, err := st.GetForgeTokenForUse(ctx, invitee.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeWorkspaceRole(ctx, invitee.ID, "one"); err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.AuthorizeWorkspace(ctx, invitee.ID, "one", core.CapabilityClaimWork); err != nil || allowed {
		t.Fatalf("revoked membership: %v %v", allowed, err)
	}
}

func TestMemoryConstructorRetainsExistingCapabilities(t *testing.T) {
	if _, ok := NewMemory().(Backend); ok {
		t.Fatal("base memory fixtures unexpectedly expose deployment capabilities")
	}
}

func TestMemoryDispatchJobConflict(t *testing.T) {
	st := NewMemory()
	ctx := WithWorkspace(t.Context(), "test")
	if err := st.CreateTask(ctx, core.Task{ID: "task", Workspace: "test", Branch: "task-branch", State: core.TaskQueued}); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "job", TaskID: "task", State: core.JobRunning}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); !errors.Is(err, ErrDispatchJobConflict) {
		t.Fatalf("duplicate job: %v", err)
	}
}
