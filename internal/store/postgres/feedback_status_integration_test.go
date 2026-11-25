package postgres

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestFeedbackStatusMigrationIntegration proves migration 179: the tables
// hold the rules that PostgreSQL can hold, and a resolved answer survives a
// reopened store (component-production-feedback invariants 7, 8, and 14).
func TestFeedbackStatusMigrationIntegration(t *testing.T) {
	st, ctx, signal := feedbackIntegration(t)
	signal.Services = []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{{Commit: "abc1234"}}}}
	if err := st.RecordFeedbackSignal(ctx, signal); err != nil {
		t.Fatal(err)
	}
	commit := "abc1234" + strings.Repeat("0", 33)
	ref := store.FeedbackVersionRef{EnvironmentID: signal.EnvironmentID, Service: "api", FeedbackVersion: store.FeedbackVersion{Commit: "abc1234"}}
	if claimed, err := st.ClaimFeedbackResolution(ctx, ref, signal.ArrivedAt, time.Now()); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := st.RecordFeedbackResolution(ctx, store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "conveyor", ResolvedCommit: commit, AttemptedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	key := store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: commit, Head: commit}
	if err := st.RecordFeedbackComparisons(ctx, []store.FeedbackComparison{{FeedbackComparisonKey: key, Status: store.FeedbackCompareIdentical, ComparedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	requireCode := func(code, what, sql string, args ...any) {
		t.Helper()
		_, err := st.pool.Exec(ctx, sql, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s err=%v, want SQLSTATE %s", what, err, code)
		}
	}
	requireCode("23514", "a short resolved commit", `UPDATE feedback_resolutions SET resolved_commit='abc1234'`)
	requireCode("23514", "a resolved row with a reason", `UPDATE feedback_resolutions SET reason='no_match'`)
	requireCode("23514", "an unresolved row without a retry time", `UPDATE feedback_resolutions SET resolved_commit='', reason='no_match'`)
	requireCode("23503", "a resolution of a missing environment", `UPDATE feedback_resolutions SET environment_id='env-missing'`)
	requireCode("23514", "an unknown comparison status", `UPDATE feedback_comparisons SET status='maybe'`)
	requireCode("23514", "a short comparison base", `UPDATE feedback_comparisons SET base_commit='abc'`)
	requireCode("23505", "a second answer for one pair", `INSERT INTO feedback_comparisons SELECT * FROM feedback_comparisons`)
	requireCode("23514", "a short branch head", `INSERT INTO feedback_branch_heads (workspace_id, repository, branch, head_commit, observed_at) VALUES ('feedback', 'acme/conveyor', 'main', 'abc', now())`)
	reopened := reopenIdentityStore(t, st)
	inputs, err := reopened.ReadFeedbackInputs(ctx, []string{signal.EnvironmentID})
	if err != nil || len(inputs.Resolutions) != 1 || inputs.Resolutions[0].ResolvedCommit != commit {
		t.Fatalf("resolutions after reopening=%+v err=%v", inputs.Resolutions, err)
	}
	if claimed, err := reopened.ClaimFeedbackResolution(ctx, ref, signal.ArrivedAt.Add(time.Hour), time.Now()); err != nil || claimed {
		t.Fatalf("a resolved version was claimed after reopening: claimed=%v err=%v", claimed, err)
	}
	if got, err := reopened.ListFeedbackComparisons(ctx, []store.FeedbackComparisonKey{key}); err != nil || len(got) != 1 {
		t.Fatalf("comparisons after reopening=%+v err=%v", got, err)
	}
}
