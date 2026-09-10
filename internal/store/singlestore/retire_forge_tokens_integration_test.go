package singlestore

import (
	"strings"
	"testing"
)

func TestRetireForgeTokensSeededRowsAndRetryIntegration(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	// Restore only the retired tables from the published legacy schema to model
	// populated pre-upgrade rows. The current migration chain has already run.
	raw, err := migrationFiles.ReadFile("migrations/0001_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(string(raw), ";") {
		if !strings.Contains(statement, "CREATE ROWSTORE TABLE IF NOT EXISTS `user_forge_tokens`") && !strings.Contains(statement, "CREATE ROWSTORE TABLE IF NOT EXISTS `workspace_forge_tokens`") {
			continue
		}
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO user_forge_tokens(user_id,cipher_nonce,ciphertext,forge_login) VALUES('legacy',X'01',X'02','historical')",
		"INSERT INTO workspace_forge_tokens(workspace_id,cipher_nonce,ciphertext,forge_login) VALUES('legacy',X'01',X'02','historical')",
		"INSERT INTO deployment_events(kind,actor_id,actor_role,payload_json,at) VALUES('identity.forge_token_stored','historical','user','{}',CURRENT_TIMESTAMP(6))",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	raw, err = migrationFiles.ReadFile("migrations/0005_retire_forge_tokens.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for _, statement := range strings.Split(string(raw), ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('user_forge_tokens','workspace_forge_tokens')").Scan(&count); err != nil || count != 0 {
		t.Fatalf("remaining tables=%d err=%v", count, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM deployment_events WHERE kind='identity.forge_token_stored'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("historical events=%d err=%v", count, err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
