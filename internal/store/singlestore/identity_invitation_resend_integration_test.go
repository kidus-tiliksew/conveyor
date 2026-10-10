package singlestore

// Native checks of scoped invitation resend rows on SingleStore
// (component-identity-membership; req-accounts-and-membership AC-2.1, AC-3.1,
// AC-4.3). Shared behavior lives in storetest/identity.go.

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type resendRows struct {
	tokens, unredeemed, issuedEvents, resentEvents int
}

func countResendRows(t *testing.T, s *Store, ctx context.Context, workspace, email string) resendRows {
	t.Helper()
	var rows resendRows
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM invitation_signin_tokens WHERE email=?),
		(SELECT COUNT(*) FROM invitation_signin_tokens WHERE email=? AND redeemed_at IS NULL),
		(SELECT COUNT(*) FROM deployment_events WHERE kind='identity.signin_link_issued' AND JSON_EXTRACT_STRING(payload_json,'email')=?),
		(SELECT COUNT(*) FROM events WHERE workspace_id=? AND task_id IS NULL AND kind='workspace.invitation_resent' AND JSON_EXTRACT_STRING(payload_json,'email')=?)`,
		email, email, email, workspace, email).Scan(&rows.tokens, &rows.unredeemed, &rows.issuedEvents, &rows.resentEvents); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSingleStoreInvitationResendRowsIntegration(t *testing.T) {
	s, ctx, owner := ownedIdentityFixture(t)
	workspace := "identity-fixture"
	email := "native-resend@example.test"
	if _, err := s.GrantWorkspaceRole(ctx, email, workspace, core.WorkspaceRoleViewer); err != nil {
		t.Fatal(err)
	}
	initial, err := s.IssueInvitationSignInLink(ctx, workspace, email, store.SignInLinkInvitation)
	if err != nil {
		t.Fatal(err)
	}
	if got := countResendRows(t, s, ctx, workspace, email); got != (resendRows{tokens: 1, unredeemed: 1, issuedEvents: 1, resentEvents: 0}) {
		t.Fatalf("after initial issuance rows=%+v", got)
	}

	// A refused resend in another workspace changes no row.
	if _, err := s.IssueInvitationSignInLink(ctx, workspace+"-absent", email, store.SignInLinkResend); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign resend error=%v", err)
	}
	// A failure at the audit append rolls back the token and its rotation.
	fault := errors.New("injected resend audit failure")
	faultCtx := store.WithIdentityTestHook(ctx, func(stage string) error {
		if stage == store.IdentityHookInvitationResentAudit {
			return fault
		}
		return nil
	})
	if _, err := s.IssueInvitationSignInLink(faultCtx, workspace, email, store.SignInLinkResend); !errors.Is(err, fault) {
		t.Fatalf("faulted resend error=%v", err)
	}
	if got := countResendRows(t, s, ctx, workspace, email); got != (resendRows{tokens: 1, unredeemed: 1, issuedEvents: 1, resentEvents: 0}) {
		t.Fatalf("after refusals rows=%+v", got)
	}

	resent, err := s.IssueInvitationSignInLink(ctx, workspace, email, store.SignInLinkResend)
	if err != nil {
		t.Fatal(err)
	}
	if got := countResendRows(t, s, ctx, workspace, email); got != (resendRows{tokens: 2, unredeemed: 1, issuedEvents: 2, resentEvents: 1}) {
		t.Fatalf("after resend rows=%+v", got)
	}
	var redeemedAt *string
	initialHash := sha256.Sum256([]byte(initial.Value))
	if err := s.db.QueryRowContext(ctx, "SELECT CAST(redeemed_at AS CHAR) FROM invitation_signin_tokens WHERE token_hash=?", initialHash[:]).Scan(&redeemedAt); err != nil || redeemedAt == nil {
		t.Fatalf("initial link not rotated: redeemed_at=%v err=%v", redeemedAt, err)
	}
	var actorID, actorRole, payload string
	if err := s.db.QueryRowContext(ctx, "SELECT actor_id,actor_role,CAST(payload_json AS CHAR) FROM events WHERE workspace_id=? AND kind='workspace.invitation_resent'", workspace).Scan(&actorID, &actorRole, &payload); err != nil {
		t.Fatal(err)
	}
	if actorID != store.UserActorID(owner.ID) || actorRole != string(core.ActorUser) {
		t.Fatalf("resend actor=%s/%s", actorID, actorRole)
	}
	resentHash := sha256.Sum256([]byte(resent.Value))
	for _, secret := range []string{resent.Value, string(resentHash[:])} {
		if secret != "" && strings.Contains(payload, secret) {
			t.Fatalf("resend payload carries link material: %s", payload)
		}
	}

	// Redemption attributes the consumed invitation to the redeemed account.
	_, user, err := s.RedeemSignInLink(ctx, resent.Value)
	if err != nil {
		t.Fatal(err)
	}
	var grantActor, invitedBy string
	if err := s.db.QueryRowContext(ctx, "SELECT actor_id,JSON_EXTRACT_STRING(payload_json,'invited_by') FROM events WHERE workspace_id=? AND kind='workspace.membership_granted' AND JSON_EXTRACT_STRING(payload_json,'user_id')=?", workspace, user.ID).Scan(&grantActor, &invitedBy); err != nil {
		t.Fatal(err)
	}
	if grantActor != store.UserActorID(user.ID) || invitedBy != owner.ID {
		t.Fatalf("redemption grant actor=%s invited_by=%s", grantActor, invitedBy)
	}
}
