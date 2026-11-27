package store

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
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

// TestVolatileFeedbackReportStorage proves that the memory backend allocates
// the four report tables of the durable backends, and that a report request
// writes reports only: the link, note, and line-read stores stay empty
// until their writers exist (component-feedback-reports, Storage;
// feature-production-feedback-listening FR-1).
func TestVolatileFeedbackReportStorage(t *testing.T) {
	st := NewVolatileBackend()
	m := st.(*volatileMemory)
	state := m.feedbackReportState
	if state.reports == nil || state.links == nil || state.notes == nil || state.lineReads == nil {
		t.Fatalf("report state=%+v, want four allocated stores", state)
	}
	ctx := WithActor(WithWorkspace(t.Context(), "reports"), SystemActor())
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "reports"}); err != nil {
		t.Fatal(err)
	}
	environment, err := st.CreateEnvironment(ctx, EnvironmentInput{Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.IssueEnvironmentKey(ctx, environment.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	batch := FeedbackReportBatch{EnvironmentID: environment.ID, KeyID: key.ID, ArrivedAt: time.Now(), Entries: []FeedbackReportEntry{
		{Kind: FeedbackReportError, Key: "crash", Summary: "Crash", File: "main.go", Line: 3, Commit: "abcdef0"},
	}}
	if err := st.RecordFeedbackReports(ctx, batch); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	// The environment's scope holds exactly the one report, and no other
	// scope holds any.
	scope := memoryScopedKey{workspace: "reports", id: environment.ID}
	reports := m.feedbackReportState.reports[scope]
	if report, ok := reports["crash"]; len(m.feedbackReportState.reports) != 1 || len(reports) != 1 || !ok || report.Key != "crash" || report.EnvironmentID != environment.ID {
		t.Fatalf("after one report: scopes=%d reports of the environment=%+v, want exactly the crash report", len(m.feedbackReportState.reports), reports)
	}
	if len(m.feedbackReportState.links) != 0 || len(m.feedbackReportState.notes) != 0 || len(m.feedbackReportState.lineReads) != 0 {
		t.Fatalf("after one report: links=%d notes=%d line reads=%d, want 0, 0, 0",
			len(m.feedbackReportState.links), len(m.feedbackReportState.notes), len(m.feedbackReportState.lineReads))
	}
}
