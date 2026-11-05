package singlestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestLauncherRetirementMigrationIntegration proves migration 0028 and its
// column hook remove the worker, pairing, activity-snapshot, and
// transcript-capture tables and the continuation columns, revoke run-bound
// agent credentials, converge on retry, and leave worker-actor events readable
// (task 261101-699ace; DEC-97, DEC-94, DEC-98; component-persistence,
// Launcher-record retirement). Migration 0033 then revokes every remaining agent
// row: it runs here because this test already holds a migrated store (DEC-116(3);
// req-security-boundaries AC-1.3; component-identity-membership invariant 35).
func TestLauncherRetirementMigrationIntegration(t *testing.T) {
	s := integrationStore(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	retiredTables := "('workers','worker_pairings','work_order_activity_snapshots','work_order_transcript_captures')"
	continuationColumns := "('continuation_session_id','continuation_attempt_id','continuation_harness','continuation_launch_environment')"
	requireAbsent := func(stage string) {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN "+retiredTables).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: retired tables=%d err=%v", stage, count, err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='work_orders' AND column_name IN "+continuationColumns).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: continuation columns=%d err=%v", stage, count, err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='work_orders' AND column_name IN ('worker_id','rate_limit','rate_limit_observed_at')").Scan(&count); err != nil || count != 3 {
			t.Fatalf("%s: kept work_orders columns=%d err=%v", stage, count, err)
		}
	}
	requireAbsent("fresh migration")

	// Restore the retired tables and columns from the published schema to model
	// a pre-upgrade database, then apply the hook and file twice.
	raw, err := migrationFiles.ReadFile("migrations/0001_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(string(raw), ";") {
		for _, table := range []string{"workers", "worker_pairings", "work_order_activity_snapshots", "work_order_transcript_captures"} {
			if strings.Contains(statement, "CREATE ROWSTORE TABLE IF NOT EXISTS `"+table+"`") {
				if _, err := s.db.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, column := range []string{"continuation_session_id", "continuation_attempt_id", "continuation_harness", "continuation_launch_environment"} {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE work_orders ADD COLUMN "+column+" LONGTEXT NOT NULL DEFAULT ''"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Retirement", Email: "retirement@example.test", DisplayName: "Retirement"}, "retirement-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := s.VerifyPersonalAccessToken(ctx, "retirement-token")
	if err != nil {
		t.Fatal(err)
	}
	runChildID, runChildValue := seedStoredAgentCredential(t, ctx, s, owner.ID, `conveyor-run:{"workspace_id":"demo","work_order_id":"order","session_id":"session"}`, false)
	plainAgentID, plainAgentValue := seedStoredAgentCredential(t, ctx, s, owner.ID, "plain agent", false)
	revokedAgentID, _ := seedStoredAgentCredential(t, ctx, s, owner.ID, "already revoked agent", true)
	// A timestamp far from now, so a migration that moved it cannot pass.
	alreadyRevokedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if _, err = s.db.ExecContext(ctx, `UPDATE user_tokens SET revoked_at=? WHERE id=?`, alreadyRevokedAt, revokedAgentID); err != nil {
		t.Fatal(err)
	}
	personal, err := s.IssueOwnPersonalAccessToken(ctx, owner.ID, "person's own token")
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := func(t *testing.T, id string) sql.NullTime {
		t.Helper()
		var at sql.NullTime
		if err := s.db.QueryRowContext(ctx, `SELECT revoked_at FROM user_tokens WHERE id=?`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	var rowsBefore int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM user_tokens`).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}
	// A worker-actor event recorded before DEC-97; no current writer can.
	if _, err = s.db.ExecContext(ctx, "INSERT INTO deployment_events(kind,actor_id,actor_role,payload_json,at) VALUES('worker.revoked','worker:retired-worker','runner','{\"worker_id\":\"retired-worker\"}',CURRENT_TIMESTAMP(6))"); err != nil {
		t.Fatal(err)
	}
	file, err := migrationFiles.ReadFile("migrations/0028_retire_launcher_records.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateLauncherRetirement(ctx); err != nil {
			t.Fatal(err)
		}
		for _, statement := range strings.Split(string(file), ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	requireAbsent("reapplied migration")
	if _, err = s.VerifyCredential(ctx, runChildValue); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("run-bound credential after migration err=%v, want invalid", err)
	}
	if !revokedAt(t, runChildID).Valid {
		t.Fatal("migration 0028 left a run-bound credential live")
	}
	// Migration 0028 reaches only labelled rows; the plain row waits for 0033.
	if at := revokedAt(t, plainAgentID); at.Valid {
		t.Fatalf("migration 0028 revoked a plain agent row at %s", at.Time)
	}

	revocation, err := migrationFiles.ReadFile("migrations/0033_revoke_agent_credentials.sql")
	if err != nil {
		t.Fatal(err)
	}
	applyRevocation := func(t *testing.T) {
		t.Helper()
		for _, statement := range strings.Split(string(revocation), ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				t.Fatalf("apply migration 0033: %v", err)
			}
		}
	}
	applyRevocation(t)
	firstRevocation := revokedAt(t, plainAgentID)
	if !firstRevocation.Valid {
		t.Fatal("migration 0033 left a plain agent row live")
	}
	runChildRevocation := revokedAt(t, runChildID)
	applyRevocation(t)
	// The refusal is the unknown credential's own error: no kind-specific code.
	if _, err = s.VerifyCredential(ctx, plainAgentValue); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("plain agent credential after migration err=%v, want refused as revoked", err)
	}
	if _, unknownErr := s.VerifyCredential(ctx, "cv_agent_agt_unknown_fixture-value"); err.Error() != unknownErr.Error() {
		t.Fatalf("plain agent refusal %q differs from an unknown credential's %q", err, unknownErr)
	}
	if at := revokedAt(t, plainAgentID); !at.Time.Equal(firstRevocation.Time) {
		t.Fatalf("rerun moved a plain agent row's revoked_at from %s to %s", firstRevocation.Time, at.Time)
	}
	if at := revokedAt(t, runChildID); !at.Time.Equal(runChildRevocation.Time) {
		t.Fatalf("migration 0033 moved a run-bound row's revoked_at from %s to %s", runChildRevocation.Time, at.Time)
	}
	if at := revokedAt(t, revokedAgentID); !at.Valid || !at.Time.Equal(alreadyRevokedAt) {
		t.Fatalf("already revoked agent row revoked_at=%v, want %s", at, alreadyRevokedAt)
	}
	if at := revokedAt(t, personal.ID); at.Valid {
		t.Fatalf("migration 0033 revoked a person's token at %s", at.Time)
	}
	if credential, verifyErr := s.VerifyCredential(ctx, personal.Value); verifyErr != nil || credential.Kind != core.CredentialUser || credential.OwnerUserID != owner.ID {
		t.Fatalf("person's token after migration=%+v err=%v", credential, verifyErr)
	}
	var rowsAfter, liveAgents int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*), COALESCE(SUM(kind='agent' AND revoked_at IS NULL),0) FROM user_tokens`).Scan(&rowsAfter, &liveAgents); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != rowsBefore || liveAgents != 0 {
		t.Fatalf("user_tokens rows %d->%d with %d live agent rows, want no row deleted and none live", rowsBefore, rowsAfter, liveAgents)
	}
	// An agent row with no revoked_at is refused on its kind alone.
	lateID, lateValue := seedStoredAgentCredential(t, ctx, s, owner.ID, "agent row after the migration", false)
	if _, err = s.VerifyCredential(ctx, lateValue); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("agent row with NULL revoked_at err=%v, want refused", err)
	}
	if at := revokedAt(t, lateID); at.Valid {
		t.Fatalf("verification wrote revoked_at %s on an agent row", at.Time)
	}
	// The person's own credentials keep working after the migration: the
	// deployment token, a personal access token, and a dashboard session.
	if credential, verifyErr := s.VerifyCredential(ctx, "retirement-token"); verifyErr != nil || credential.OwnerUserID != owner.ID {
		t.Fatalf("deployment token after migration=%+v err=%v", credential, verifyErr)
	}
	link, err := s.IssueSignInLink(ctx, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := s.RedeemSignInLink(ctx, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	if credential, verifyErr := s.VerifyDashboardSession(ctx, session.Value); verifyErr != nil || credential.OwnerUserID != owner.ID || credential.Kind != core.CredentialUser {
		t.Fatalf("dashboard session after migration=%+v err=%v", credential, verifyErr)
	}
	// Over HTTP the refused row is indistinguishable from an unknown bearer, on
	// the MCP mount and on a REST route, and the person's token still authenticates.
	handler := httpapi.NewServer(s).Handler()
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	const toolsList = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, route := range [][3]string{{http.MethodPost, "/mcp", toolsList}, {http.MethodGet, "/v1/me", ""}} {
		refused, unknown := call(route[0], route[1], route[2], plainAgentValue), call(route[0], route[1], route[2], "cv_agent_agt_unknown_fixture-value")
		if refused.Code != http.StatusUnauthorized || refused.Body.String() != unknown.Body.String() {
			t.Fatalf("%s agent row status=%d body=%q, unknown status=%d body=%q", route[1], refused.Code, refused.Body.String(), unknown.Code, unknown.Body.String())
		}
		if accepted := call(route[0], route[1], route[2], personal.Value); accepted.Code != http.StatusOK {
			t.Fatalf("%s person's token status=%d body=%s", route[1], accepted.Code, accepted.Body.String())
		}
	}
	// Acts under every stored agent row, revoked or not, resolve to the owner.
	owners, err := s.AgentActorOwners(ctx, []string{runChildID, plainAgentID, revokedAgentID, lateID, personal.ID, "agt_unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{runChildID: owner.ID, plainAgentID: owner.ID, revokedAgentID: owner.ID, lateID: owner.ID}; !maps.Equal(owners, want) {
		t.Fatalf("agent actor owners=%v, want %v", owners, want)
	}
	var actorID, actorRole string
	if err = s.db.QueryRowContext(ctx, "SELECT actor_id,actor_role FROM deployment_events WHERE kind='worker.revoked' AND actor_id='worker:retired-worker'").Scan(&actorID, &actorRole); err != nil {
		t.Fatal(err)
	}
	if actorRole != string(core.ActorWorker) {
		t.Fatalf("worker-actor event read back as %s/%s", actorID, actorRole)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

// seedStoredAgentCredential inserts one user_tokens row of kind agent by raw SQL,
// as the retired class stored it. Nothing in the store can issue one (DEC-116(1)).
func seedStoredAgentCredential(t *testing.T, ctx context.Context, s *Store, userID, label string, revoked bool) (string, string) {
	t.Helper()
	id := "agt_" + core.NewTaskID()
	value := "cv_agent_" + id + "_fixture-value"
	hash := sha256.Sum256([]byte(value))
	var revokedAt any
	if revoked {
		revokedAt = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_tokens(id,user_id,label,token_hash,kind,scope,deployment_credential,created_at,revoked_at) VALUES(?,?,?,?,'agent','user',FALSE,CURRENT_TIMESTAMP(6),?)`, id, userID, label, hash[:], revokedAt); err != nil {
		t.Fatalf("seed stored agent credential: %v", err)
	}
	return id, value
}
