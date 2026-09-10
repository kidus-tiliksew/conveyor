package singlestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func ownedIdentityFixture(t *testing.T) (*Store, context.Context, core.IdentityUser) {
	t.Helper()
	s := integrationStore(t)
	ctx := store.WithWorkspace(t.Context(), "identity-fixture")
	cfg := &config.Config{Workspace: "identity-fixture", Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/repo", Base: "main"}}}
	if _, err := s.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Fixture", Email: "owner@example.test", DisplayName: "Owner"}, "fixture-bootstrap"); err != nil {
		t.Fatal(err)
	}
	owner, err := s.VerifyPersonalAccessToken(ctx, "fixture-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	ctx = store.WithCredential(ctx, core.AuthenticatedCredential{ID: "fixture", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	ctx = userActorContext(ctx, owner.ID)
	return s, ctx, owner
}
func TestIdentitySecurityIntegration(t *testing.T) {
	s, ctx, owner := ownedIdentityFixture(t)
	// Parallel provisioning and bootstrap must still produce one account and organization.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ProvisionIdentityUser(ctx, " SAME@example.test ", "Same"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE email='same@example.test'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("global email count=%d error=%v", count, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM orgs").Scan(&count); err != nil || count != 1 {
		t.Fatal("organization singleton missing")
	}
	err := s.identityTx(ctx, func(tx *sql.Tx) error {
		_, err := writeRow(ctx, tx, rowWrite{table: "user_tokens", operation: "INSERT", values: map[string]any{"id": "duplicate", "user_id": owner.ID, "token_hash": []byte("fixture"), "kind": "user", "scope": "operator", "deployment_credential": true}})
		return err
	})
	if err == nil {
		t.Fatal("second deployment credential accepted")
	}
	issued, err := s.IssueOwnPersonalAccessToken(ctx, owner.ID, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var hash []byte
	var kind, scope string
	if err = s.db.QueryRowContext(ctx, "SELECT token_hash,kind,scope FROM user_tokens WHERE id=?", issued.ID).Scan(&hash, &kind, &scope); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(issued.Value))
	if !bytes.Equal(hash, want[:]) || kind != "user" || scope != "operator" {
		t.Fatal("credential metadata or hash differs")
	}
	// The audit write fails after the profile mutation and must roll it back.
	link, err := s.IssueSignInLink(ctx, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := s.RedeemSignInLink(ctx, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	badCtx := store.WithActor(ctx, store.Actor{ID: string([]byte{0xff}), Role: core.ActorUser})
	if _, err = s.SetOwnDisplayName(badCtx, owner.ID, session.ID, "must roll back"); err == nil {
		t.Fatal("invalid audit actor accepted")
	}
	current, err := s.GetCallerIdentity(ctx, owner.ID, "")
	if err != nil || current.DisplayName != "Owner" {
		t.Fatal("failed audit committed profile")
	}
	// Both session identifiers and active owner status participate in every use.
	if _, err = s.SetOwnDisplayName(ctx, "foreign", session.ID, "foreign"); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatal("cross-owner session accepted")
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE dashboard_sessions SET expires_at=? WHERE id=?", time.Now().UTC().Add(time.Hour), session.ID); err != nil {
		t.Fatal(err)
	}
	auth, err := s.VerifyDashboardSession(ctx, session.Value)
	if err != nil || time.Until(auth.SessionExpiresAt) < 6*24*time.Hour {
		t.Fatal("session did not slide seven days")
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE users SET status='deactivated' WHERE id=?", owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.VerifyCredential(ctx, issued.Value); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatal("inactive token authenticates")
	}
	if _, err = s.VerifyDashboardSession(ctx, session.Value); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatal("inactive session authenticates")
	}
}
func TestSessionAndMembershipConcurrencyIntegration(t *testing.T) {
	s, ctx, owner := ownedIdentityFixture(t)
	link, err := s.IssueSignInLink(ctx, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	wins := make(chan bool, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, err := s.RedeemSignInLink(ctx, link.Value); wins <- err == nil }()
	}
	wg.Wait()
	close(wins)
	count := 0
	for win := range wins {
		if win {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("single-use link winners=%d", count)
	}
	member, err := s.ProvisionIdentityUser(ctx, "second@example.test", "Second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.GrantWorkspaceRole(ctx, member.Email, "identity-fixture", core.WorkspaceRoleOperator); err != nil {
		t.Fatal(err)
	}
	outcomes := make(chan error, 2)
	for _, uid := range []string{owner.ID, member.ID} {
		wg.Add(1)
		go func() { defer wg.Done(); outcomes <- s.RevokeWorkspaceRole(ctx, uid, "identity-fixture") }()
	}
	wg.Wait()
	close(outcomes)
	success, guard := 0, 0
	for err := range outcomes {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrLastWorkspaceOperator) {
			guard++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || guard != 1 {
		t.Fatal("concurrent revocation lost last operator")
	}
}
