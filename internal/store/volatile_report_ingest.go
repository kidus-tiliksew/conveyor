package store

import (
	"context"
	"slices"
	"strings"
)

// feedbackReportState mirrors the report table of the durable backends: each
// environment's reports by problem key. Every mutation runs under the memory
// write mutex, the volatile backend's critical section, which signals take
// too (component-feedback-reports invariant 6).
type feedbackReportState struct {
	reports map[memoryScopedKey]map[string]FeedbackReport
}

func newFeedbackReportState() feedbackReportState {
	return feedbackReportState{reports: map[memoryScopedKey]map[string]FeedbackReport{}}
}

var _ ReportStore = (*volatileMemory)(nil)

// CheckFeedbackReportRepositories implements ReportStore.
func (m *volatileMemory) CheckFeedbackReportRepositories(ctx context.Context, batch FeedbackReportBatch) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	repositories, err := m.environmentRepositoriesLocked(workspaceOrDefault(ctx, ""))
	if err != nil {
		return err
	}
	return CheckFeedbackReportRepositories(batch, repositories)
}

// RecordFeedbackReports implements ReportStore. The summaries are redacted
// before the mutex is taken. Every check and the whole plan run before the
// first write, so a refusal leaves the store as it was.
func (m *volatileMemory) RecordFeedbackReports(ctx context.Context, batch FeedbackReportBatch) error {
	prepared, err := PrepareFeedbackReports(ctx, m, batch)
	if err != nil {
		return err
	}
	workspace := workspaceOrDefault(ctx, "")
	scope := memoryScopedKey{workspace: workspace, id: prepared.EnvironmentID}
	keyScope := memoryScopedKey{workspace: workspace, id: prepared.KeyID}
	m.mu.Lock()
	defer m.mu.Unlock()
	environment, found := m.environmentState.environments[scope]
	key, issued := m.environmentState.keys[keyScope]
	if !found || environment.Archived || !issued || key.EnvironmentID != prepared.EnvironmentID || key.RevokedAt != nil {
		return ErrInvalidEnvironmentKey
	}
	repositories, err := m.environmentRepositoriesLocked(workspace)
	if err != nil {
		return err
	}
	if err := CheckFeedbackReportRepositories(prepared.FeedbackReportBatch, repositories); err != nil {
		return err
	}
	stored := m.feedbackReportState.reports[scope]
	open := 0
	for _, report := range stored {
		if report.State == FeedbackReportOpen {
			open++
		}
	}
	matched := map[string]FeedbackReport{}
	for _, entry := range prepared.Entries {
		if report, ok := stored[entry.Key]; ok {
			matched[entry.Key] = cloneFeedbackReport(report)
		}
	}
	plan, err := PlanFeedbackReports(prepared, key.Label, matched, open)
	if err != nil {
		return err
	}
	if stored == nil {
		stored = map[string]FeedbackReport{}
		m.feedbackReportState.reports[scope] = stored
	}
	for _, change := range plan.Changes {
		stored[change.Report.Key] = cloneFeedbackReport(change.Report)
	}
	for _, event := range plan.Events(prepared.ArrivedAt) {
		m.appendEventLocked(ctx, event)
	}
	m.touchEnvironmentKeyLocked(keyScope)
	return nil
}

// ListEnvironmentFeedbackReports implements ReportStore.
func (m *volatileMemory) ListEnvironmentFeedbackReports(ctx context.Context, environmentID string) ([]FeedbackReport, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stored := m.feedbackReportState.reports[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: environmentID}]
	reports := make([]FeedbackReport, 0, len(stored))
	for _, report := range stored {
		reports = append(reports, cloneFeedbackReport(report))
	}
	slices.SortFunc(reports, func(a, b FeedbackReport) int { return strings.Compare(a.ID, b.ID) })
	return reports, nil
}

// cloneFeedbackReport copies the pointer members, so no caller shares them
// with the map.
func cloneFeedbackReport(report FeedbackReport) FeedbackReport {
	if report.ClosedAt != nil {
		at := *report.ClosedAt
		report.ClosedAt = &at
	}
	if report.ReopenedAt != nil {
		at := *report.ReopenedAt
		report.ReopenedAt = &at
	}
	if report.ReopenOrigin != nil {
		origin := *report.ReopenOrigin
		report.ReopenOrigin = &origin
	}
	if report.LinkingEndedAt != nil {
		at := *report.LinkingEndedAt
		report.LinkingEndedAt = &at
	}
	return report
}
