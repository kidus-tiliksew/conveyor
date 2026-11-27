package singlestore

import (
	"context"
	"testing"
	"time"
)

// closeFeedbackReport writes one stored report as closed at the given time.
// Closing belongs to a sibling task, so the conformance entry point supplies
// this as Fixture.CloseFeedbackReport.
func closeFeedbackReport(t *testing.T, ctx context.Context, st *Store, reportID string, at time.Time) {
	t.Helper()
	ws, err := workspace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := st.db.ExecContext(ctx, `UPDATE feedback_reports SET state='closed', in_inbox=0, closed_at=?, closed_by='usr-closer', close_reason='fixed' WHERE workspace_id=? AND id=?`, at.UTC(), ws, reportID)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("close feedback report: rows=%d err=%v", rows, err)
	}
}
