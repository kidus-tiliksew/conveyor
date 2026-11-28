package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

var _ ReportReadStore = (*volatileMemory)(nil)

// feedbackReportRefsLocked answers the tasks and crates of the workspace
// that the given IDs name, under the held mutex. An ID the workspace does
// not hold has no entry.
func (m *volatileMemory) feedbackReportRefsLocked(workspace string, taskIDs, crateIDs []string) (map[string]FeedbackReportTaskRef, map[string]FeedbackReportCrateRef) {
	tasks := map[string]FeedbackReportTaskRef{}
	crates := map[string]FeedbackReportCrateRef{}
	crateRef := func(id string) (FeedbackReportCrateRef, bool) {
		crate, ok := m.crates[memoryScopedKey{workspace: workspace, id: id}]
		if !ok {
			return FeedbackReportCrateRef{}, false
		}
		return FeedbackReportCrateRef{ID: crate.ID, Name: crate.Name}, true
	}
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || task.Workspace != workspace {
			continue
		}
		ref := FeedbackReportTaskRef{ID: task.ID, Title: task.Title}
		if crate, ok := crateRef(task.TaskCrateID()); ok {
			ref.CrateID, ref.CrateName = crate.ID, crate.Name
			crates[crate.ID] = crate
		}
		tasks[id] = ref
	}
	for _, id := range crateIDs {
		if crate, ok := crateRef(id); ok {
			crates[id] = crate
		}
	}
	return tasks, crates
}

// feedbackReportLinkRefsLocked answers the stored links of a report with the
// tasks and crates they name.
func (m *volatileMemory) feedbackReportLinkRefsLocked(workspace, reportID string) ([]FeedbackReportLink, map[string]FeedbackReportTaskRef, map[string]FeedbackReportCrateRef, error) {
	links, err := m.feedbackReportLinksLocked(workspace, reportID)
	if err != nil {
		return nil, nil, nil, err
	}
	taskIDs, crateIDs := []string{}, []string{}
	for _, link := range links {
		if link.TaskID != "" {
			taskIDs = append(taskIDs, link.TaskID)
		}
		if link.CrateID != "" {
			crateIDs = append(crateIDs, link.CrateID)
		}
	}
	tasks, crates := m.feedbackReportRefsLocked(workspace, taskIDs, crateIDs)
	return links, tasks, crates, nil
}

// ListFeedbackReports implements ReportReadStore. Every filter applies
// before the page and the count.
func (m *volatileMemory) ListFeedbackReports(ctx context.Context, query FeedbackReportQuery) (FeedbackReportPage, error) {
	query, err := NormalizeFeedbackReportQuery(query)
	if err != nil {
		return FeedbackReportPage{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	matched := []FeedbackReport{}
	for scope, reports := range m.feedbackReportState.reports {
		if scope.workspace != workspace || (query.EnvironmentID != "" && scope.id != query.EnvironmentID) {
			continue
		}
		for _, report := range reports {
			if report.State != query.State {
				continue
			}
			links, tasks, crates, err := m.feedbackReportLinkRefsLocked(workspace, report.ID)
			if err != nil {
				return FeedbackReportPage{}, err
			}
			if FeedbackReportMatches(query, links, tasks, crates) {
				matched = append(matched, cloneFeedbackReport(report))
			}
		}
	}
	SortFeedbackReports(matched)
	page := FeedbackReportPage{Reports: []FeedbackReport{}, Total: len(matched)}
	if query.Offset < len(matched) {
		page.Reports = matched[query.Offset:min(len(matched), query.Offset+query.Limit)]
	}
	return page, nil
}

// GetFeedbackReport implements ReportReadStore.
func (m *volatileMemory) GetFeedbackReport(ctx context.Context, reportID string) (FeedbackReport, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, _, report, found := m.feedbackReportLocked(workspaceOrDefault(ctx, ""), reportID)
	if !found {
		return FeedbackReport{}, fmt.Errorf("%w: feedback report %s", ErrNotFound, reportID)
	}
	return cloneFeedbackReport(report), nil
}

// ReadFeedbackReportDetails implements ReportReadStore.
func (m *volatileMemory) ReadFeedbackReportDetails(ctx context.Context, reports []FeedbackReport) (FeedbackReportDetails, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	details := FeedbackReportDetails{Links: map[string][]FeedbackReportLink{}, Notes: map[string][]FeedbackReportNote{}}
	taskIDs, crateIDs := []string{}, []string{}
	for _, report := range reports {
		links, err := m.feedbackReportLinksLocked(workspace, report.ID)
		if err != nil {
			return FeedbackReportDetails{}, err
		}
		details.Links[report.ID] = links
		for _, link := range links {
			if link.TaskID != "" {
				taskIDs = append(taskIDs, link.TaskID)
			}
			if link.CrateID != "" {
				crateIDs = append(crateIDs, link.CrateID)
			}
		}
		if report.FixTaskID != "" {
			taskIDs = append(taskIDs, report.FixTaskID)
		}
		notes, err := m.feedbackReportNotesLocked(workspace, report.ID)
		if err != nil {
			return FeedbackReportDetails{}, err
		}
		details.Notes[report.ID] = notes
	}
	details.Tasks, details.Crates = m.feedbackReportRefsLocked(workspace, taskIDs, crateIDs)
	return details, nil
}

// feedbackReportNotesLocked answers the notes of a report in order under
// the held mutex.
func (m *volatileMemory) feedbackReportNotesLocked(workspace, reportID string) ([]FeedbackReportNote, error) {
	rows := []feedbackReportNoteRow{}
	for scope, row := range m.feedbackReportState.notes {
		if scope.workspace == workspace && row.ReportID == reportID {
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b feedbackReportNoteRow) int { return strings.Compare(a.ID, b.ID) })
	notes := make([]FeedbackReportNote, 0, len(rows))
	for _, row := range rows {
		note, err := ParseFeedbackReportNote(row.Detail)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, nil
}

// CountCrateOpenFeedbackReports implements ReportReadStore.
func (m *volatileMemory) CountCrateOpenFeedbackReports(ctx context.Context, crateID string) (map[string]int, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	counts := map[string]int{}
	if crateID == "" {
		return counts, nil
	}
	query := FeedbackReportQuery{CrateID: crateID}
	for scope, reports := range m.feedbackReportState.reports {
		if scope.workspace != workspace {
			continue
		}
		for _, report := range reports {
			if report.State != FeedbackReportOpen {
				continue
			}
			links, tasks, crates, err := m.feedbackReportLinkRefsLocked(workspace, report.ID)
			if err != nil {
				return nil, err
			}
			if FeedbackReportMatches(query, links, tasks, crates) {
				counts[report.EnvironmentID]++
			}
		}
	}
	return counts, nil
}

// RecordFeedbackReportAct implements ReportReadStore. The reason is redacted
// before the mutex is taken. The memory write mutex is the lock that report
// entries and the linking work take, and every check and the whole plan run
// before the first write.
func (m *volatileMemory) RecordFeedbackReportAct(ctx context.Context, act FeedbackReportAct) (FeedbackReportActResult, error) {
	actor, err := RequireActor(ctx)
	if err != nil {
		return FeedbackReportActResult{}, err
	}
	act, err = PrepareFeedbackReportAct(ctx, m, act)
	if err != nil {
		return FeedbackReportActResult{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	scope, key, report, found := m.feedbackReportLocked(workspace, act.ReportID)
	if !found {
		return FeedbackReportActResult{}, fmt.Errorf("%w: feedback report %s", ErrNotFound, act.ReportID)
	}
	stored, err := m.feedbackReportLinksLocked(workspace, report.ID)
	if err != nil {
		return FeedbackReportActResult{}, err
	}
	targets := FeedbackReportActTargets{Tasks: map[string]FeedbackReportActTask{}, Crates: map[string]bool{}}
	for _, id := range act.TaskIDs() {
		task, ok := m.tasks[id]
		if ok && task.Workspace == workspace {
			targets.Tasks[id] = FeedbackReportActTask{Exists: true, Merged: task.State == core.TaskMerged}
		}
	}
	if act.CrateID != "" {
		_, targets.Crates[act.CrateID] = m.crates[memoryScopedKey{workspace: workspace, id: act.CrateID}]
	}
	write, err := PlanFeedbackReportAct(cloneFeedbackReport(report), stored, act, targets, actor)
	if err != nil || !write.Changed {
		return FeedbackReportActResult{}, err
	}
	for _, link := range append(slices.Clone(write.NewLinks), write.UpdatedLinks...) {
		m.feedbackReportState.links[memoryScopedKey{workspace: workspace, id: link.ID}] = feedbackReportLinkRowOf(link)
	}
	m.feedbackReportState.reports[scope][key] = cloneFeedbackReport(write.Report)
	m.appendEventLocked(ctx, write.Event)
	return FeedbackReportActResult{Changed: true}, nil
}
