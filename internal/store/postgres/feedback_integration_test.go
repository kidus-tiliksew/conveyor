package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// feedbackIntegration returns a migrated store with one environment and key,
// and a signal of it.
func feedbackIntegration(t *testing.T) (*Store, context.Context, store.FeedbackSignal) {
	t.Helper()
	st := newIdentityIntegrationStore(t, 124)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "feedback"), store.SystemActor())
	for range 2 {
		if err := Migrate(ctx, st.pool); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "feedback"}); err != nil {
		t.Fatal(err)
	}
	record, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.IssueEnvironmentKey(ctx, record.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	return st, ctx, store.FeedbackSignal{
		EnvironmentID: record.ID, KeyID: key.ID, ArrivedAt: time.Now(), NextSignalSeconds: 60,
		Services: []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{{Version: "1"}}}},
	}
}

// TestFeedbackMigrationIntegration proves migration 178: the tables hold the
// rules that PostgreSQL can hold, and a second run changes nothing
// (component-production-feedback invariants 3 and 6).
func TestFeedbackMigrationIntegration(t *testing.T) {
	st, ctx, signal := feedbackIntegration(t)
	if err := st.RecordFeedbackSignal(ctx, signal); err != nil {
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
	requireCode("23514", "an interval under the bound", `UPDATE feedback_signals SET next_signal_seconds=9`)
	requireCode("23514", "an interval over the bound", `UPDATE feedback_signals SET next_signal_seconds=86401`)
	requireCode("23514", "an unknown source", `UPDATE feedback_deploys SET source='person'`)
	requireCode("23514", "a stop without its origin", `UPDATE feedback_deploys SET stopped_at=now()`)
	requireCode("23503", "a signal of a missing environment", `UPDATE feedback_signals SET environment_id='env-missing'`)
	requireCode("23503", "a running version without a deploy", `UPDATE feedback_running_versions SET deploy_id='deploy_missing'`)
	requireCode("23505", "a second row for one version", `INSERT INTO feedback_running_versions SELECT * FROM feedback_running_versions`)
}

// TestFeedbackFailureRollsBackIntegration proves that a signal whose last
// write fails records nothing partial (component-production-feedback, State
// and failure).
func TestFeedbackFailureRollsBackIntegration(t *testing.T) {
	st, ctx, signal := feedbackIntegration(t)
	if _, err := st.pool.Exec(ctx, `ALTER TABLE feedback_signals ADD CONSTRAINT feedback_refused CHECK (next_signal_seconds <> 61)`); err != nil {
		t.Fatal(err)
	}
	counts := func() (deploys, running, stopped, signals, used int) {
		t.Helper()
		if err := st.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM feedback_deploys), (SELECT count(*) FROM feedback_running_versions), (SELECT count(*) FROM feedback_deploys WHERE stopped_at IS NOT NULL), (SELECT count(*) FROM feedback_signals), (SELECT count(*) FROM environment_keys WHERE last_used_at IS NOT NULL)`).Scan(&deploys, &running, &stopped, &signals, &used); err != nil {
			t.Fatal(err)
		}
		return
	}
	require := func(what string, want ...int) {
		t.Helper()
		deploys, running, stopped, signals, used := counts()
		for i, got := range []int{deploys, running, stopped, signals, used} {
			if got != want[i] {
				t.Fatalf("%s: deploys=%d running=%d stopped=%d signals=%d used=%d, want %v", what, deploys, running, stopped, signals, used, want)
			}
		}
	}
	refused := signal
	refused.NextSignalSeconds = 61
	if err := st.RecordFeedbackSignal(ctx, refused); !errors.Is(err, store.ErrBackendOperation) {
		t.Fatalf("err=%v, want the generic store failure", err)
	}
	require("a failed first signal", 0, 0, 0, 0, 0)
	if err := st.RecordFeedbackSignal(ctx, signal); err != nil {
		t.Fatal(err)
	}
	require("an accepted signal", 1, 1, 0, 1, 1)
	refused.ArrivedAt = signal.ArrivedAt.Add(10 * time.Second)
	refused.Services = []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{{Version: "2"}}}}
	if err := st.RecordFeedbackSignal(ctx, refused); !errors.Is(err, store.ErrBackendOperation) {
		t.Fatalf("err=%v, want the generic store failure", err)
	}
	require("a failed later signal", 1, 1, 0, 1, 1)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.RecordFeedbackSignal(cancelled, signal); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err=%v", err)
	}
	require("a cancelled signal", 1, 1, 0, 1, 1)
}
