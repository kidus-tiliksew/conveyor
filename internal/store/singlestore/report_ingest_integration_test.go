package singlestore

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
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

// seedFeedbackReportLink writes one stored link as given. Confirming and
// removing belong to a sibling task, so the conformance entry point supplies
// this as Fixture.SeedFeedbackReportLink.
func seedFeedbackReportLink(t *testing.T, ctx context.Context, st *Store, link store.FeedbackReportLink) {
	t.Helper()
	ws, err := workspace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var confirmedBy, removedBy, confirmedAt, removedAt any
	if link.ConfirmedAt != nil {
		confirmedBy, confirmedAt = link.ConfirmedBy, link.ConfirmedAt.UTC()
	}
	if link.RemovedAt != nil {
		removedBy, removedAt = link.RemovedBy, link.RemovedAt.UTC()
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO feedback_report_links (workspace_id,id,report_id,task_id,state,evidence,created_at,created_by,confirmed_by,confirmed_at,removed_by,removed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		ws, link.ID, link.ReportID, link.TaskID, link.State, store.FeedbackReportEvidenceJSON(link.Evidence), link.CreatedAt.UTC(), link.CreatedBy, confirmedBy, confirmedAt, removedBy, removedAt); err != nil {
		t.Fatalf("seed feedback report link: %v", err)
	}
}
