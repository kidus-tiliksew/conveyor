package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.ReportReadStore = (*Store)(nil)

// feedbackServedLinkReports is a subquery of the report IDs that have a
// served link meeting cond: a possible or confirmed link to a task or a crate
// the workspace holds. tc is the crate a linked task is a member of at the
// read, and lc the crate a link names. It states the rule of
// store.FeedbackReportServed and store.FeedbackReportInCrate
// (component-feedback-reports invariant 21). The subquery's first
// placeholder is the workspace ID.
func feedbackServedLinkReports(cond string) string {
	return `SELECT l.report_id FROM feedback_report_links l
LEFT JOIN tasks t ON t.workspace_id=l.workspace_id AND t.id=l.task_id
LEFT JOIN crates lc ON lc.workspace_id=l.workspace_id AND lc.id=l.crate_id
LEFT JOIN crates tc ON tc.workspace_id=t.workspace_id AND tc.id=t.crate_id
WHERE l.workspace_id=? AND l.state IN ('possible','confirmed')
AND ((l.task_id IS NOT NULL AND l.crate_id IS NULL AND t.id IS NOT NULL) OR (l.crate_id IS NOT NULL AND l.task_id IS NULL AND lc.id IS NOT NULL))
AND ` + cond
}

// feedbackReportFilter renders the WHERE clause of a list and its arguments.
// Every filter is a predicate of the one statement, so it runs before the
// page and the count, and IN keeps a report that matches twice to one row.
func feedbackReportFilter(ws string, query store.FeedbackReportQuery) (string, []any) {
	clauses := []string{"r.workspace_id=?", "r.state=?"}
	args := []any{ws, query.State}
	if query.EnvironmentID != "" {
		clauses = append(clauses, "r.environment_id=?")
		args = append(args, query.EnvironmentID)
	}
	if query.TaskID != "" {
		clauses = append(clauses, "r.id IN ("+feedbackServedLinkReports("l.task_id=?")+")")
		args = append(args, ws, query.TaskID)
	}
	if query.CrateID != "" {
		clauses = append(clauses, "r.id IN ("+feedbackServedLinkReports("(lc.id=? OR tc.id=?)")+")")
		args = append(args, ws, query.CrateID, query.CrateID)
	}
	if query.NoCrate {
		clauses = append(clauses, "r.id NOT IN ("+feedbackServedLinkReports("(lc.id IS NOT NULL OR tc.id IS NOT NULL)")+")")
		args = append(args, ws)
	}
	return strings.Join(clauses, " AND "), args
}

// prefixFeedbackReportColumns qualifies every report column with a table
// alias.
func prefixFeedbackReportColumns(alias string) string {
	columns := strings.Split(strings.ReplaceAll(feedbackReportColumns, "\n", ""), ",")
	for i, column := range columns {
		columns[i] = alias + "." + strings.TrimSpace(column)
	}
	return strings.Join(columns, ",")
}

// feedbackReportNullableColumns are the report columns that hold NULL in a
// stored row. Every other column is NOT NULL.
var feedbackReportNullableColumns = map[string]bool{
	"line": true, "closed_at": true, "closed_by": true, "close_reason": true, "reopened_at": true, "reopen_source": true,
	"reopen_source_label": true, "reopen_actor_id": true, "linking_ended_at": true, "fix_task_id": true,
	"linking_last_attempt_at": true, "line_progress": true, "timing_result": true, "fix_task_set_by": true, "fix_task_set_at": true,
}

// feedbackReportPageColumns selects the report columns of the page
// subquery p for a row that may have no report. A NOT NULL column gets a
// placeholder of its type, so the one row of an empty page decodes and is
// recognized by its empty ID.
func feedbackReportPageColumns() string {
	columns := strings.Split(strings.ReplaceAll(feedbackReportColumns, "\n", ""), ",")
	for i, column := range columns {
		column = strings.TrimSpace(column)
		switch {
		case feedbackReportNullableColumns[column]:
			columns[i] = "p." + column
		case column == "first_seen" || column == "last_seen" || column == "first_entry_seen" || column == "created_at":
			columns[i] = "COALESCE(p." + column + ",CAST('1970-01-01 00:00:00' AS DATETIME(6)))"
		case column == "occurrence_count" || column == "in_inbox":
			columns[i] = "COALESCE(p." + column + ",0)"
		case column == "summary_redactions":
			columns[i] = "COALESCE(p." + column + ",'{}')"
		default:
			columns[i] = "COALESCE(p." + column + ",'')"
		}
	}
	return strings.Join(columns, ",")
}

// feedbackReportViewStatement renders the one statement of every report
// read on SingleStore. SingleStore runs each statement on one committed
// view, and offers no isolation that holds a view across statements, so the
// count, the page's records, their links with each linked task's title and
// crate at the read, their notes, and the fix task are all read here, in one
// statement (component-feedback-reports invariants 18 and 21). where and its
// arguments select the reports. The count subquery is joined onto the page
// subquery, so a page past the end answers one row with no report and the
// total.
func feedbackReportViewStatement(ws, where string, whereArgs []any, limit, offset int) (string, []any) {
	statement := `SELECT ` + feedbackReportPageColumns() + `, t.total, la.links, na.notes, ft.id, ft.title, fc.id, fc.name
FROM (SELECT COUNT(*) AS total FROM feedback_reports r WHERE ` + where + `) t
LEFT JOIN (SELECT ` + prefixFeedbackReportColumns("r") + ` FROM feedback_reports r WHERE ` + where + ` ORDER BY r.last_seen DESC, r.id LIMIT ? OFFSET ?) p ON 1=1
LEFT JOIN (SELECT l.report_id, JSON_AGG(JSON_BUILD_OBJECT('id',l.id,'task_id',l.task_id,'crate_id',l.crate_id,'state',l.state,'evidence',l.evidence:>JSON,
 'created_at',l.created_at,'created_by',l.created_by,'confirmed_by',l.confirmed_by,'confirmed_at',l.confirmed_at,'removed_by',l.removed_by,'removed_at',l.removed_at,
 'task_ref',lt.id,'task_title',lt.title,'task_crate_id',tc.id,'task_crate_name',tc.name,'crate_ref',lc.id,'crate_name',lc.name) ORDER BY l.id) AS links
 FROM feedback_report_links l
 LEFT JOIN tasks lt ON lt.workspace_id=l.workspace_id AND lt.id=l.task_id
 LEFT JOIN crates tc ON tc.workspace_id=lt.workspace_id AND tc.id=lt.crate_id
 LEFT JOIN crates lc ON lc.workspace_id=l.workspace_id AND lc.id=l.crate_id
 WHERE l.workspace_id=? AND l.report_id IN (SELECT r.id FROM feedback_reports r WHERE ` + where + `) GROUP BY l.report_id) la ON la.report_id=p.id
LEFT JOIN (SELECT n.report_id, JSON_AGG(n.detail:>JSON ORDER BY n.id) AS notes FROM feedback_report_notes n
 WHERE n.workspace_id=? AND n.report_id IN (SELECT r.id FROM feedback_reports r WHERE ` + where + `) GROUP BY n.report_id) na ON na.report_id=p.id
LEFT JOIN tasks ft ON ft.workspace_id=? AND ft.id=p.fix_task_id
LEFT JOIN crates fc ON fc.workspace_id=ft.workspace_id AND fc.id=ft.crate_id
ORDER BY p.last_seen DESC, p.id`
	args := append([]any{}, whereArgs...)
	args = append(append(args, whereArgs...), limit, offset)
	args = append(append(args, ws), whereArgs...)
	args = append(append(args, ws), whereArgs...)
	args = append(args, ws)
	return statement, args
}

// feedbackReportViewLink is one link of the view statement, with the task or
// crate it names as the read found them.
type feedbackReportViewLink struct {
	ID            string          `json:"id"`
	TaskID        *string         `json:"task_id"`
	CrateID       *string         `json:"crate_id"`
	State         string          `json:"state"`
	Evidence      json.RawMessage `json:"evidence"`
	CreatedAt     string          `json:"created_at"`
	CreatedBy     string          `json:"created_by"`
	ConfirmedBy   *string         `json:"confirmed_by"`
	ConfirmedAt   *string         `json:"confirmed_at"`
	RemovedBy     *string         `json:"removed_by"`
	RemovedAt     *string         `json:"removed_at"`
	TaskRef       *string         `json:"task_ref"`
	TaskTitle     *string         `json:"task_title"`
	TaskCrateID   *string         `json:"task_crate_id"`
	TaskCrateName *string         `json:"task_crate_name"`
	CrateRef      *string         `json:"crate_ref"`
	CrateName     *string         `json:"crate_name"`
}

// feedbackReportJSONTime reads a DATETIME(6) that JSON_BUILD_OBJECT rendered
// as text, as UTC at the stored precision.
func feedbackReportJSONTime(text string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if at, err := time.ParseInLocation(layout, text, time.UTC); err == nil {
			return store.EnvironmentTime(at), nil
		}
	}
	return time.Time{}, fmt.Errorf("decode link time: unexpected form")
}

func feedbackReportJSONTimePtr(text *string) (*time.Time, error) {
	if text == nil {
		return nil, nil
	}
	at, err := feedbackReportJSONTime(*text)
	if err != nil {
		return nil, err
	}
	return &at, nil
}

// addFeedbackReportViewLinks decodes the links of one report into details.
func addFeedbackReportViewLinks(details *store.FeedbackReportDetails, reportID string, raw sql.NullString) error {
	if !raw.Valid {
		return nil
	}
	var rows []feedbackReportViewLink
	if err := json.Unmarshal([]byte(raw.String), &rows); err != nil {
		return fmt.Errorf("decode report links: %w", err)
	}
	for _, row := range rows {
		link := store.FeedbackReportLink{ID: row.ID, ReportID: reportID, State: row.State, CreatedBy: row.CreatedBy}
		var err error
		if link.Evidence, err = store.ParseFeedbackReportEvidence(row.Evidence); err != nil {
			return err
		}
		if link.CreatedAt, err = feedbackReportJSONTime(row.CreatedAt); err != nil {
			return err
		}
		if row.TaskID != nil {
			link.TaskID = *row.TaskID
		}
		if row.CrateID != nil {
			link.CrateID = *row.CrateID
		}
		if row.ConfirmedBy != nil && row.ConfirmedAt != nil {
			if link.ConfirmedAt, err = feedbackReportJSONTimePtr(row.ConfirmedAt); err != nil {
				return err
			}
			link.ConfirmedBy = *row.ConfirmedBy
		}
		if row.RemovedBy != nil && row.RemovedAt != nil {
			if link.RemovedAt, err = feedbackReportJSONTimePtr(row.RemovedAt); err != nil {
				return err
			}
			link.RemovedBy = *row.RemovedBy
		}
		if row.TaskRef != nil && row.TaskTitle != nil {
			ref := store.FeedbackReportTaskRef{ID: *row.TaskRef, Title: *row.TaskTitle}
			if row.TaskCrateID != nil && row.TaskCrateName != nil {
				ref.CrateID, ref.CrateName = *row.TaskCrateID, *row.TaskCrateName
				details.Crates[ref.CrateID] = store.FeedbackReportCrateRef{ID: ref.CrateID, Name: ref.CrateName}
			}
			details.Tasks[ref.ID] = ref
		}
		if row.CrateRef != nil && row.CrateName != nil {
			details.Crates[*row.CrateRef] = store.FeedbackReportCrateRef{ID: *row.CrateRef, Name: *row.CrateName}
		}
		details.Links[reportID] = append(details.Links[reportID], link)
	}
	return nil
}

// addFeedbackReportViewNotes decodes the notes of one report into details.
func addFeedbackReportViewNotes(details *store.FeedbackReportDetails, reportID string, raw sql.NullString) error {
	if !raw.Valid {
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(raw.String), &rows); err != nil {
		return fmt.Errorf("decode report notes: %w", err)
	}
	for _, row := range rows {
		note, err := store.ParseFeedbackReportNote(row)
		if err != nil {
			return err
		}
		details.Notes[reportID] = append(details.Notes[reportID], note)
	}
	return nil
}

// readFeedbackReportView runs the view statement and answers the page, the
// total, and the details.
func (s *Store) readFeedbackReportView(ctx context.Context, ws, where string, whereArgs []any, limit, offset int) (store.FeedbackReportPage, error) {
	statement, args := feedbackReportViewStatement(ws, where, whereArgs, limit, offset)
	rows, err := documentRows(ctx, s.db, statement, args...)
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	page := store.FeedbackReportPage{Reports: []store.FeedbackReport{}, Details: store.FeedbackReportDetails{Links: map[string][]store.FeedbackReportLink{},
		Notes: map[string][]store.FeedbackReportNote{}, Tasks: map[string]store.FeedbackReportTaskRef{}, Crates: map[string]store.FeedbackReportCrateRef{}}}
	err = func() error {
		defer rows.Close()
		for rows.Next() {
			var total int64
			var links, notes, fixID, fixTitle, fixCrateID, fixCrateName sql.NullString
			report, err := decodeFeedbackReportRow(rows, &total, &links, &notes, &fixID, &fixTitle, &fixCrateID, &fixCrateName)
			if err != nil {
				return err
			}
			page.Total = int(total)
			if report.ID == "" {
				continue
			}
			page.Reports = append(page.Reports, report)
			page.Details.Links[report.ID], page.Details.Notes[report.ID] = []store.FeedbackReportLink{}, []store.FeedbackReportNote{}
			if err := addFeedbackReportViewLinks(&page.Details, report.ID, links); err != nil {
				return err
			}
			if err := addFeedbackReportViewNotes(&page.Details, report.ID, notes); err != nil {
				return err
			}
			if fixID.Valid && fixTitle.Valid {
				ref := store.FeedbackReportTaskRef{ID: fixID.String, Title: fixTitle.String}
				if fixCrateID.Valid && fixCrateName.Valid {
					ref.CrateID, ref.CrateName = fixCrateID.String, fixCrateName.String
					page.Details.Crates[ref.CrateID] = store.FeedbackReportCrateRef{ID: ref.CrateID, Name: ref.CrateName}
				}
				page.Details.Tasks[ref.ID] = ref
			}
		}
		return rows.Err()
	}()
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	return page, nil
}

// ListFeedbackReports implements store.ReportReadStore with the one view
// statement: the filters, the page's records, their links and crate
// membership, the fix task, and the total come from one committed view.
func (s *Store) ListFeedbackReports(ctx context.Context, query store.FeedbackReportQuery) (store.FeedbackReportPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackReports")
	query, err := store.NormalizeFeedbackReportQuery(query)
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	where, args := feedbackReportFilter(ws, query)
	return s.readFeedbackReportView(ctx, ws, where, args, query.Limit, query.Offset)
}

// ReadFeedbackReports implements store.ReportReadStore with the one view
// statement.
func (s *Store) ReadFeedbackReports(ctx context.Context, reportIDs []string) ([]store.FeedbackReport, store.FeedbackReportDetails, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackReports")
	empty := store.FeedbackReportDetails{Links: map[string][]store.FeedbackReportLink{}, Notes: map[string][]store.FeedbackReportNote{},
		Tasks: map[string]store.FeedbackReportTaskRef{}, Crates: map[string]store.FeedbackReportCrateRef{}}
	if len(reportIDs) == 0 {
		return []store.FeedbackReport{}, empty, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, store.FeedbackReportDetails{}, err
	}
	args := []any{ws}
	for _, id := range reportIDs {
		args = append(args, id)
	}
	page, err := s.readFeedbackReportView(ctx, ws, "r.workspace_id=? AND r.id IN ("+inList(len(reportIDs))+")", args, len(reportIDs), 0)
	if err != nil {
		return nil, store.FeedbackReportDetails{}, err
	}
	byID := make(map[string]store.FeedbackReport, len(page.Reports))
	for _, report := range page.Reports {
		byID[report.ID] = report
	}
	reports := []store.FeedbackReport{}
	for _, id := range reportIDs {
		if report, ok := byID[id]; ok {
			reports = append(reports, report)
			delete(byID, id)
		}
	}
	return reports, page.Details, nil
}

// ReadFeedbackReport implements store.ReportReadStore with the one view
// statement.
func (s *Store) ReadFeedbackReport(ctx context.Context, reportID string) (store.FeedbackReport, store.FeedbackReportDetails, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackReport")
	reports, details, err := s.ReadFeedbackReports(ctx, []string{reportID})
	if err != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, err
	}
	if len(reports) != 1 {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, fmt.Errorf("%w: feedback report %s", store.ErrNotFound, reportID)
	}
	return reports[0], details, nil
}

// GetFeedbackReport implements store.ReportReadStore.
func (s *Store) GetFeedbackReport(ctx context.Context, reportID string) (store.FeedbackReport, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetFeedbackReport")
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackReport{}, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=? AND id=?`, ws, reportID)
	if err != nil {
		return store.FeedbackReport{}, err
	}
	reports, err := collectFeedbackReports(rows)
	if err != nil {
		return store.FeedbackReport{}, err
	}
	if len(reports) != 1 {
		return store.FeedbackReport{}, fmt.Errorf("%w: feedback report %s", store.ErrNotFound, reportID)
	}
	return reports[0], nil
}

// inList renders count placeholders for an IN list.
func inList(count int) string {
	return "?" + strings.Repeat(",?", count-1)
}

// CountCrateOpenFeedbackReports implements store.ReportReadStore with the
// crate filter of the list, so the count and the list agree on what belongs
// to a crate (component-feedback-reports invariants 21 and 23).
func (s *Store) CountCrateOpenFeedbackReports(ctx context.Context, crateID string) (map[string]int, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CountCrateOpenFeedbackReports")
	counts := map[string]int{}
	if crateID == "" {
		return counts, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT r.environment_id,COUNT(*) FROM feedback_reports r WHERE r.workspace_id=? AND r.state='open' AND r.id IN (`+
		feedbackServedLinkReports("(lc.id=? OR tc.id=?)")+`) GROUP BY r.environment_id`, ws, ws, crateID, crateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var environmentID string
		var count int
		if err := rows.Scan(&environmentID, &count); err != nil {
			return nil, err
		}
		counts[environmentID] = count
	}
	return counts, rows.Err()
}

// RecordFeedbackReportAct implements store.ReportReadStore under the
// workspace environment claim, the lock that report entries and the linking
// work take, so an act, a report entry, and the end of linking run one at a
// time (component-feedback-reports invariants 6, 15, and 17;
// component-persistence invariant 8).
func (s *Store) RecordFeedbackReportAct(ctx context.Context, act store.FeedbackReportAct) (store.FeedbackReportActResult, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackReportAct")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return store.FeedbackReportActResult{}, err
	}
	act, err = store.PrepareFeedbackReportAct(ctx, s, act)
	if err != nil {
		return store.FeedbackReportActResult{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackReportActResult{}, err
	}
	result := store.FeedbackReportActResult{}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		result = store.FeedbackReportActResult{}
		claim, report, err := lockFeedbackReport(ctx, tx, ws, act.ReportID)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, feedbackReportLinkSQL, ws, report.ID)
		if err != nil {
			return err
		}
		stored, err := collectFeedbackReportLinks(rows)
		if err != nil {
			return err
		}
		targets := store.FeedbackReportActTargets{Tasks: map[string]store.FeedbackReportActTask{}, Crates: map[string]bool{}}
		if ids := act.TaskIDs(); len(ids) > 0 {
			args := []any{ws}
			for _, id := range ids {
				args = append(args, id)
			}
			taskRows, err := tx.QueryContext(ctx, `SELECT id,state FROM tasks WHERE workspace_id=? AND id IN (`+inList(len(ids))+`)`, args...)
			if err != nil {
				return err
			}
			err = func() error {
				defer taskRows.Close()
				for taskRows.Next() {
					var id, state string
					if err := taskRows.Scan(&id, &state); err != nil {
						return err
					}
					targets.Tasks[id] = store.FeedbackReportActTask{Exists: true, Merged: core.TaskState(state) == core.TaskMerged}
				}
				return taskRows.Err()
			}()
			if err != nil {
				return err
			}
		}
		if act.CrateID != "" {
			var held int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM crates WHERE workspace_id=? AND id=?`, ws, act.CrateID).Scan(&held)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				targets.Crates[act.CrateID] = false
			case err != nil:
				return err
			default:
				targets.Crates[act.CrateID] = true
			}
		}
		write, err := store.PlanFeedbackReportAct(report, stored, act, targets, actor)
		if err != nil || !write.Changed {
			return err
		}
		for _, link := range write.NewLinks {
			if _, err := tx.ExecContext(ctx, `INSERT INTO feedback_report_links (workspace_id,id,report_id,task_id,crate_id,state,evidence,created_at,created_by,confirmed_by,confirmed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				ws, link.ID, link.ReportID, optionalText(link.TaskID), optionalText(link.CrateID), link.State, store.FeedbackReportEvidenceJSON(link.Evidence),
				link.CreatedAt, link.CreatedBy, optionalText(link.ConfirmedBy), link.ConfirmedAt); err != nil {
				return err
			}
		}
		for _, link := range write.UpdatedLinks {
			var confirmedBy, removedBy sql.NullString
			if link.ConfirmedAt != nil {
				confirmedBy = sql.NullString{String: link.ConfirmedBy, Valid: true}
			}
			if link.RemovedAt != nil {
				removedBy = sql.NullString{String: link.RemovedBy, Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE feedback_report_links SET state=?,confirmed_by=?,confirmed_at=?,removed_by=?,removed_at=? WHERE workspace_id=? AND id=?`,
				link.State, confirmedBy, link.ConfirmedAt, removedBy, link.RemovedAt, ws, link.ID); err != nil {
				return err
			}
		}
		r := write.Report
		var closedBy, closeReason, reopenSource, reopenLabel, reopenActor, fixTaskID, fixTaskSetBy sql.NullString
		if r.ClosedAt != nil {
			closedBy, closeReason = sql.NullString{String: r.ClosedBy, Valid: true}, sql.NullString{String: r.CloseReason, Valid: true}
		}
		if r.ReopenedAt != nil && r.ReopenOrigin != nil {
			reopenSource = sql.NullString{String: r.ReopenOrigin.Source, Valid: true}
			reopenLabel = sql.NullString{String: r.ReopenOrigin.Label, Valid: true}
			reopenActor = sql.NullString{String: r.ReopenOrigin.Actor, Valid: true}
		}
		if r.FixTaskSetAt != nil {
			fixTaskID, fixTaskSetBy = sql.NullString{String: r.FixTaskID, Valid: true}, sql.NullString{String: r.FixTaskSetBy, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_reports SET state=?,in_inbox=?,closed_at=?,closed_by=?,close_reason=?,
reopened_at=?,reopen_source=?,reopen_source_label=?,reopen_actor_id=?,fix_task_id=?,fix_task_set_by=?,fix_task_set_at=? WHERE workspace_id=? AND id=?`,
			r.State, r.InInbox, r.ClosedAt, closedBy, closeReason, r.ReopenedAt, reopenSource, reopenLabel, reopenActor, fixTaskID, fixTaskSetBy, r.FixTaskSetAt, ws, r.ID); err != nil {
			return err
		}
		if err := insertWorkspaceEvent(ctx, tx, write.Event); err != nil {
			return err
		}
		if err := claim.touchEnvironment(ctx, tx, report.EnvironmentID); err != nil {
			return err
		}
		result.Changed = true
		return nil
	})
	if err != nil {
		return store.FeedbackReportActResult{}, err
	}
	return result, nil
}

// optionalText is NULL for an empty value.
func optionalText(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}
