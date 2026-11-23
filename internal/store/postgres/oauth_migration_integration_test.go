package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestMCPOAuthMigrationIntegration migrates a schema through migration 176
// from version 124, where it holds a row of a deployment kind that migration
// 125 retired. The
// new deployment_events CHECK admits every kind migration 125 admits plus the
// three OAuth kinds, refuses an unknown kind, and keeps the historical row.
// The OAuth tables refuse a digest that is not 32 bytes
// (component-persistence invariants 22, 34, and 41; req-security-boundaries
// AC-2.1; component-identity-membership invariants 36, 53, and 54).
func TestMCPOAuthMigrationIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 124)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	if _, err := st.pool.Exec(ctx, `INSERT INTO deployment_events(kind,actor_id,actor_role,payload_json) VALUES('identity.forge_token_stored','historical','user','{}')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(ctx, st.pool); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	if err := st.pool.QueryRow(ctx, `SELECT max(version) FROM conveyor_schema_migrations`).Scan(&version); err != nil || version < 176 {
		t.Fatalf("schema version=%d err=%v, want at least 176", version, err)
	}
	for _, kind := range []string{
		"identity.legacy_token_rotated", "identity.legacy_bindings_healed",
		"identity.personal_token_issued", "identity.personal_token_revoked",
		"identity.signin_link_issued", "identity.signin_link_redeemed",
		"identity.invitation_delivery_sent", "identity.invitation_delivery_failed",
		"identity.invitation_delivery_fallback",
		"identity.dashboard_session_created", "identity.dashboard_session_revoked",
		"identity.password_set", "identity.password_changed", "identity.display_name_changed",
		"identity.oauth_grant_created", "identity.oauth_grant_revoked", "identity.oauth_refresh_reuse_detected",
	} {
		if _, err := st.pool.Exec(ctx, `INSERT INTO deployment_events(kind,actor_id,actor_role,payload_json) VALUES($1,'system','system','{}')`, kind); err != nil {
			t.Fatalf("kind %s refused: %v", kind, err)
		}
	}
	requireCheckViolation := func(t *testing.T, err error, what string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s err=%v, want a check violation", what, err)
		}
	}
	_, err := st.pool.Exec(ctx, `INSERT INTO deployment_events(kind,actor_id,actor_role,payload_json) VALUES('identity.oauth_token_issued','system','system','{}')`)
	requireCheckViolation(t, err, "unknown deployment kind")
	var historical int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM deployment_events WHERE kind='identity.forge_token_stored'`).Scan(&historical); err != nil || historical != 1 {
		t.Fatalf("historical rows=%d err=%v", historical, err)
	}

	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "OAuth", Email: "owner@example.test", DisplayName: "Owner"}, "bootstrap-oauth"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "bootstrap-oauth")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO oauth_grants(id,user_id,client_id,client_name,resource,current_refresh_token_id,created_at) VALUES('ogr_fixture',$1,'ocl_fixture','Fixture','https://example.test/mcp','',now())`, owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{31, 33} {
		_, err = st.pool.Exec(ctx, `INSERT INTO oauth_tokens(id,grant_id,kind,token_hash,created_at,expires_at) VALUES('oat_fixture','ogr_fixture','oat',$1,now(),now())`, make([]byte, size))
		requireCheckViolation(t, err, "token digest of the wrong size")
		_, err = st.pool.Exec(ctx, `INSERT INTO oauth_authorization_codes(id,code_hash,user_id,client_id,client_name,redirect_uri,resource,code_challenge,created_at,expires_at) VALUES('oac_fixture',$1,$2,'ocl_fixture','Fixture','https://example.test/cb','https://example.test/mcp','challenge',now(),now())`, make([]byte, size), owner.ID)
		requireCheckViolation(t, err, "code digest of the wrong size")
	}
	_, err = st.pool.Exec(ctx, `INSERT INTO oauth_tokens(id,grant_id,kind,token_hash,created_at,expires_at) VALUES('oat_fixture','ogr_fixture','pat',$1,now(),now())`, make([]byte, 32))
	requireCheckViolation(t, err, "token of another kind")
	if _, err = st.pool.Exec(ctx, `INSERT INTO oauth_tokens(id,grant_id,kind,token_hash,created_at,expires_at) VALUES('oat_fixture','ogr_fixture','oat',$1,now(),now())`, make([]byte, 32)); err != nil {
		t.Fatalf("a 32-byte token digest is refused: %v", err)
	}
}
