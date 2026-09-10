package postgres

import (
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"testing"
)

func TestRetireForgeTokensPreservesHistoricalEventsIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 124)
	ctx := store.WithWorkspace(t.Context(), "retirement")
	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Retirement", Email: "owner@example.test", DisplayName: "Owner"}, "bootstrap-retirement"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "bootstrap-retirement")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, isolationConfig("retirement")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO user_forge_tokens(user_id,cipher_nonce,ciphertext,forge_login) VALUES($1,$2,$3,'historical')`, owner.ID, make([]byte, 12), []byte("encrypted historical fixture")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO workspace_forge_tokens(workspace_id,cipher_nonce,ciphertext,forge_login) VALUES('retirement',$1,$2,'historical')`, make([]byte, 12), []byte("encrypted historical fixture")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO deployment_events(kind,actor_id,actor_role,payload_json) VALUES('identity.forge_token_stored','historical','user','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO events(workspace_id,kind,actor_id,actor_role,payload_json) VALUES('retirement','workspace.forge_token_stored','historical','user','{}')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(ctx, st.pool); err != nil {
			t.Fatal(err)
		}
	}
	var tables int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ('user_forge_tokens','workspace_forge_tokens')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("retired tables=%d err=%v", tables, err)
	}
	var history int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM deployment_events WHERE kind='identity.forge_token_stored'`).Scan(&history); err != nil || history != 1 {
		t.Fatalf("identity history=%d err=%v", history, err)
	}
	events, err := st.queries.ListWorkspaceEvents(ctx, "retirement")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || event.Kind == "workspace.forge_token_stored"
	}
	if !found {
		t.Fatal("historical workspace event is unreadable")
	}
	for _, query := range []string{
		`INSERT INTO deployment_events(kind,actor_id,actor_role,payload_json) VALUES('identity.forge_token_stored','new','user','{}')`,
		`INSERT INTO events(workspace_id,kind,actor_id,actor_role,payload_json) VALUES('retirement','workspace.forge_token_stored','new','user','{}')`,
	} {
		_, err := st.pool.Exec(ctx, query)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			t.Fatalf("retired event constraint: %v", err)
		}
	}
}
