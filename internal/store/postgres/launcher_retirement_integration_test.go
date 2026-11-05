package postgres

import (
	"context"
	"crypto/sha256"
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

// TestLauncherRetirementMigrationIntegration proves migration 153 removes the
// worker, pairing, model-failure, activity-snapshot, and transcript-capture
// tables and the continuation columns, revokes run-bound agent credentials,
// is idempotent, and leaves worker-actor events readable (task 261101-699ace;
// DEC-97, DEC-94, DEC-98; component-persistence, Launcher-record retirement).
// Migration 158 then revokes every remaining agent row: it runs here because
// this test already holds a migrated store, so the package's time budget gains
// no new schema (DEC-116(3); req-security-boundaries AC-1.3;
// component-identity-membership invariant 35).
func TestLauncherRetirementMigrationIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	var present int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('workers','worker_pairings','harness_model_failures','work_order_activity_snapshots','work_order_transcript_captures')`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Fatalf("%d retired tables remain", present)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'work_orders' AND column_name LIKE 'continuation\_%'`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Fatalf("%d continuation columns remain", present)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'work_orders' AND column_name IN ('worker_id','rate_limit','rate_limit_observed_at')`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 3 {
		t.Fatalf("kept work_orders columns = %d, want worker_id, rate_limit, and rate_limit_observed_at", present)
	}

	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Retirement", Email: "retirement@example.test", DisplayName: "Retirement"}, "retirement-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "retirement-token")
	if err != nil {
		t.Fatal(err)
	}
	runChildID, runChildValue := seedStoredAgentCredential(t, ctx, st, owner.ID, `conveyor-run:{"workspace_id":"demo","work_order_id":"order","session_id":"session"}`, false)
	plainAgentID, plainAgentValue := seedStoredAgentCredential(t, ctx, st, owner.ID, "plain agent", false)
	revokedAgentID, _ := seedStoredAgentCredential(t, ctx, st, owner.ID, "already revoked agent", true)
	// A timestamp far from now, so a migration that moved it cannot pass.
	alreadyRevokedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if _, err = st.pool.Exec(ctx, `UPDATE user_tokens SET revoked_at=$2 WHERE id=$1`, revokedAgentID, alreadyRevokedAt); err != nil {
		t.Fatal(err)
	}
	personal, err := st.IssueOwnPersonalAccessToken(ctx, owner.ID, "person's own token")
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := func(t *testing.T, id string) *time.Time {
		t.Helper()
		var at *time.Time
		if err := st.pool.QueryRow(ctx, `SELECT revoked_at FROM user_tokens WHERE id=$1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	var rowsBefore int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM user_tokens`).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}
	workspace := "retirement-" + core.NewTaskID()
	if _, err = st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, workspace), isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	// A worker-actor event recorded before DEC-97, written as the worker
	// plane wrote it; no current writer can (store.ErrRetiredWorkerActor).
	var eventID int64
	if err = st.pool.QueryRow(ctx, `INSERT INTO events (task_id, kind, actor_id, actor_role, payload_json, at, workspace_id)
		VALUES (NULL, 'worker.enrolled', 'worker:retired-worker', 'runner', '{"worker_id":"retired-worker"}', now(), $1) RETURNING id`, workspace).Scan(&eventID); err != nil {
		t.Fatal(err)
	}

	migration, err := migrationFiles.ReadFile("migrations/153_retire_launcher_records.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = st.pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("reapply migration 153: %v", err)
		}
	}
	if _, err = st.VerifyCredential(ctx, runChildValue); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("run-bound credential after migration err=%v, want refused", err)
	}
	if revokedAt(t, runChildID) == nil {
		t.Fatal("migration 153 left a run-bound credential live")
	}
	// Migration 153 reaches only labelled rows; the plain row waits for 158.
	if at := revokedAt(t, plainAgentID); at != nil {
		t.Fatalf("migration 153 revoked a plain agent row at %s", at)
	}

	revocation, err := migrationFiles.ReadFile("migrations/158_revoke_agent_credentials.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, string(revocation)); err != nil {
		t.Fatalf("apply migration 158: %v", err)
	}
	firstRevocation := revokedAt(t, plainAgentID)
	if firstRevocation == nil {
		t.Fatal("migration 158 left a plain agent row live")
	}
	runChildRevocation := revokedAt(t, runChildID)
	if _, err = st.pool.Exec(ctx, string(revocation)); err != nil {
		t.Fatalf("reapply migration 158: %v", err)
	}
	// The refusal is the unknown credential's own error: no kind-specific code.
	if _, err = st.VerifyCredential(ctx, plainAgentValue); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("plain agent credential after migration err=%v, want refused as revoked", err)
	}
	if _, unknownErr := st.VerifyCredential(ctx, "cv_agent_agt_unknown_fixture-value"); err.Error() != unknownErr.Error() {
		t.Fatalf("plain agent refusal %q differs from an unknown credential's %q", err, unknownErr)
	}
	if at := revokedAt(t, plainAgentID); !at.Equal(*firstRevocation) {
		t.Fatalf("rerun moved a plain agent row's revoked_at from %s to %s", firstRevocation, at)
	}
	if at := revokedAt(t, runChildID); !at.Equal(*runChildRevocation) {
		t.Fatalf("migration 158 moved a run-bound row's revoked_at from %s to %s", runChildRevocation, at)
	}
	if at := revokedAt(t, revokedAgentID); at == nil || !at.Equal(alreadyRevokedAt) {
		t.Fatalf("already revoked agent row revoked_at=%v, want %s", at, alreadyRevokedAt)
	}
	if at := revokedAt(t, personal.ID); at != nil {
		t.Fatalf("migration 158 revoked a person's token at %s", at)
	}
	if credential, verifyErr := st.VerifyCredential(ctx, personal.Value); verifyErr != nil || credential.Kind != core.CredentialUser || credential.OwnerUserID != owner.ID {
		t.Fatalf("person's token after migration=%+v err=%v", credential, verifyErr)
	}
	var rowsAfter, liveAgents int
	if err = st.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE kind='agent' AND revoked_at IS NULL) FROM user_tokens`).Scan(&rowsAfter, &liveAgents); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != rowsBefore || liveAgents != 0 {
		t.Fatalf("user_tokens rows %d->%d with %d live agent rows, want no row deleted and none live", rowsBefore, rowsAfter, liveAgents)
	}
	// An agent row with no revoked_at is refused on its kind alone.
	lateID, lateValue := seedStoredAgentCredential(t, ctx, st, owner.ID, "agent row after the migration", false)
	if _, err = st.VerifyCredential(ctx, lateValue); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("agent row with NULL revoked_at err=%v, want refused", err)
	}
	if at := revokedAt(t, lateID); at != nil {
		t.Fatalf("verification wrote revoked_at %s on an agent row", at)
	}
	// The person's own credentials keep working after the migration: the
	// deployment token, a personal access token, and a dashboard session.
	if credential, verifyErr := st.VerifyCredential(ctx, "retirement-token"); verifyErr != nil || credential.OwnerUserID != owner.ID {
		t.Fatalf("deployment token after migration=%+v err=%v", credential, verifyErr)
	}
	link, err := st.IssueSignInLink(ctx, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := st.RedeemSignInLink(ctx, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	if credential, verifyErr := st.VerifyDashboardSession(ctx, session.Value); verifyErr != nil || credential.OwnerUserID != owner.ID || credential.Kind != core.CredentialUser {
		t.Fatalf("dashboard session after migration=%+v err=%v", credential, verifyErr)
	}
	// Over HTTP the refused row is indistinguishable from an unknown bearer, on
	// the MCP mount and on a REST route, and the person's token still authenticates.
	handler := httpapi.NewServer(st).Handler()
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
	owners, err := st.AgentActorOwners(ctx, []string{runChildID, plainAgentID, revokedAgentID, lateID, personal.ID, "agt_unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{runChildID: owner.ID, plainAgentID: owner.ID, revokedAgentID: owner.ID, lateID: owner.ID}; !maps.Equal(owners, want) {
		t.Fatalf("agent actor owners=%v, want %v", owners, want)
	}
	var actorID, actorRole, payload string
	if err = st.pool.QueryRow(ctx, `SELECT actor_id, actor_role, payload_json::text FROM events WHERE id = $1`, eventID).Scan(&actorID, &actorRole, &payload); err != nil {
		t.Fatal(err)
	}
	if actorID != "worker:retired-worker" || actorRole != string(core.ActorWorker) || payload != `{"worker_id": "retired-worker"}` {
		t.Fatalf("worker-actor event read back as %s/%s %s", actorID, actorRole, payload)
	}
}

// seedStoredAgentCredential inserts one user_tokens row of kind agent by raw SQL,
// as the retired class stored it. Nothing in the store can issue one (DEC-116(1)).
func seedStoredAgentCredential(t *testing.T, ctx context.Context, st *Store, userID, label string, revoked bool) (string, string) {
	t.Helper()
	id := "agt_" + core.NewTaskID()
	value := "cv_agent_" + id + "_fixture-value"
	hash := sha256.Sum256([]byte(value))
	var revokedAt *time.Time
	if revoked {
		now := time.Now().UTC()
		revokedAt = &now
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO user_tokens(id,user_id,label,token_hash,kind,scope,revoked_at) VALUES($1,$2,$3,$4,'agent','user',$5)`, id, userID, label, hash[:], revokedAt); err != nil {
		t.Fatalf("seed stored agent credential: %v", err)
	}
	return id, value
}
