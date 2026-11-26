package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// feedbackStatusMigrationVersion is migration 0052, which creates the
// resolving and comparing tables.
const feedbackStatusMigrationVersion = 52

// TestFeedbackStatusMigrationRestartIntegration drops one table of migration
// 0052. A replay, as a restart after an implicit DDL commit leaves it,
// creates the missing table as rowstore, keeps the others and their rows, and
// records the version once (component-persistence invariants 36 and 44).
func TestFeedbackStatusMigrationRestartIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, _, signal := feedbackIntegration(t, st, "feedback-status-migration")
	t.Cleanup(func() {
		if err := st.migrate(store.WithActor(context.Background(), store.SystemActor())); err != nil {
			t.Errorf("restore migration 0052: %v", err)
		}
	})
	tables := []string{"feedback_resolutions", "feedback_comparisons", "feedback_admissions", "feedback_branch_heads", "feedback_status_state"}
	requireComplete := func(label string) {
		t.Helper()
		rows, err := st.db.QueryContext(ctx, "SELECT table_name,storage_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('"+strings.Join(tables, "','")+"')")
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
			if storage.String == "" || strings.Contains(strings.ToUpper(storage.String), "COLUMNSTORE") {
				t.Fatalf("%s: %s storage is %q, want rowstore", label, name, storage.String)
			}
			found++
		}
		if err := rows.Err(); err != nil || found != len(tables) {
			t.Fatalf("%s: %d tables err=%v, want %d", label, found, err, len(tables))
		}
		var ledger int
		if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?", feedbackStatusMigrationVersion).Scan(&ledger); err != nil || ledger != 1 {
			t.Fatalf("%s: ledger rows=%d err=%v, want one", label, ledger, err)
		}
	}
	requireComplete("fresh schema")
	signal.Services = []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{{Commit: "abc1234"}}}}
	if err := st.RecordFeedbackSignal(ctx, signal); err != nil {
		t.Fatal(err)
	}
	ref := store.FeedbackVersionRef{EnvironmentID: signal.EnvironmentID, Service: "api", FeedbackVersion: store.FeedbackVersion{Commit: "abc1234"}}
	commit := "abc1234" + strings.Repeat("0", 33)
	if claimed, err := st.ClaimFeedbackResolution(ctx, ref, signal.ArrivedAt, time.Now()); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := st.RecordFeedbackResolution(ctx, store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "conveyor", ResolvedCommit: commit, AttemptedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{fmt.Sprintf("DELETE FROM conveyor_singlestore_migrations WHERE version=%d", feedbackStatusMigrationVersion), "DROP TABLE feedback_comparisons"} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for _, label := range []string{"restart after a partial application", "replay"} {
		if err := st.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		requireComplete(label)
	}
	inputs, err := st.ReadFeedbackInputs(ctx, []string{signal.EnvironmentID})
	if err != nil || len(inputs.Resolutions) != 1 || inputs.Resolutions[0].ResolvedCommit != commit || !inputs.Running[signal.EnvironmentID][0].Resolved {
		t.Fatalf("inputs after the replay=%+v err=%v", inputs, err)
	}
	key := store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: commit, Head: commit}
	if err := st.RecordFeedbackComparisons(ctx, []store.FeedbackComparison{{FeedbackComparisonKey: key, Status: store.FeedbackCompareIdentical, ComparedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ListFeedbackComparisons(ctx, []store.FeedbackComparisonKey{key}); err != nil || len(got) != 1 {
		t.Fatalf("comparisons after the replay=%+v err=%v", got, err)
	}
}
