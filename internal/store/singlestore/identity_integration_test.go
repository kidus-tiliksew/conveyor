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
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "identity-fixture")
	cfg := &config.Config{Workspace: "identity-fixture", Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/repo", Base: "main"}}}
	// Identity first: the first workspace of the reset registry binds the
	// deployment owner; bootstrap never heals a later binding (DEC-75(4)).
	if _, err := s.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Fixture", Email: "owner@example.test", DisplayName: "Owner"}, "fixture-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
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
	// Sliding needs a stale last use as well as a near expiry: a session used
	// within the refresh interval is verified without a write.
	if _, err = s.db.ExecContext(ctx, "UPDATE dashboard_sessions SET last_used_at=?,expires_at=? WHERE id=?", time.Now().UTC().Add(-10*time.Minute), time.Now().UTC().Add(time.Hour), session.ID); err != nil {
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

// TestDashboardSessionRefreshIntegration proves on stored rows that a
// verification inside the refresh interval writes nothing and that a stale
// one writes once and stores exactly the expiry it returns. Verification
// never enters identityTx: it records no lock-row acquisition and completes
// while another transaction holds both registry lock rows
// (component-identity-membership).
func TestDashboardSessionRefreshIntegration(t *testing.T) {
	s, ctx, owner := ownedIdentityFixture(t)
	link, err := s.IssueSignInLink(ctx, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := s.RedeemSignInLink(ctx, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	stored := func() (sql.NullTime, time.Time) {
		t.Helper()
		var lastUsed sql.NullTime
		var expires time.Time
		if err := s.db.QueryRowContext(ctx, "SELECT last_used_at,expires_at FROM dashboard_sessions WHERE id=?", session.ID).Scan(&lastUsed, &expires); err != nil {
			t.Fatal(err)
		}
		return lastUsed, expires
	}
	attempts := 0
	verify := func(t *testing.T) core.AuthenticatedCredential {
		t.Helper()
		hooked := store.WithIdentityTestHook(ctx, func(stage string) error {
			if stage == store.IdentityHookSessionRefreshing {
				attempts++
			}
			return nil
		})
		observedCtx, observed := observeLockReturn(hooked)
		auth, err := s.VerifyDashboardSession(observedCtx, session.Value)
		if err != nil {
			t.Fatal(err)
		}
		if !observed.at.IsZero() {
			t.Fatal("session verification acquired a lock row")
		}
		return auth
	}

	// A never-used session counts from its creation, so it is fresh.
	beforeUsed, beforeExpires := stored()
	if beforeUsed.Valid {
		t.Fatalf("minted session last_used_at=%s, want null", beforeUsed.Time)
	}
	for range 2 {
		if auth := verify(t); !auth.SessionExpiresAt.IsZero() {
			t.Fatalf("unused fresh session refreshed to %s", auth.SessionExpiresAt)
		}
	}
	if afterUsed, afterExpires := stored(); afterUsed.Valid || !afterExpires.Equal(beforeExpires) || attempts != 0 {
		t.Fatalf("unused fresh verification wrote: last_used=%v expires %s->%s attempts=%d", afterUsed, beforeExpires, afterExpires, attempts)
	}

	if _, err = s.db.ExecContext(ctx, "UPDATE dashboard_sessions SET last_used_at=DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL 1 MINUTE) WHERE id=?", session.ID); err != nil {
		t.Fatal(err)
	}
	beforeUsed, beforeExpires = stored()
	for range 2 {
		if auth := verify(t); !auth.SessionExpiresAt.IsZero() {
			t.Fatalf("fresh session refreshed to %s", auth.SessionExpiresAt)
		}
	}
	if afterUsed, afterExpires := stored(); !afterUsed.Time.Equal(beforeUsed.Time) || !afterExpires.Equal(beforeExpires) || attempts != 0 {
		t.Fatalf("fresh verification wrote: last_used %s->%s expires %s->%s attempts=%d", beforeUsed.Time, afterUsed.Time, beforeExpires, afterExpires, attempts)
	}

	if _, err = s.db.ExecContext(ctx, "UPDATE dashboard_sessions SET last_used_at=DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL 10 MINUTE),expires_at=DATE_ADD(CURRENT_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE id=?", session.ID); err != nil {
		t.Fatal(err)
	}
	beforeUsed, _ = stored()
	auth := verify(t)
	afterUsed, afterExpires := stored()
	if attempts != 1 || !afterUsed.Time.After(beforeUsed.Time) || !auth.SessionExpiresAt.Equal(afterExpires) || time.Until(afterExpires) < 6*24*time.Hour+23*time.Hour {
		t.Fatalf("stale verification: last_used %s->%s stored=%s output=%s attempts=%d", beforeUsed.Time, afterUsed.Time, afterExpires, auth.SessionExpiresAt, attempts)
	}

	// Another transaction holds both registry lock rows that identityTx
	// takes; fresh and stale verifications still complete. The deadline is
	// only a failure bound for a verification that would wait on them.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, key := range []string{"workspace-registry", "identity-registry"} {
		if err = lockKey(ctx, tx, key); err != nil {
			t.Fatal(err)
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err = s.VerifyDashboardSession(bounded, session.Value); err != nil {
		t.Fatalf("fresh verification under held registry locks: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE dashboard_sessions SET last_used_at=DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL 10 MINUTE) WHERE id=?", session.ID); err != nil {
		t.Fatal(err)
	}
	if refreshed, err := s.VerifyDashboardSession(bounded, session.Value); err != nil || refreshed.SessionExpiresAt.IsZero() {
		t.Fatalf("stale verification under held registry locks: output=%s err=%v", refreshed.SessionExpiresAt, err)
	}
}
