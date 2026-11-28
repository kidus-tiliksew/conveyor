package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
// placeholder is the workspace ID. It runs in the database over the link,
// task, and crate keys and transfers no row.
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

func emptyFeedbackReportDetails() store.FeedbackReportDetails {
	return store.FeedbackReportDetails{Links: map[string][]store.FeedbackReportLink{}, Notes: map[string][]store.FeedbackReportNote{},
		Tasks: map[string]store.FeedbackReportTaskRef{}, Crates: map[string]store.FeedbackReportCrateRef{}}
}

// feedbackReportViewAttempts bounds the reads of one view when a linked
// task moves between crates while the view is read.
const feedbackReportViewAttempts = 3

// errFeedbackReportViewMoved reports that the membership the page's links
// were read with no longer selects a page report, so the view is read again.
var errFeedbackReportViewMoved = errors.New("crate membership changed during the report read")

// readFeedbackReportView reads one consistent view of a page of reports,
// with the work bounded by the page. The transaction takes the workspace
// environment claim, the lock every report write takes (report entries, the
// linking work, and the acts), and waits until the last write under it is
// visible, so no report write runs while the view is read: SingleStore's
// read committed isolation keeps no view across statements, or even across
// the tables of one statement. Under the claim it runs:
//
//   - when refuseAbove is positive, the count, an aggregate in the database
//     that transfers one row, and stops there when the selection passes it;
//   - one statement of the count and the page's records, with the filters as
//     semi-joins in the database, transferring the page and one count;
//   - the links and the notes of the page's reports only;
//   - the tasks and crates those links and fix tasks name only.
//
// A task's crate changes under the task's own lock, not under the claim. So
// when validate is set, each page report is checked against the filters with
// the links and membership read for the page, and the page against the
// total. A page report that no longer matches, or a page that ends before
// its limit away from the total, makes the whole view read again, at most
// feedbackReportViewAttempts times, and then store.ErrRetryable
// (component-feedback-reports invariants 18, 21, and 24;
// component-persistence invariant 8).
func (s *Store) readFeedbackReportView(ctx context.Context, ws, where string, whereArgs []any, limit, offset, refuseAbove int, validate *store.FeedbackReportQuery) (store.FeedbackReportPage, error) {
	for range feedbackReportViewAttempts {
		page, err := s.readFeedbackReportViewOnce(ctx, ws, where, whereArgs, limit, offset, refuseAbove, validate)
		if !errors.Is(err, errFeedbackReportViewMoved) {
			return page, err
		}
	}
	return store.FeedbackReportPage{}, fmt.Errorf("%w: %v", store.ErrRetryable, errFeedbackReportViewMoved)
}

func (s *Store) readFeedbackReportViewOnce(ctx context.Context, ws, where string, whereArgs []any, limit, offset, refuseAbove int, validate *store.FeedbackReportQuery) (store.FeedbackReportPage, error) {
	page := store.FeedbackReportPage{}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		page = store.FeedbackReportPage{Reports: []store.FeedbackReport{}, Details: emptyFeedbackReportDetails()}
		if _, err := claimEnvironments(ctx, tx, ws); err != nil {
			return err
		}
		if refuseAbove > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_reports r WHERE `+where, whereArgs...).Scan(&page.Total); err != nil {
				return err
			}
			if page.Total > refuseAbove {
				return nil
			}
		}
		statement := `SELECT ` + feedbackReportPageColumns() + `, t.total FROM (SELECT COUNT(*) AS total FROM feedback_reports r WHERE ` + where + `) t
LEFT JOIN (SELECT ` + prefixFeedbackReportColumns("r") + ` FROM feedback_reports r WHERE ` + where + ` ORDER BY r.last_seen DESC, r.id LIMIT ? OFFSET ?) p ON 1=1
ORDER BY p.last_seen DESC, p.id`
		rows, err := tx.QueryContext(ctx, statement, append(append(append([]any{}, whereArgs...), whereArgs...), limit, offset)...)
		if err != nil {
			return err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var total int64
				report, err := decodeFeedbackReportRow(rows, &total)
				if err != nil {
					return err
				}
				page.Total = int(total)
				if report.ID != "" {
					page.Reports = append(page.Reports, report)
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		// The count and the page are two parts of one statement, which need
		// not see one membership either: a page that ends before its limit
		// must end at the total, and a page that holds reports must end at
		// or before it.
		served := offset + len(page.Reports)
		if validate != nil && ((len(page.Reports) > 0 && page.Total < served) || (len(page.Reports) < limit && page.Total > served)) {
			return errFeedbackReportViewMoved
		}
		if len(page.Reports) == 0 {
			return nil
		}
		if page.Details, err = readFeedbackReportPageDetails(ctx, tx, ws, page.Reports); err != nil {
			return err
		}
		if validate != nil {
			for _, report := range page.Reports {
				if !store.FeedbackReportMatches(*validate, page.Details.Links[report.ID], page.Details.Tasks, page.Details.Crates) {
					return errFeedbackReportViewMoved
				}
			}
		}
		return nil
	})
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	return page, nil
}

// readFeedbackReportPageDetails reads, in the view's transaction, the links
// and notes of the given reports and the tasks and crates they name, and
// nothing else.
func readFeedbackReportPageDetails(ctx context.Context, tx *sql.Tx, ws string, reports []store.FeedbackReport) (store.FeedbackReportDetails, error) {
	details := emptyFeedbackReportDetails()
	ids := []any{ws}
	taskIDs, crateIDs := []string{}, []string{}
	for _, report := range reports {
		ids = append(ids, report.ID)
		details.Links[report.ID], details.Notes[report.ID] = []store.FeedbackReportLink{}, []store.FeedbackReportNote{}
		if report.FixTaskID != "" {
			taskIDs = append(taskIDs, report.FixTaskID)
		}
	}
	marks := inList(len(reports))
	linkRows, err := tx.QueryContext(ctx, `SELECT id,report_id,task_id,crate_id,state,evidence,created_at,created_by,confirmed_by,confirmed_at,removed_by,removed_at
FROM feedback_report_links WHERE workspace_id=? AND report_id IN (`+marks+`) ORDER BY report_id, id`, ids...)
	if err != nil {
		return details, err
	}
	links, err := collectFeedbackReportLinks(linkRows)
	if err != nil {
		return details, err
	}
	for _, link := range links {
		details.Links[link.ReportID] = append(details.Links[link.ReportID], link)
		if link.TaskID != "" {
			taskIDs = append(taskIDs, link.TaskID)
		}
		if link.CrateID != "" {
			crateIDs = append(crateIDs, link.CrateID)
		}
	}
	noteRows, err := tx.QueryContext(ctx, `SELECT report_id,detail FROM feedback_report_notes WHERE workspace_id=? AND report_id IN (`+marks+`) ORDER BY report_id, id`, ids...)
	if err != nil {
		return details, err
	}
	err = func() error {
		defer noteRows.Close()
		for noteRows.Next() {
			var reportID, detail string
			if err := noteRows.Scan(&reportID, &detail); err != nil {
				return err
			}
			note, err := store.ParseFeedbackReportNote([]byte(detail))
			if err != nil {
				return err
			}
			details.Notes[reportID] = append(details.Notes[reportID], note)
		}
		return noteRows.Err()
	}()
	if err != nil {
		return details, err
	}
	if len(taskIDs) > 0 {
		args := []any{ws}
		for _, id := range taskIDs {
			args = append(args, id)
		}
		taskRows, err := tx.QueryContext(ctx, `SELECT t.id,t.title,c.id,c.name FROM tasks t LEFT JOIN crates c ON c.workspace_id=t.workspace_id AND c.id=t.crate_id
WHERE t.workspace_id=? AND t.id IN (`+inList(len(taskIDs))+`)`, args...)
		if err != nil {
			return details, err
		}
		err = func() error {
			defer taskRows.Close()
			for taskRows.Next() {
				var ref store.FeedbackReportTaskRef
				var crateID, crateName sql.NullString
				if err := taskRows.Scan(&ref.ID, &ref.Title, &crateID, &crateName); err != nil {
					return err
				}
				if crateID.Valid && crateName.Valid {
					ref.CrateID, ref.CrateName = crateID.String, crateName.String
					details.Crates[ref.CrateID] = store.FeedbackReportCrateRef{ID: ref.CrateID, Name: ref.CrateName}
				}
				details.Tasks[ref.ID] = ref
			}
			return taskRows.Err()
		}()
		if err != nil {
			return details, err
		}
	}
	if len(crateIDs) > 0 {
		args := []any{ws}
		for _, id := range crateIDs {
			args = append(args, id)
		}
		crateRows, err := tx.QueryContext(ctx, `SELECT id,name FROM crates WHERE workspace_id=? AND id IN (`+inList(len(crateIDs))+`)`, args...)
		if err != nil {
			return details, err
		}
		err = func() error {
			defer crateRows.Close()
			for crateRows.Next() {
				var ref store.FeedbackReportCrateRef
				if err := crateRows.Scan(&ref.ID, &ref.Name); err != nil {
					return err
				}
				details.Crates[ref.ID] = ref
			}
			return crateRows.Err()
		}()
		if err != nil {
			return details, err
		}
	}
	return details, nil
}

// ListFeedbackReports implements store.ReportReadStore with one consistent
// view whose work the page bounds (readFeedbackReportView).
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
	return s.readFeedbackReportView(ctx, ws, where, args, query.Limit, query.Offset, query.RefuseAbove, &query)
}

// ReadFeedbackReports implements store.ReportReadStore with one consistent
// view of the given reports (readFeedbackReportView).
func (s *Store) ReadFeedbackReports(ctx context.Context, reportIDs []string) ([]store.FeedbackReport, store.FeedbackReportDetails, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackReports")
	if len(reportIDs) == 0 {
		return []store.FeedbackReport{}, emptyFeedbackReportDetails(), nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, store.FeedbackReportDetails{}, err
	}
	args := []any{ws}
	for _, id := range reportIDs {
		args = append(args, id)
	}
	page, err := s.readFeedbackReportView(ctx, ws, "r.workspace_id=? AND r.id IN ("+inList(len(reportIDs))+")", args, len(reportIDs), 0, 0, nil)
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

// ReadFeedbackReport implements store.ReportReadStore with one consistent
// view.
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
// crate filter of the list, one aggregate in the database that transfers one
// row for each environment, under the workspace environment claim, so the
// count and the list agree on what belongs to a crate
// (component-feedback-reports invariants 21 and 23).
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
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		counts = map[string]int{}
		if _, err := claimEnvironments(ctx, tx, ws); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT r.environment_id,COUNT(*) FROM feedback_reports r WHERE r.workspace_id=? AND r.state='open' AND r.id IN (`+
			feedbackServedLinkReports("(lc.id=? OR tc.id=?)")+`) GROUP BY r.environment_id`, ws, ws, crateID, crateID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var environmentID string
			var count int
			if err := rows.Scan(&environmentID, &count); err != nil {
				return err
			}
			counts[environmentID] = count
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
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
