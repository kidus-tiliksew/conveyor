package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// feedbackMigrationVersion is migration 0051, which creates the feedback
// tables.
const feedbackMigrationVersion = 51

// feedbackIntegration returns a workspace with one environment and key, and a
// signal of it.
func feedbackIntegration(t *testing.T, st *Store, prefix string) (context.Context, string, store.FeedbackSignal) {
	t.Helper()
	ctx, ws := seedEnvironmentWorkspace(t, st, prefix)
	record, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.IssueEnvironmentKey(ctx, record.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, ws, store.FeedbackSignal{
		EnvironmentID: record.ID, KeyID: key.ID, ArrivedAt: time.Now(), NextSignalSeconds: 60,
		Services: []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{{Version: "1"}}}},
	}
}

// TestFeedbackMigrationRestartIntegration drops one feedback table. A signal
// whose last write then fails records nothing partial. A replay of migration
// 0051, as a restart after an implicit DDL commit leaves it, creates the
// missing table, keeps the others, and records the version once
// (component-production-feedback, State and failure; component-persistence
// invariants 36 and 44).
func TestFeedbackMigrationRestartIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, signal := feedbackIntegration(t, st, "feedback-migration")
	t.Cleanup(func() {
		if err := st.migrate(store.WithActor(context.Background(), store.SystemActor())); err != nil {
			t.Errorf("restore migration 0051: %v", err)
		}
	})
	requireComplete := func(label string) {
		t.Helper()
		rows, err := st.db.QueryContext(ctx, "SELECT table_name,storage_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('feedback_signals','feedback_deploys','feedback_running_versions','feedback_accepted_signals')")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := 0
		for rows.Next() {
			var name string
			var storage sql.NullString
			if err := rows.Scan(&name, &storage); err != nil {
				t.Fatal(err)
			}
			if columnstore := strings.Contains(strings.ToUpper(storage.String), "COLUMNSTORE"); storage.String == "" || columnstore != (name == "feedback_accepted_signals") {
				t.Fatalf("%s: %s storage is %q", label, name, storage.String)
			}
			found++
		}
		if err := rows.Err(); err != nil || found != 4 {
			t.Fatalf("%s: %d tables err=%v, want four", label, found, err)
		}
		var ledger int
		if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?", feedbackMigrationVersion).Scan(&ledger); err != nil || ledger != 1 {
			t.Fatalf("%s: ledger rows=%d err=%v, want one", label, ledger, err)
		}
	}
	require := func(label string, wantDeploys, wantRunning, wantUsed int) {
		t.Helper()
		var deploys, running, used int
		for target, query := range map[*int]string{
			&deploys: "SELECT COUNT(*) FROM feedback_deploys WHERE workspace_id=?",
			&running: "SELECT COUNT(*) FROM feedback_running_versions WHERE workspace_id=?",
			&used:    "SELECT COUNT(*) FROM environment_keys WHERE workspace_id=? AND last_used_at IS NOT NULL",
		} {
			if err := st.db.QueryRowContext(ctx, query, ws).Scan(target); err != nil {
				t.Fatal(err)
			}
		}
		if deploys != wantDeploys || running != wantRunning || used != wantUsed {
			t.Fatalf("%s: deploys=%d running=%d used=%d, want %d %d %d", label, deploys, running, used, wantDeploys, wantRunning, wantUsed)
		}
	}
	requireComplete("fresh schema")
	for _, statement := range []string{fmt.Sprintf("DELETE FROM conveyor_singlestore_migrations WHERE version=%d", feedbackMigrationVersion), "DROP TABLE feedback_accepted_signals"} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	holder := claimHolder(t, st, environmentClaimKey(ws))
	if err := st.RecordFeedbackSignal(ctx, signal); !errors.Is(err, store.ErrBackendOperation) {
		t.Fatalf("err=%v, want the generic store failure", err)
	}
	require("a failed signal", 0, 0, 0)
	if got := claimHolder(t, st, environmentClaimKey(ws)); got != holder {
		t.Fatalf("a failed signal moved the claim from %q to %q", holder, got)
	}
	for _, label := range []string{"restart after a partial application", "replay"} {
		if err := st.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		requireComplete(label)
	}
	if err := st.RecordFeedbackSignal(ctx, signal); err != nil {
		t.Fatal(err)
	}
	require("an accepted signal", 1, 1, 1)
}

// TestFeedbackClaimWaitsForPredecessorIntegration models a predecessor whose
// claim row is released before its rows are visible. A signal waits for the
// witness row, a wait that runs out writes nothing, and an accepted signal
// names its own revision on the environment row (component-persistence
// invariant 8).
func TestFeedbackClaimWaitsForPredecessorIntegration(t *testing.T) {
	st := integrationStore(t)
	shortClaimWait(t)
	ctx, ws, signal := feedbackIntegration(t, st, "feedback-claim")
	claim := environmentClaimKey(ws)
	if err := st.RecordFeedbackSignal(ctx, signal); err != nil {
		t.Fatal(err)
	}
	if holder := claimHolder(t, st, claim); holder != "environment:"+signal.EnvironmentID+":3" {
		t.Fatalf("holder after a signal=%q", holder)
	}
	nameClaim(t, st, ctx, claim, "environment:"+signal.EnvironmentID+":4")
	later := signal
	later.ArrivedAt = signal.ArrivedAt.Add(10 * time.Second)
	later.Services = []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{{Version: "2"}}}}
	if err := st.RecordFeedbackSignal(ctx, later); !errors.Is(err, store.ErrRetryable) {
		t.Fatalf("err=%v, want ErrRetryable", err)
	}
	if deploys, err := st.ListFeedbackDeploys(ctx, signal.EnvironmentID); err != nil || len(deploys) != 1 || deploys[0].StoppedAt != nil {
		t.Fatalf("deploys after the wait=%+v err=%v", deploys, err)
	}
	if _, err := st.db.ExecContext(ctx, "UPDATE environments SET revision=4 WHERE workspace_id=? AND id=?", ws, signal.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFeedbackSignal(ctx, later); err != nil {
		t.Fatal(err)
	}
	if holder := claimHolder(t, st, claim); holder != "environment:"+signal.EnvironmentID+":5" {
		t.Fatalf("holder after the retry=%q", holder)
	}
	if deploys, err := st.ListFeedbackDeploys(ctx, signal.EnvironmentID); err != nil || len(deploys) != 2 || deploys[0].StoppedAt == nil {
		t.Fatalf("deploys after the retry=%+v err=%v", deploys, err)
	}
}
