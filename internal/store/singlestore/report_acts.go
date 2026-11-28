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

// ListFeedbackReports implements store.ReportReadStore. One statement
// answers the IDs of the page and the count of every selected row, also when
// the page is empty, so the page and the total agree. The records and their
// details are then read by ID.
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
	statement := `SELECT p.id, t.total FROM (SELECT COUNT(*) AS total FROM feedback_reports r WHERE ` + where + `) t
LEFT JOIN (SELECT r.id, r.last_seen FROM feedback_reports r WHERE ` + where + ` ORDER BY r.last_seen DESC, r.id LIMIT ? OFFSET ?) p ON 1=1
ORDER BY p.last_seen DESC, p.id`
	rows, err := documentRows(ctx, s.db, statement, append(append(append([]any{}, args...), args...), query.Limit, query.Offset)...)
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	page := store.FeedbackReportPage{Reports: []store.FeedbackReport{}}
	ids := []string{}
	err = func() error {
		defer rows.Close()
		for rows.Next() {
			var id sql.NullString
			var total int64
			if err := rows.Scan(&id, &total); err != nil {
				return err
			}
			page.Total = int(total)
			if id.Valid {
				ids = append(ids, id.String)
			}
		}
		return rows.Err()
	}()
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	if len(ids) > 0 {
		recordArgs := []any{ws}
		for _, id := range ids {
			recordArgs = append(recordArgs, id)
		}
		recordRows, err := documentRows(ctx, s.db, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=? AND id IN (`+inList(len(ids))+`)`, recordArgs...)
		if err != nil {
			return store.FeedbackReportPage{}, err
		}
		records, err := collectFeedbackReports(recordRows)
		if err != nil {
			return store.FeedbackReportPage{}, err
		}
		byID := make(map[string]store.FeedbackReport, len(records))
		for _, record := range records {
			byID[record.ID] = record
		}
		for _, id := range ids {
			record, ok := byID[id]
			if !ok {
				return store.FeedbackReportPage{}, fmt.Errorf("feedback report %s of the page was not read back", id)
			}
			page.Reports = append(page.Reports, record)
		}
	}
	if page.Details, err = s.ReadFeedbackReportDetails(ctx, page.Reports); err != nil {
		return store.FeedbackReportPage{}, err
	}
	return page, nil
}

// ReadFeedbackReport implements store.ReportReadStore.
func (s *Store) ReadFeedbackReport(ctx context.Context, reportID string) (store.FeedbackReport, store.FeedbackReportDetails, error) {
	report, err := s.GetFeedbackReport(ctx, reportID)
	if err != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, err
	}
	details, err := s.ReadFeedbackReportDetails(ctx, []store.FeedbackReport{report})
	if err != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, err
	}
	return report, details, nil
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

// ReadFeedbackReportDetails implements store.ReportReadStore.
func (s *Store) ReadFeedbackReportDetails(ctx context.Context, reports []store.FeedbackReport) (store.FeedbackReportDetails, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackReportDetails")
	details := store.FeedbackReportDetails{Links: map[string][]store.FeedbackReportLink{}, Notes: map[string][]store.FeedbackReportNote{},
		Tasks: map[string]store.FeedbackReportTaskRef{}, Crates: map[string]store.FeedbackReportCrateRef{}}
	if len(reports) == 0 {
		return details, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackReportDetails{}, err
	}
	args := []any{ws}
	taskIDs, crateIDs := []string{}, []string{}
	for _, report := range reports {
		args = append(args, report.ID)
		details.Links[report.ID], details.Notes[report.ID] = []store.FeedbackReportLink{}, []store.FeedbackReportNote{}
		if report.FixTaskID != "" {
			taskIDs = append(taskIDs, report.FixTaskID)
		}
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,report_id,task_id,crate_id,state,evidence,created_at,created_by,confirmed_by,confirmed_at,removed_by,removed_at
FROM feedback_report_links WHERE workspace_id=? AND report_id IN (`+inList(len(reports))+`) ORDER BY report_id, id`, args...)
	if err != nil {
		return store.FeedbackReportDetails{}, err
	}
	links, err := collectFeedbackReportLinks(rows)
	if err != nil {
		return store.FeedbackReportDetails{}, err
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
	noteRows, err := documentRows(ctx, s.db, `SELECT report_id,detail FROM feedback_report_notes WHERE workspace_id=? AND report_id IN (`+inList(len(reports))+`) ORDER BY report_id, id`, args...)
	if err != nil {
		return store.FeedbackReportDetails{}, err
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
		return store.FeedbackReportDetails{}, err
	}
	if details.Tasks, details.Crates, err = s.feedbackReportRefs(ctx, ws, taskIDs, crateIDs); err != nil {
		return store.FeedbackReportDetails{}, err
	}
	return details, nil
}

// feedbackReportRefs reads the tasks and crates the given IDs name, with
// the crate each task is a member of.
func (s *Store) feedbackReportRefs(ctx context.Context, ws string, taskIDs, crateIDs []string) (map[string]store.FeedbackReportTaskRef, map[string]store.FeedbackReportCrateRef, error) {
	tasks := map[string]store.FeedbackReportTaskRef{}
	crates := map[string]store.FeedbackReportCrateRef{}
	if len(taskIDs) > 0 {
		args := []any{ws}
		for _, id := range taskIDs {
			args = append(args, id)
		}
		rows, err := documentRows(ctx, s.db, `SELECT t.id,t.title,c.id,c.name FROM tasks t LEFT JOIN crates c ON c.workspace_id=t.workspace_id AND c.id=t.crate_id
WHERE t.workspace_id=? AND t.id IN (`+inList(len(taskIDs))+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var ref store.FeedbackReportTaskRef
				var crateID, crateName sql.NullString
				if err := rows.Scan(&ref.ID, &ref.Title, &crateID, &crateName); err != nil {
					return err
				}
				if crateID.Valid && crateName.Valid {
					ref.CrateID, ref.CrateName = crateID.String, crateName.String
					crates[ref.CrateID] = store.FeedbackReportCrateRef{ID: ref.CrateID, Name: ref.CrateName}
				}
				tasks[ref.ID] = ref
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, nil, err
		}
	}
	if len(crateIDs) > 0 {
		args := []any{ws}
		for _, id := range crateIDs {
			args = append(args, id)
		}
		rows, err := documentRows(ctx, s.db, `SELECT id,name FROM crates WHERE workspace_id=? AND id IN (`+inList(len(crateIDs))+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var ref store.FeedbackReportCrateRef
				if err := rows.Scan(&ref.ID, &ref.Name); err != nil {
					return err
				}
				crates[ref.ID] = ref
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, nil, err
		}
	}
	return tasks, crates, nil
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
