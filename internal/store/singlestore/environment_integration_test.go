package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// environmentMigrationVersion is migration 0050, which creates the
// environment tables.
const environmentMigrationVersion = 50

// ageEnvironmentKey moves one environment key's stored last use back by by.
func ageEnvironmentKey(t *testing.T, ctx context.Context, st *Store, keyID string, by time.Duration) {
	t.Helper()
	ws, err := workspace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := st.db.ExecContext(ctx, "UPDATE environment_keys SET last_used_at=DATE_SUB(last_used_at,INTERVAL ? MICROSECOND) WHERE workspace_id=? AND id=? AND last_used_at IS NOT NULL", by.Microseconds(), ws, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("age environment key: rows=%d err=%v", rows, err)
	}
}

// storedEnvironmentKey reads one key row: its hash apart and every other
// column as text.
func storedEnvironmentKey(t *testing.T, ctx context.Context, st *Store, keyID string) storetest.EnvironmentStoredKey {
	t.Helper()
	ws, err := workspace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.db.QueryContext(ctx, "SELECT * FROM environment_keys WHERE workspace_id=? AND id=?", ws, keyID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("environment key %q is missing: %v", keyID, rows.Err())
	}
	values := make([]sql.RawBytes, len(columns))
	targets := make([]any, len(columns))
	for i := range values {
		targets[i] = &values[i]
	}
	if err := rows.Scan(targets...); err != nil {
		t.Fatal(err)
	}
	var stored storetest.EnvironmentStoredKey
	var text []string
	for i, column := range columns {
		if column == "key_hash" {
			stored.Digest = append([]byte{}, values[i]...)
			continue
		}
		text = append(text, column+"="+string(values[i]))
	}
	stored.Text = strings.Join(text, " ")
	return stored
}

func seedEnvironmentWorkspace(t *testing.T, st *Store, prefix string) (context.Context, string) {
	t.Helper()
	ws := prefix + "-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	return ctx, ws
}

// TestEnvironmentMigrationRestartIntegration replays migration 0050 after a
// partial application, as a restart after an implicit DDL commit leaves it.
// The replay creates the missing tables, keeps the existing one, and records
// the version once (component-persistence invariants 36 and 44).
func TestEnvironmentMigrationRestartIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, _ := seedEnvironmentWorkspace(t, st, "environment-migration")
	t.Cleanup(func() {
		if err := st.migrate(store.WithActor(context.Background(), store.SystemActor())); err != nil {
			t.Errorf("restore migration 0050: %v", err)
		}
	})
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	tables := func() map[string]bool {
		t.Helper()
		rows, err := st.db.QueryContext(ctx, "SELECT table_name,storage_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('environments','environment_keys','environment_suggestions')")
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
	requireComplete := func(label string) {
		t.Helper()
		found := tables()
		if len(found) != 3 {
			t.Fatalf("%s: tables=%v, want three", label, found)
		}
		for name, rowstore := range found {
			if !rowstore {
				t.Fatalf("%s: %s is not rowstore", label, name)
			}
		}
		var ledger int
		if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?", environmentMigrationVersion).Scan(&ledger); err != nil || ledger != 1 {
			t.Fatalf("%s: ledger rows=%d err=%v, want one", label, ledger, err)
		}
	}
	requireComplete("fresh schema")
	before, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Before restart"})
	if err != nil {
		t.Fatal(err)
	}
	exec("DELETE FROM conveyor_singlestore_migrations WHERE version=?", environmentMigrationVersion)
	exec("DROP TABLE environment_keys")
	exec("DROP TABLE environment_suggestions")
	for _, label := range []string{"restart after a partial application", "replay"} {
		if err := st.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		requireComplete(label)
	}
	if listed, err := st.ListEnvironments(ctx, store.EnvironmentListQuery{}); err != nil || len(listed) != 1 || listed[0].ID != before.ID {
		t.Fatalf("environments after replay=%+v err=%v", listed, err)
	}
}

// TestEnvironmentClaimWaitsForPredecessorIntegration models a predecessor
// whose claim row is released before its rows are visible: the claim names a
// revision that the witness row does not show yet. A mutation waits for the
// row, and a wait that runs out returns ErrRetryable with nothing written
// (component-persistence invariant 8). Both kinds of witness row are covered.
func TestEnvironmentClaimWaitsForPredecessorIntegration(t *testing.T) {
	st := integrationStore(t)
	shortClaimWait(t)
	ctx, ws := seedEnvironmentWorkspace(t, st, "environment-claim")
	record, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.IssueEnvironmentKey(ctx, record.ID, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	claim := environmentClaimKey(ws)
	if holder := claimHolder(t, st, claim); holder != "environment:"+record.ID+":2" {
		t.Fatalf("holder after two mutations = %q", holder)
	}
	if recorded, err := st.RecordEnvironmentReport(ctx, store.EnvironmentReport{GitHubEnvironment: "Preview", Repository: "repo", ReportedAt: time.Now()}); err != nil || !recorded {
		t.Fatalf("report recorded=%v err=%v", recorded, err)
	}
	digest := environmentNameDigest("preview")
	if holder := claimHolder(t, st, claim); holder != "suggestion:"+digest+":3" {
		t.Fatalf("holder after a report = %q", holder)
	}
	state := func() string {
		t.Helper()
		records, err := st.ListEnvironmentRecords(ctx, store.EnvironmentListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		suggestions, err := st.ListEnvironmentSuggestions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var events int
		if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND LEFT(kind,12)='environment.'", ws).Scan(&events); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%+v %+v %d", records, suggestions, events)
	}
	acts := map[string]func() error{
		"create": func() error {
			_, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Staging"})
			return err
		},
		"update": func() error {
			window := 5
			_, err := st.UpdateEnvironment(ctx, record.ID, store.EnvironmentPatch{TimingWindowMinutes: &window})
			return err
		},
		"issue": func() error {
			_, err := st.IssueEnvironmentKey(ctx, record.ID, "second")
			return err
		},
		"revoke": func() error { return st.RevokeEnvironmentKey(ctx, record.ID, key.ID) },
		"report": func() error {
			_, err := st.RecordEnvironmentReport(ctx, store.EnvironmentReport{GitHubEnvironment: "canary", Repository: "repo", ReportedAt: time.Now()})
			return err
		},
		"ignore": func() error { return st.IgnoreEnvironmentSuggestion(ctx, "preview") },
	}
	for witness, holder := range map[string]string{"environment": "environment:" + record.ID + ":4", "suggestion": "suggestion:" + digest + ":4"} {
		nameClaim(t, st, ctx, claim, holder)
		before := state()
		for name, act := range acts {
			if err := act(); !errors.Is(err, store.ErrRetryable) || strings.Contains(err.Error(), record.ID) {
				t.Fatalf("%s under an unseen %s err=%v, want ErrRetryable", name, witness, err)
			}
		}
		if after := state(); after != before {
			t.Fatalf("a timed-out wait wrote: before %s after %s", before, after)
		}
		if got := claimHolder(t, st, claim); got != holder {
			t.Fatalf("a timed-out wait moved the holder to %q", got)
		}
	}

	// The row appears while a mutation waits, and the mutation then decides
	// on it and records the next revision.
	claimedRecordWaitLimit = 10 * time.Second
	waitCtx, observer := observeClaimedRecordWait(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := st.CreateEnvironment(waitCtx, store.EnvironmentInput{Name: "Staging"})
		done <- err
	}()
	select {
	case <-observer.waits:
	case err := <-done:
		t.Fatalf("the create did not wait: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the create did not reach its wait")
	}
	if _, err := st.db.ExecContext(ctx, "UPDATE environment_suggestions SET revision=4 WHERE workspace_id=? AND name_digest=?", ws, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, "UPDATE environments SET revision=4 WHERE workspace_id=? AND id=?", ws, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if holder := claimHolder(t, st, claim); !strings.HasPrefix(holder, "environment:env-") || !strings.HasSuffix(holder, ":5") {
		t.Fatalf("holder after the wait = %q", holder)
	}
}
