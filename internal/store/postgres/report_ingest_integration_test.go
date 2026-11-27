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

// closeFeedbackReport writes one stored report as closed at the given time.
// Closing belongs to a sibling task, so the conformance entry point supplies
// this as Fixture.CloseFeedbackReport.
func closeFeedbackReport(t *testing.T, ctx context.Context, st *Store, reportID string, at time.Time) {
	t.Helper()
	tag, err := st.pool.Exec(ctx, `UPDATE feedback_reports SET state='closed', in_inbox=false, closed_at=$3, closed_by='usr-closer', close_reason='fixed' WHERE workspace_id=$1 AND id=$2`, workspace(ctx), reportID, at)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("close feedback report: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

// TestFeedbackReportMigrationIntegration proves migration 182: the taskless
// event CHECK admits the seven report kinds and every kind migration 177
// admits, refuses an unknown one, and each of the four new tables is keyed to
// its workspace and cascades with it (component-feedback-reports invariant
// 20; component-persistence invariants 21 and 34). A second run changes
// nothing.
func TestFeedbackReportMigrationIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 124)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "reports"), store.SystemActor())
	for range 2 {
		if err := Migrate(ctx, st.pool); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "reports"}); err != nil {
		t.Fatal(err)
	}
	insertEvent := func(kind string) error {
		_, err := st.pool.Exec(ctx, `INSERT INTO events(workspace_id,kind,actor_id,actor_role,payload_json) VALUES('reports',$1,'system','system','{}')`, kind)
		return err
	}
	for _, kind := range append([]string{store.EnvironmentCreatedEvent, store.EnvironmentSuggestionIgnoredEvent, store.CrateCreatedEvent, "config.updated"}, store.FeedbackReportEventKinds...) {
		if err := insertEvent(kind); err != nil {
			t.Fatalf("kind %s refused: %v", kind, err)
		}
	}
	var pgErr *pgconn.PgError
	if err := insertEvent("feedback_report.deleted"); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("an unknown report kind err=%v, want SQLSTATE 23514", err)
	}
	rows, err := st.pool.Query(ctx, `SELECT c.conrelid::regclass::text FROM pg_constraint c
WHERE c.contype='f' AND c.confrelid='workspaces'::regclass AND c.confdeltype='c'
AND c.conrelid IN ('feedback_reports'::regclass, 'feedback_report_links'::regclass, 'feedback_report_notes'::regclass, 'feedback_report_line_reads'::regclass)
ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var keyed []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		keyed = append(keyed, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"feedback_report_line_reads", "feedback_report_links", "feedback_report_notes", "feedback_reports"}
	if len(keyed) != len(want) {
		t.Fatalf("tables keyed to workspaces with a cascade=%v, want %v", keyed, want)
	}
	for i := range want {
		if keyed[i] != want[i] {
			t.Fatalf("tables keyed to workspaces with a cascade=%v, want %v", keyed, want)
		}
	}
}
