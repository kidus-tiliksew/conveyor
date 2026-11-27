package store

import (
	"context"
	"testing"
	"time"
)

// CloseFeedbackReportForTest writes one volatile report as closed at the
// given time. Closing belongs to a sibling task, so the memory conformance
// entry point supplies this as Fixture.CloseFeedbackReport.
func CloseFeedbackReportForTest(t *testing.T, st Backend, ctx context.Context, reportID string, at time.Time) {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	for scope, reports := range m.feedbackReportState.reports {
		if scope.workspace != workspace {
			continue
		}
		for key, report := range reports {
			if report.ID != reportID {
				continue
			}
			closed := EnvironmentTime(at)
			report.State, report.InInbox = FeedbackReportClosed, false
			report.ClosedAt, report.ClosedBy, report.CloseReason = &closed, "usr-closer", "fixed"
			reports[key] = report
			return
		}
	}
	t.Fatalf("feedback report %q is missing", reportID)
}
