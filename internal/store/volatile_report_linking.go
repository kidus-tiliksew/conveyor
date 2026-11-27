package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

var _ ReportLinkingStore = (*volatileMemory)(nil)

// feedbackReportLocked finds a report of the workspace by ID under the held
// mutex, with the key of its environment's map.
func (m *volatileMemory) feedbackReportLocked(workspace, reportID string) (memoryScopedKey, string, FeedbackReport, bool) {
	for scope, reports := range m.feedbackReportState.reports {
		if scope.workspace != workspace {
			continue
		}
		for key, report := range reports {
			if report.ID == reportID {
				return scope, key, report, true
			}
		}
	}
	return memoryScopedKey{}, "", FeedbackReport{}, false
}

// ClaimFeedbackReportLinking implements ReportLinkingStore. The memory write
// mutex is the report write lock.
func (m *volatileMemory) ClaimFeedbackReportLinking(ctx context.Context, reportID string, now time.Time) (FeedbackReport, bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return FeedbackReport{}, false, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	scope, key, report, found := m.feedbackReportLocked(workspace, reportID)
	if !found {
		return FeedbackReport{}, false, fmt.Errorf("%w: feedback report %s", ErrNotFound, reportID)
	}
	if !FeedbackReportLinkingClaimable(report, now) {
		return cloneFeedbackReport(report), false, nil
	}
	at := EnvironmentTime(now)
	report.LinkingLastAttemptAt = &at
	m.feedbackReportState.reports[scope][key] = cloneFeedbackReport(report)
	return cloneFeedbackReport(report), true, nil
}

// ReadFeedbackReportLineRead implements ReportLinkingStore.
func (m *volatileMemory) ReadFeedbackReportLineRead(ctx context.Context, reportID string) (FeedbackReportLineRead, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	row, found := m.feedbackReportState.lineReads[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: reportID}]
	if !found {
		return FeedbackReportLineRead{}, false, nil
	}
	answer, err := ParseFeedbackReportLineAnswer(row.Answer)
	if err != nil {
		return FeedbackReportLineRead{}, false, err
	}
	return FeedbackReportLineRead{ReportID: row.ReportID, Repository: row.Repository, ResolvedCommit: row.ResolvedCommit, File: row.File, Line: row.Line, Answer: answer, AnsweredAt: row.AnsweredAt}, true, nil
}

// RecordFeedbackReportLineRead implements ReportLinkingStore. A stored
// answer is kept.
func (m *volatileMemory) RecordFeedbackReportLineRead(ctx context.Context, read FeedbackReportLineRead) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	read, err := NormalizeFeedbackReportLineRead(read)
	if err != nil {
		return err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, _, _, found := m.feedbackReportLocked(workspace, read.ReportID); !found {
		return fmt.Errorf("%w: feedback report %s", ErrNotFound, read.ReportID)
	}
	key := memoryScopedKey{workspace: workspace, id: read.ReportID}
	if _, stored := m.feedbackReportState.lineReads[key]; stored {
		return nil
	}
	m.feedbackReportState.lineReads[key] = feedbackReportLineReadRow{ReportID: read.ReportID, Repository: read.Repository, ResolvedCommit: read.ResolvedCommit,
		File: read.File, Line: read.Line, Answer: json.RawMessage(FeedbackReportLineAnswerJSON(read.Answer)), AnsweredAt: read.AnsweredAt}
	return nil
}

// FinishFeedbackReportLinking implements ReportLinkingStore. The plan and
// every check run before the first write, under the mutex.
func (m *volatileMemory) FinishFeedbackReportLinking(ctx context.Context, linking FeedbackReportLinking) (bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return false, err
	}
	if linking.EndedAt.IsZero() {
		return false, fmt.Errorf("the end of linking needs its time")
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	scope, key, report, found := m.feedbackReportLocked(workspace, linking.ReportID)
	if !found {
		return false, fmt.Errorf("%w: feedback report %s", ErrNotFound, linking.ReportID)
	}
	if report.LinkingEndedAt != nil {
		return false, nil
	}
	stored, err := m.feedbackReportLinksLocked(workspace, report.ID)
	if err != nil {
		return false, err
	}
	write := PlanFeedbackReportLinking(report, stored, linking)
	for _, link := range append(slices.Clone(write.New), write.Extended...) {
		m.feedbackReportState.links[memoryScopedKey{workspace: workspace, id: link.ID}] = feedbackReportLinkRowOf(link)
	}
	for _, note := range write.Notes {
		m.feedbackReportState.notes[memoryScopedKey{workspace: workspace, id: note.ID}] = feedbackReportNoteRow{ID: note.ID, ReportID: report.ID, Kind: note.Note.Kind,
			Detail: json.RawMessage(FeedbackReportNoteJSON(note.Note)), CreatedAt: write.Event.At}
	}
	ended := write.Event.At
	report.LinkingEndedAt = &ended
	m.feedbackReportState.reports[scope][key] = cloneFeedbackReport(report)
	m.appendEventLocked(ctx, write.Event)
	return true, nil
}

// feedbackReportLinkRowOf is the row of a link.
func feedbackReportLinkRowOf(link FeedbackReportLink) feedbackReportLinkRow {
	row := feedbackReportLinkRow{ID: link.ID, ReportID: link.ReportID, State: link.State, Evidence: json.RawMessage(FeedbackReportEvidenceJSON(link.Evidence)),
		CreatedAt: link.CreatedAt, CreatedBy: link.CreatedBy, ConfirmedAt: link.ConfirmedAt, RemovedAt: link.RemovedAt}
	if link.TaskID != "" {
		task := link.TaskID
		row.TaskID = &task
	}
	if link.CrateID != "" {
		crate := link.CrateID
		row.CrateID = &crate
	}
	if link.ConfirmedAt != nil {
		by := link.ConfirmedBy
		row.ConfirmedBy = &by
	}
	if link.RemovedAt != nil {
		by := link.RemovedBy
		row.RemovedBy = &by
	}
	return row
}

// feedbackReportLinksLocked returns the stored links of a report, ordered by
// ID, under the held mutex.
func (m *volatileMemory) feedbackReportLinksLocked(workspace, reportID string) ([]FeedbackReportLink, error) {
	links := []FeedbackReportLink{}
	for scope, row := range m.feedbackReportState.links {
		if scope.workspace != workspace || row.ReportID != reportID {
			continue
		}
		evidence, err := ParseFeedbackReportEvidence(row.Evidence)
		if err != nil {
			return nil, err
		}
		link := FeedbackReportLink{ID: row.ID, ReportID: row.ReportID, State: row.State, Evidence: evidence, CreatedAt: row.CreatedAt, CreatedBy: row.CreatedBy}
		if row.TaskID != nil {
			link.TaskID = *row.TaskID
		}
		if row.CrateID != nil {
			link.CrateID = *row.CrateID
		}
		if row.ConfirmedAt != nil && row.ConfirmedBy != nil {
			at := *row.ConfirmedAt
			link.ConfirmedAt, link.ConfirmedBy = &at, *row.ConfirmedBy
		}
		if row.RemovedAt != nil && row.RemovedBy != nil {
			at := *row.RemovedAt
			link.RemovedAt, link.RemovedBy = &at, *row.RemovedBy
		}
		links = append(links, link)
	}
	slices.SortFunc(links, func(a, b FeedbackReportLink) int { return strings.Compare(a.ID, b.ID) })
	return links, nil
}

// ListFeedbackReportLinks implements ReportLinkingStore.
func (m *volatileMemory) ListFeedbackReportLinks(ctx context.Context, reportID string) ([]FeedbackReportLink, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.feedbackReportLinksLocked(workspaceOrDefault(ctx, ""), reportID)
}

// ListFeedbackReportNotes implements ReportLinkingStore. The notes of a
// report are written in one transaction with IDs numbered in order.
func (m *volatileMemory) ListFeedbackReportNotes(ctx context.Context, reportID string) ([]FeedbackReportNote, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
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

// ListPendingFeedbackReports implements ReportLinkingStore.
func (m *volatileMemory) ListPendingFeedbackReports(ctx context.Context, environmentID string) ([]FeedbackReport, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	reports := []FeedbackReport{}
	for _, report := range m.feedbackReportState.reports[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: environmentID}] {
		if report.LinkingEndedAt == nil {
			reports = append(reports, cloneFeedbackReport(report))
		}
	}
	slices.SortFunc(reports, func(a, b FeedbackReport) int { return strings.Compare(a.ID, b.ID) })
	return reports, nil
}
