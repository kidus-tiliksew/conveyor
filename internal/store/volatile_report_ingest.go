package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// feedbackReportState mirrors the four report tables of the durable
// backends. reports holds each environment's reports by problem key. links,
// notes, and lineReads hold the rows of feedback_report_links,
// feedback_report_notes, and feedback_report_line_reads with the same keys
// and fields. Ingest writes reports only. The linking work writes the line
// reads, the links, the notes, and each report's linking attempt and end. Every mutation runs
// under the memory write mutex, the volatile backend's critical section,
// which signals take too (component-feedback-reports invariant 6, Storage).
type feedbackReportState struct {
	reports map[memoryScopedKey]map[string]FeedbackReport
	// links is keyed by workspace and link ID, as the table's primary key.
	links map[memoryScopedKey]feedbackReportLinkRow
	// notes is keyed by workspace and note ID, as the table's primary key.
	notes map[memoryScopedKey]feedbackReportNoteRow
	// lineReads is keyed by workspace and report ID, as the table's primary
	// key: one answer for each report.
	lineReads map[memoryScopedKey]feedbackReportLineReadRow
	// byID locates each report by workspace and report ID, as the table's
	// primary key does, so a read of one report reads no other report.
	byID map[memoryScopedKey]feedbackReportLocation
	// linksByReport and notesByReport hold, by workspace and report ID, the
	// IDs of a report's link and note rows, as the tables' report indexes
	// do, so the links and notes of one report are read without reading the
	// rows of other reports.
	linksByReport map[memoryScopedKey]map[string]bool
	notesByReport map[memoryScopedKey]map[string]bool
	// readCopies counts the report records, links, and notes that report
	// reads copy out, so a test can prove that a read's work follows its
	// page and not the workspace's history. It is shared by every copy of
	// the state.
	readCopies *atomic.Int64
}

// feedbackReportLocation is where a report is kept: its environment's map
// key and its problem key.
type feedbackReportLocation struct {
	scope memoryScopedKey
	key   string
}

// feedbackReportLinkRow is one row of feedback_report_links. Exactly one of
// TaskID and CrateID is set. State is possible, confirmed, or removed. A nil
// time and its empty actor were not recorded.
type feedbackReportLinkRow struct {
	ID          string
	ReportID    string
	TaskID      *string
	CrateID     *string
	State       string
	Evidence    json.RawMessage
	CreatedAt   time.Time
	CreatedBy   string
	ConfirmedBy *string
	ConfirmedAt *time.Time
	RemovedBy   *string
	RemovedAt   *time.Time
}

// feedbackReportNoteRow is one row of feedback_report_notes. Detail holds
// what the note names.
type feedbackReportNoteRow struct {
	ID        string
	ReportID  string
	Kind      string
	Detail    json.RawMessage
	CreatedAt time.Time
}

// feedbackReportLineReadRow is one row of feedback_report_line_reads: the
// answer of the line read of one report.
type feedbackReportLineReadRow struct {
	ReportID       string
	Repository     string
	ResolvedCommit string
	File           string
	Line           int
	Answer         json.RawMessage
	AnsweredAt     time.Time
}

func newFeedbackReportState() feedbackReportState {
	return feedbackReportState{
		reports:   map[memoryScopedKey]map[string]FeedbackReport{},
		links:     map[memoryScopedKey]feedbackReportLinkRow{},
		notes:     map[memoryScopedKey]feedbackReportNoteRow{},
		lineReads: map[memoryScopedKey]feedbackReportLineReadRow{},
		byID:      map[memoryScopedKey]feedbackReportLocation{},

		linksByReport: map[memoryScopedKey]map[string]bool{},
		notesByReport: map[memoryScopedKey]map[string]bool{},
		readCopies:    new(atomic.Int64),
	}
}

// putFeedbackReportLinkLocked writes one link row and its report index
// entry under the held write mutex. Every link write goes through it.
func (m *volatileMemory) putFeedbackReportLinkLocked(workspace string, row feedbackReportLinkRow) {
	m.feedbackReportState.links[memoryScopedKey{workspace: workspace, id: row.ID}] = row
	index := memoryScopedKey{workspace: workspace, id: row.ReportID}
	if m.feedbackReportState.linksByReport[index] == nil {
		m.feedbackReportState.linksByReport[index] = map[string]bool{}
	}
	m.feedbackReportState.linksByReport[index][row.ID] = true
}

// putFeedbackReportNoteLocked writes one note row and its report index
// entry under the held write mutex. Every note write goes through it.
func (m *volatileMemory) putFeedbackReportNoteLocked(workspace string, row feedbackReportNoteRow) {
	m.feedbackReportState.notes[memoryScopedKey{workspace: workspace, id: row.ID}] = row
	index := memoryScopedKey{workspace: workspace, id: row.ReportID}
	if m.feedbackReportState.notesByReport[index] == nil {
		m.feedbackReportState.notesByReport[index] = map[string]bool{}
	}
	m.feedbackReportState.notesByReport[index][row.ID] = true
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
	// The events carry the system actor, and a request without an actor is
	// refused before any write (component-persistence, Actor context).
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
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
	// Report IDs are unique in the workspace. The mutex serializes every
	// report write, so the set holds until this request has written.
	held := map[string]bool{}
	for reportScope, reports := range m.feedbackReportState.reports {
		if reportScope.workspace != workspace {
			continue
		}
		for _, report := range reports {
			held[report.ID] = true
		}
	}
	plan, err := PlanFeedbackReports(ctx, prepared, key.Label, matched, open, func(id string) (bool, error) { return held[id], nil })
	if err != nil {
		return err
	}
	if stored == nil {
		stored = map[string]FeedbackReport{}
		m.feedbackReportState.reports[scope] = stored
	}
	for _, change := range plan.Changes {
		stored[change.Report.Key] = cloneFeedbackReport(change.Report)
		if change.New {
			m.feedbackReportState.byID[memoryScopedKey{workspace: workspace, id: change.Report.ID}] = feedbackReportLocation{scope: scope, key: change.Report.Key}
		}
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
	if report.LinkingLastAttemptAt != nil {
		at := *report.LinkingLastAttemptAt
		report.LinkingLastAttemptAt = &at
	}
	if report.FixTaskSetAt != nil {
		at := *report.FixTaskSetAt
		report.FixTaskSetAt = &at
	}
	if report.LineProgress != nil {
		progress := CheckStoredFeedbackLineProgress(cloneFeedbackLineProgress(*report.LineProgress))
		report.LineProgress = &progress
	}
	if report.TimingResult != nil {
		// A JSON round trip copies every slice and time pointer, as the
		// durable backends' columns do.
		copied, _ := ParseFeedbackTimingResult([]byte(FeedbackTimingResultJSON(*report.TimingResult)))
		report.TimingResult = copied
	}
	return report
}
