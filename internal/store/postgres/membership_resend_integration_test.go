package postgres

// Native checks of scoped invitation resend rows and of migration 143 on
// PostgreSQL (component-identity-membership, component-persistence;
// req-accounts-and-membership AC-2.1, AC-3.1, AC-4.3). Shared behavior lives
// in storetest/identity.go.

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const invitationResentMigrationVersion = 143

type pgResendRows struct {
	tokens, unredeemed, issuedEvents, resentEvents int
}

func countPGResendRows(t *testing.T, st *Store, ctx context.Context, workspace, email string) pgResendRows {
	t.Helper()
	var rows pgResendRows
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM invitation_signin_tokens WHERE email=$1),
		(SELECT count(*) FROM invitation_signin_tokens WHERE email=$1 AND redeemed_at IS NULL),
		(SELECT count(*) FROM deployment_events WHERE kind='identity.signin_link_issued' AND payload_json->>'email'=$1),
		(SELECT count(*) FROM events WHERE workspace_id=$2 AND task_id IS NULL AND kind='workspace.invitation_resent' AND payload_json->>'email'=$1)`,
		email, workspace).Scan(&rows.tokens, &rows.unredeemed, &rows.issuedEvents, &rows.resentEvents); err != nil {
		t.Fatal(err)
	}
	return rows
}

func resendIntegrationOwner(t *testing.T, st *Store, workspace string) (context.Context, core.IdentityUser) {
	t.Helper()
	legacy := "resend-owner-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "Resend Org", Email: "owner@example.test", DisplayName: "Owner"}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithActor(store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err := st.CreateWorkspace(ctx, workspace, workspace, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	return store.WithWorkspace(ctx, workspace), owner
}

func TestInvitationResendRowsIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	workspace := "resend-rows-" + core.NewTaskID()
	ctx, owner := resendIntegrationOwner(t, st, workspace)
	email := "native-resend@example.test"
	if _, err := st.GrantWorkspaceRole(ctx, email, workspace, core.WorkspaceRoleViewer); err != nil {
		t.Fatal(err)
	}
	initial, err := st.IssueInvitationSignInLink(ctx, workspace, email, store.SignInLinkInvitation)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPGResendRows(t, st, ctx, workspace, email); got != (pgResendRows{tokens: 1, unredeemed: 1, issuedEvents: 1}) {
		t.Fatalf("after initial issuance rows=%+v", got)
	}
	if _, err := st.IssueInvitationSignInLink(ctx, workspace+"-absent", email, store.SignInLinkResend); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign resend error=%v", err)
	}
	fault := errors.New("injected resend audit failure")
	faultCtx := store.WithIdentityTestHook(ctx, func(stage string) error {
		if stage == store.IdentityHookInvitationResentAudit {
			return fault
		}
		return nil
	})
	if _, err := st.IssueInvitationSignInLink(faultCtx, workspace, email, store.SignInLinkResend); !errors.Is(err, fault) {
		t.Fatalf("faulted resend error=%v", err)
	}
	if got := countPGResendRows(t, st, ctx, workspace, email); got != (pgResendRows{tokens: 1, unredeemed: 1, issuedEvents: 1}) {
		t.Fatalf("after refusals rows=%+v", got)
	}

	resent, err := st.IssueInvitationSignInLink(ctx, workspace, email, store.SignInLinkResend)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPGResendRows(t, st, ctx, workspace, email); got != (pgResendRows{tokens: 2, unredeemed: 1, issuedEvents: 2, resentEvents: 1}) {
		t.Fatalf("after resend rows=%+v", got)
	}
	initialHash := sha256.Sum256([]byte(initial.Value))
	var rotated bool
	if err := st.pool.QueryRow(ctx, `SELECT redeemed_at IS NOT NULL FROM invitation_signin_tokens WHERE token_hash=$1`, initialHash[:]).Scan(&rotated); err != nil || !rotated {
		t.Fatalf("initial link rotated=%v err=%v", rotated, err)
	}
	var actorID, actorRole, payload string
	if err := st.pool.QueryRow(ctx, `SELECT actor_id,actor_role,payload_json::text FROM events WHERE workspace_id=$1 AND kind='workspace.invitation_resent'`, workspace).Scan(&actorID, &actorRole, &payload); err != nil {
		t.Fatal(err)
	}
	if actorID != store.UserActorID(owner.ID) || actorRole != string(core.ActorUser) {
		t.Fatalf("resend actor=%s/%s", actorID, actorRole)
	}
	if strings.Contains(payload, resent.Value) || strings.Contains(payload, "cv_signin_") || strings.Contains(payload, "sign-in") {
		t.Fatalf("resend payload carries link material: %s", payload)
	}

	_, user, err := st.RedeemSignInLink(ctx, resent.Value)
	if err != nil {
		t.Fatal(err)
	}
	var grantActor, invitedBy, grantedBy string
	if err := st.pool.QueryRow(ctx, `SELECT actor_id,payload_json->>'invited_by',payload_json->>'granted_by' FROM events
		WHERE workspace_id=$1 AND kind='workspace.membership_granted' AND payload_json->>'redemption'='true' AND payload_json->>'user_id'=$2`, workspace, user.ID).Scan(&grantActor, &invitedBy, &grantedBy); err != nil {
		t.Fatal(err)
	}
	if grantActor != store.UserActorID(user.ID) || invitedBy != owner.ID || grantedBy != owner.ID {
		t.Fatalf("redemption grant actor=%s invited_by=%s granted_by=%s", grantActor, invitedBy, grantedBy)
	}
}

// TestInvitationResentEventMigrationIntegration upgrades a version-142 schema
// and checks that migration 143 admits the taskless resend event, keeps every
// earlier kind and recorded row, and still refuses an unrelated taskless kind.
func TestInvitationResentEventMigrationIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, invitationResentMigrationVersion-1)
	ctx := t.Context()
	workspace := "resent-migration-" + core.NewTaskID()
	if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, workspace), isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	insert := func(kind string) error {
		_, err := st.pool.Exec(ctx, `INSERT INTO events(workspace_id,kind,actor_id,actor_role,payload_json,at) VALUES($1,$2,'user:fixture','user','{}',now())`, workspace, kind)
		return err
	}
	if err := insert("workspace.invitation_resent"); err == nil || !strings.Contains(err.Error(), "events_scope_check") {
		t.Fatalf("version-142 schema admitted the resend event: %v", err)
	}
	for _, kind := range []string{"workspace.membership_granted", "workspace.membership_revoked", "system_design.title_changed"} {
		if err := insert(kind); err != nil {
			t.Fatalf("version-142 schema refused %s: %v", kind, err)
		}
	}
	before := rowsAsJSON(t, ctx, st.pool, `SELECT id,workspace_id,kind,actor_id,payload_json FROM events WHERE workspace_id=$1`, workspace)

	if err := migrateControlPlaneToVersion(ctx, st.pool, 0); err != nil {
		t.Fatal(err)
	}
	var ledger int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM conveyor_schema_migrations WHERE version=$1 AND name='143_workspace_invitation_resent_event.sql'`, invitationResentMigrationVersion).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("migration 143 ledger rows=%d err=%v", ledger, err)
	}
	if after := rowsAsJSON(t, ctx, st.pool, `SELECT id,workspace_id,kind,actor_id,payload_json FROM events WHERE workspace_id=$1`, workspace); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("recorded events changed:\nbefore=%v\nafter=%v", before, after)
	}
	for _, kind := range []string{"workspace.invitation_resent", "workspace.membership_granted", "workspace.membership_revoked", "requirement.title_changed", "system_design.title_changed"} {
		if err := insert(kind); err != nil {
			t.Fatalf("migrated schema refused %s: %v", kind, err)
		}
	}
	if err := insert("workspace.unrelated_kind"); err == nil || !strings.Contains(err.Error(), "events_scope_check") {
		t.Fatalf("migrated schema admitted an unrelated taskless kind: %v", err)
	}
}
