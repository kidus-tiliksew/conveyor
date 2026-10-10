package store

// Native checks of scoped invitation resend state in the volatile backend
// (component-identity-membership). The volatile store validates and prepares
// every fallible step before it mutates state, so a refusal or an audit
// failure leaves links and events exactly as they were.

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func volatileResendFixture(t *testing.T) (*volatileMemory, context.Context, core.IdentityUser) {
	t.Helper()
	m := NewVolatileBackend().(*volatileMemory)
	t.Cleanup(m.Close)
	if _, err := m.BootstrapWorkspaceConfig(WithWorkspace(t.Context(), "alpha"), &config.Config{Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "Volatile", Email: "owner@example.test", DisplayName: "Owner"}, "volatile-owner"); err != nil {
		t.Fatal(err)
	}
	owner, err := m.VerifyPersonalAccessToken(t.Context(), "volatile-owner")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithActor(WithCredential(t.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}), Actor{ID: UserActorID(owner.ID), Role: core.ActorUser})
	return m, ctx, owner
}

func (m *volatileMemory) snapshotResendState() (map[string]signInLink, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return maps.Clone(m.signInLinks), len(m.events[""])
}

func TestVolatileInvitationResendLeavesStateOnRefusalAndAuditFailure(t *testing.T) {
	m, ctx, owner := volatileResendFixture(t)
	email := "volatile-resend@example.test"
	if _, err := m.GrantWorkspaceRole(ctx, email, "alpha", core.WorkspaceRoleViewer); err != nil {
		t.Fatal(err)
	}
	if _, err := m.IssueInvitationSignInLink(ctx, "alpha", email, SignInLinkInvitation); err != nil {
		t.Fatal(err)
	}
	links, events := m.snapshotResendState()
	fault := errors.New("injected resend audit failure")
	faultCtx := WithIdentityTestHook(ctx, func(stage string) error {
		if stage == IdentityHookInvitationResentAudit {
			return fault
		}
		return nil
	})
	for _, attempt := range []struct {
		name      string
		ctx       context.Context
		workspace string
		email     string
		want      error
	}{
		{"foreign workspace", ctx, "beta", email, ErrNotFound},
		{"unknown email", ctx, "alpha", "volatile-unknown@example.test", ErrNotFound},
		{"existing account", ctx, "alpha", owner.Email, ErrNotFound},
		{"audit failure", faultCtx, "alpha", email, fault},
	} {
		if _, err := m.IssueInvitationSignInLink(attempt.ctx, attempt.workspace, attempt.email, SignInLinkResend); !errors.Is(err, attempt.want) {
			t.Fatalf("%s error=%v want %v", attempt.name, err, attempt.want)
		}
		afterLinks, afterEvents := m.snapshotResendState()
		if !maps.EqualFunc(links, afterLinks, func(a, b signInLink) bool { return a.ID == b.ID && (a.RedeemedAt == nil) == (b.RedeemedAt == nil) }) || afterEvents != events {
			t.Fatalf("%s changed state: links %d->%d events %d->%d", attempt.name, len(links), len(afterLinks), events, afterEvents)
		}
	}

	if _, err := m.IssueInvitationSignInLink(ctx, "alpha", email, SignInLinkResend); err != nil {
		t.Fatal(err)
	}
	afterLinks, afterEvents := m.snapshotResendState()
	unredeemed := 0
	for _, link := range afterLinks {
		if link.Email == email && link.RedeemedAt == nil {
			unredeemed++
		}
	}
	// One new link, one deployment issuance event, and one resend event.
	if len(afterLinks) != len(links)+1 || unredeemed != 1 || afterEvents != events+2 {
		t.Fatalf("resend state links=%d->%d unredeemed=%d events=%d->%d", len(links), len(afterLinks), unredeemed, events, afterEvents)
	}
	m.mu.RLock()
	last := m.events[""][len(m.events[""])-1]
	m.mu.RUnlock()
	if last.Kind != "workspace.invitation_resent" || last.ActorID != UserActorID(owner.ID) || last.ActorRole != core.ActorUser {
		t.Fatalf("last event=%+v", last)
	}
}
