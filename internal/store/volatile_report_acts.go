package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

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

// feedbackReportRef is a report the filters selected, held by reference: its
// order keys and where it is kept. Selecting copies no record, and a list
// keeps at most Offset+Limit of them (feedbackReportTopRefs).
type feedbackReportRef struct {
	lastSeen time.Time
	id       string
	location feedbackReportLocation
}

// feedbackReportLinkMatchesLocked decides the task, crate, and no_crate
// filters for one report from its link rows, through the report's link
// index and the task and crate maps, by reference and without decoding
// evidence (store.FeedbackReportServed and store.FeedbackReportInCrate).
func (m *volatileMemory) feedbackReportLinkMatchesLocked(workspace, reportID string, query FeedbackReportQuery) bool {
	if query.TaskID == "" && query.CrateID == "" && !query.NoCrate {
		return true
	}
	taskMatched, crateMatched, inAnyCrate := false, false, false
	crateOf := func(row feedbackReportLinkRow) (string, bool) {
		if row.State != FeedbackReportLinkPossible && row.State != FeedbackReportLinkConfirmed {
			return "", false
		}
		switch {
		case row.TaskID != nil && row.CrateID == nil:
			task, ok := m.tasks[*row.TaskID]
			if !ok || task.Workspace != workspace {
				return "", false
			}
			if _, ok := m.crates[memoryScopedKey{workspace: workspace, id: task.TaskCrateID()}]; ok {
				return task.TaskCrateID(), true
			}
			return "", true
		case row.CrateID != nil && row.TaskID == nil:
			if _, ok := m.crates[memoryScopedKey{workspace: workspace, id: *row.CrateID}]; ok {
				return *row.CrateID, true
			}
		}
		return "", false
	}
	for linkID := range m.feedbackReportState.linksByReport[memoryScopedKey{workspace: workspace, id: reportID}] {
		row, ok := m.feedbackReportState.links[memoryScopedKey{workspace: workspace, id: linkID}]
		if !ok || row.ReportID != reportID {
			continue
		}
		crate, served := crateOf(row)
		if !served {
			continue
		}
		if row.TaskID != nil && *row.TaskID == query.TaskID {
			taskMatched = true
		}
		if crate != "" {
			inAnyCrate = true
			if crate == query.CrateID {
				crateMatched = true
			}
		}
	}
	return (query.TaskID == "" || taskMatched) && (query.CrateID == "" || crateMatched) && (!query.NoCrate || !inAnyCrate)
}

// feedbackReportRefsBefore orders references as a list does: newest
// last_seen first, then by ID.
func feedbackReportRefsBefore(a, b feedbackReportRef) bool {
	if n := a.lastSeen.Compare(b.lastSeen); n != 0 {
		return n > 0
	}
	return a.id < b.id
}

// feedbackReportTopRefs keeps at most keep references, the first ones in
// list order, as a heap whose root is the last kept reference. A reference
// after the root is dropped at once, so the kept set never passes keep.
type feedbackReportTopRefs struct {
	keep int
	refs []feedbackReportRef
}

func (h *feedbackReportTopRefs) offer(ref feedbackReportRef) {
	if h.keep <= 0 {
		return
	}
	if len(h.refs) < h.keep {
		h.refs = append(h.refs, ref)
		for i := len(h.refs) - 1; i > 0; {
			parent := (i - 1) / 2
			if !feedbackReportRefsBefore(h.refs[parent], h.refs[i]) {
				break
			}
			h.refs[parent], h.refs[i] = h.refs[i], h.refs[parent]
			i = parent
		}
		return
	}
	if !feedbackReportRefsBefore(ref, h.refs[0]) {
		return
	}
	h.refs[0] = ref
	for i := 0; ; {
		last, left, right := i, 2*i+1, 2*i+2
		if left < len(h.refs) && feedbackReportRefsBefore(h.refs[last], h.refs[left]) {
			last = left
		}
		if right < len(h.refs) && feedbackReportRefsBefore(h.refs[last], h.refs[right]) {
			last = right
		}
		if last == i {
			return
		}
		h.refs[i], h.refs[last] = h.refs[last], h.refs[i]
		i = last
	}
}

// forEachFeedbackReportMatchLocked calls fn with each report of the
// workspace that the query selects, by reference, under the held mutex.
func (m *volatileMemory) forEachFeedbackReportMatchLocked(workspace string, query FeedbackReportQuery, fn func(feedbackReportRef)) {
	for scope, reports := range m.feedbackReportState.reports {
		if scope.workspace != workspace || (query.EnvironmentID != "" && scope.id != query.EnvironmentID) {
			continue
		}
		for key, report := range reports {
			if report.State != query.State || !m.feedbackReportLinkMatchesLocked(workspace, report.ID, query) {
				continue
			}
			fn(feedbackReportRef{lastSeen: report.LastSeen, id: report.ID, location: feedbackReportLocation{scope: scope, key: key}})
		}
	}
}

// ListFeedbackReports implements ReportReadStore under one read lock, which
// every write of a report, link, note, task, crate, environment, and signal
// waits for: the records, links, notes, fix tasks, and crate membership are
// one state, and each title, name, and signal is read once with them
// (component-feedback-reports v4 invariant 21). While it runs it keeps a count, at
// most Offset+Limit references (sort keys and where a report is kept), and
// the page's records and details. A query with RefuseAbove counts first and
// keeps nothing else, and a count over RefuseAbove answers the count alone
// (component-feedback-reports invariants 21 and 24).
func (m *volatileMemory) ListFeedbackReports(ctx context.Context, query FeedbackReportQuery) (FeedbackReportPage, error) {
	query, err := NormalizeFeedbackReportQuery(query)
	if err != nil {
		return FeedbackReportPage{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	page := FeedbackReportPage{Reports: []FeedbackReport{}, Details: NewFeedbackReportDetails()}
	page.Details.ReadAt = FeedbackReadClock(ctx)
	if query.RefuseAbove > 0 {
		m.forEachFeedbackReportMatchLocked(workspace, query, func(feedbackReportRef) { page.Total++ })
		if page.Refused(query) {
			return page, nil
		}
		page.Total = 0
	}
	top := feedbackReportTopRefs{keep: query.Offset + query.Limit}
	m.forEachFeedbackReportMatchLocked(workspace, query, func(ref feedbackReportRef) {
		page.Total++
		top.offer(ref)
	})
	m.feedbackReportState.readCopies.Add(int64(len(top.refs)))
	slices.SortFunc(top.refs, func(a, b feedbackReportRef) int {
		if feedbackReportRefsBefore(a, b) {
			return -1
		}
		return 1
	})
	if query.Offset < len(top.refs) {
		for _, ref := range top.refs[query.Offset:] {
			page.Reports = append(page.Reports, cloneFeedbackReport(m.feedbackReportState.reports[ref.location.scope][ref.location.key]))
		}
	}
	// The details are read under the same read lock as the page.
	readAt := page.Details.ReadAt
	if page.Details, err = m.feedbackReportDetailsLocked(workspace, page.Reports); err != nil {
		return FeedbackReportPage{}, err
	}
	page.Details.ReadAt = readAt
	return page, nil
}

// ReadFeedbackReport implements ReportReadStore under one read lock.
func (m *volatileMemory) ReadFeedbackReport(ctx context.Context, reportID string) (FeedbackReport, FeedbackReportDetails, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, _, report, found := m.feedbackReportLocked(workspace, reportID)
	if !found {
		return FeedbackReport{}, FeedbackReportDetails{}, fmt.Errorf("%w: feedback report %s", ErrNotFound, reportID)
	}
	report = cloneFeedbackReport(report)
	details, err := m.feedbackReportDetailsLocked(workspace, []FeedbackReport{report})
	if err != nil {
		return FeedbackReport{}, FeedbackReportDetails{}, err
	}
	details.ReadAt = FeedbackReadClock(ctx)
	return report, details, nil
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

// ReadFeedbackReports implements ReportReadStore under one read lock.
func (m *volatileMemory) ReadFeedbackReports(ctx context.Context, reportIDs []string) ([]FeedbackReport, FeedbackReportDetails, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	reports := []FeedbackReport{}
	seen := map[string]bool{}
	for _, id := range reportIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, _, report, found := m.feedbackReportLocked(workspace, id); found {
			reports = append(reports, cloneFeedbackReport(report))
		}
	}
	details, err := m.feedbackReportDetailsLocked(workspace, reports)
	if err != nil {
		return nil, FeedbackReportDetails{}, err
	}
	details.ReadAt = FeedbackReadClock(ctx)
	return reports, details, nil
}

// feedbackReportDetailsLocked reads the details of reports under the held
// mutex.
func (m *volatileMemory) feedbackReportDetailsLocked(workspace string, reports []FeedbackReport) (FeedbackReportDetails, error) {
	details := NewFeedbackReportDetails()
	taskIDs, crateIDs := []string{}, []string{}
	for _, report := range reports {
		if _, ok := details.Environments[report.EnvironmentID]; !ok {
			key := memoryScopedKey{workspace: workspace, id: report.EnvironmentID}
			if environment, ok := m.environmentState.environments[key]; ok {
				ref := FeedbackReportEnvironment{ID: environment.ID, Name: environment.Name}
				if signal, ok := m.feedbackState.signals[key]; ok {
					ref.Signal = &signal
				}
				details.Environments[report.EnvironmentID] = ref
			}
		}
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
	m.feedbackReportState.readCopies.Add(int64(len(reports)))
	for _, links := range details.Links {
		m.feedbackReportState.readCopies.Add(int64(len(links)))
	}
	return details, nil
}

// feedbackReportNotesLocked answers the notes of a report in order under
// the held mutex.
func (m *volatileMemory) feedbackReportNotesLocked(workspace, reportID string) ([]FeedbackReportNote, error) {
	rows := []feedbackReportNoteRow{}
	for noteID := range m.feedbackReportState.notesByReport[memoryScopedKey{workspace: workspace, id: reportID}] {
		if row, ok := m.feedbackReportState.notes[memoryScopedKey{workspace: workspace, id: noteID}]; ok && row.ReportID == reportID {
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
	m.feedbackReportState.readCopies.Add(int64(len(notes)))
	return notes, nil
}

// CountCrateOpenFeedbackReports implements ReportReadStore under one read
// lock. It selects by reference and copies no record.
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
			// Selected by reference, with the list's crate rule; nothing is
			// copied or decoded.
			if m.feedbackReportLinkMatchesLocked(workspace, report.ID, query) {
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
		m.putFeedbackReportLinkLocked(workspace, feedbackReportLinkRowOf(link))
	}
	m.feedbackReportState.reports[scope][key] = cloneFeedbackReport(write.Report)
	m.appendEventLocked(ctx, write.Event)
	return FeedbackReportActResult{Changed: true}, nil
}
