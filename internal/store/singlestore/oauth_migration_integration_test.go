package singlestore

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// oauthMigrationVersion is migration 0049, which creates the OAuth tables.
const oauthMigrationVersion = 49

// TestMCPOAuthMigrationRestartIntegration replays migration 0049 after a
// partial application: the first two tables exist, the last two do not, and
// the ledger has no row, as a restart after an implicit DDL commit leaves it.
// The replay creates the missing tables, keeps the existing ones, records
// the version once, and a second replay changes nothing. Every table is
// rowstore, and the store refuses a digest of the wrong size before any SQL
// (component-persistence invariants 36 and 44; DEC-139(2);
// req-security-boundaries AC-2.1).
func TestMCPOAuthMigrationRestartIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	tables := []string{"oauth_clients", "oauth_authorization_codes", "oauth_grants", "oauth_tokens"}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	present := func() map[string]string {
		t.Helper()
		rows, err := st.db.QueryContext(ctx, "SELECT table_name,table_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN (?,?,?,?)", tables[0], tables[1], tables[2], tables[3])
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[string]string{}
		for rows.Next() {
			var name, kind string
			if err := rows.Scan(&name, &kind); err != nil {
				t.Fatal(err)
			}
			found[name] = kind
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return found
	}
	ledgerRows := func() int {
		t.Helper()
		var rows int
		if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?", oauthMigrationVersion).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rowstore := func() map[string]bool {
		t.Helper()
		rows, err := st.db.QueryContext(ctx, "SELECT table_name,storage_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN (?,?,?,?)", tables[0], tables[1], tables[2], tables[3])
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var name string
			var storage sql.NullString
			if err := rows.Scan(&name, &storage); err != nil {
				t.Fatal(err)
			}
			out[name] = storage.String != "" && !strings.Contains(strings.ToUpper(storage.String), "COLUMNSTORE")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	assertComplete := func(label string) {
		t.Helper()
		if found := present(); len(found) != len(tables) {
			t.Fatalf("%s: OAuth tables=%v, want all four", label, found)
		}
		for table, isRowstore := range rowstore() {
			if !isRowstore {
				t.Fatalf("%s: %s is not rowstore", label, table)
			}
		}
		if rows := ledgerRows(); rows != 1 {
			t.Fatalf("%s: ledger rows=%d, want one", label, rows)
		}
	}
	assertComplete("fresh schema")

	// A client registered before the restart survives the replay.
	client, err := st.RegisterOAuthClient(ctx, store.OAuthClientRegistration{Name: "Before Restart", RedirectURIs: []string{"http://127.0.0.1:9/callback"}})
	if err != nil {
		t.Fatal(err)
	}
	exec("DELETE FROM conveyor_singlestore_migrations WHERE version=?", oauthMigrationVersion)
	exec("DROP TABLE oauth_tokens")
	exec("DROP TABLE oauth_grants")
	if found := present(); len(found) != 2 {
		t.Fatalf("partial fixture tables=%v, want two", found)
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertComplete("restart after a partial application")
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertComplete("replay")
	if read, err := st.GetOAuthClient(ctx, client.ID); err != nil || read.Name != "Before Restart" {
		t.Fatalf("client after replay=%+v err=%v", read, err)
	}

	for _, size := range []int{0, 31, 33} {
		if err := checkOAuthDigest(make([]byte, size)); err == nil {
			t.Fatalf("a %d-byte digest passed the write boundary", size)
		}
	}
	if err := checkOAuthDigest(core.OAuthCredentialDigest("cv_oat_fixture_value")); err != nil {
		t.Fatalf("a SHA-256 digest failed the write boundary: %v", err)
	}
}
