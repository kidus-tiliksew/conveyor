package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.ReportReadStore = (*Store)(nil)

// feedbackServedLinkReports is a subquery of the report IDs that have a
// served link meeting cond: a possible or confirmed link to a task or a crate
// the workspace holds. tc is the crate a linked task is a member of at the
// read, and lc the crate a link names. It states the rule of
// store.FeedbackReportServed and store.FeedbackReportInCrate
// (component-feedback-reports invariant 21). workspace is the placeholder of
// the workspace ID.
func feedbackServedLinkReports(workspace, cond string) string {
	return `SELECT l.report_id FROM feedback_report_links l
LEFT JOIN tasks t ON t.workspace_id=l.workspace_id AND t.id=l.task_id
LEFT JOIN crates lc ON lc.workspace_id=l.workspace_id AND lc.id=l.crate_id
LEFT JOIN crates tc ON tc.workspace_id=t.workspace_id AND tc.id=t.crate_id
WHERE l.workspace_id=` + workspace + ` AND l.state IN ('possible', 'confirmed')
AND ((l.task_id IS NOT NULL AND l.crate_id IS NULL AND t.id IS NOT NULL) OR (l.crate_id IS NOT NULL AND l.task_id IS NULL AND lc.id IS NOT NULL))
AND ` + cond
}

// feedbackReportFilter renders the WHERE clause of a list and its
// arguments. Every filter is a predicate of the one statement, so it runs
// before the page and the count, and IN keeps a report that matches twice to
// one row.
func feedbackReportFilter(workspaceID string, query store.FeedbackReportQuery) (string, []any) {
	args := []any{workspaceID, query.State}
	clauses := []string{"r.workspace_id=$1", "r.state=$2"}
	next := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}
	if query.EnvironmentID != "" {
		clauses = append(clauses, "r.environment_id="+next(query.EnvironmentID))
	}
	if query.TaskID != "" {
		clauses = append(clauses, "r.id IN ("+feedbackServedLinkReports("$1", "l.task_id="+next(query.TaskID))+")")
	}
	if query.CrateID != "" {
		crate := next(query.CrateID)
		clauses = append(clauses, "r.id IN ("+feedbackServedLinkReports("$1", "(lc.id="+crate+" OR tc.id="+crate+")")+")")
	}
	if query.NoCrate {
		clauses = append(clauses, "r.id NOT IN ("+feedbackServedLinkReports("$1", "(lc.id IS NOT NULL OR tc.id IS NOT NULL)")+")")
	}
	return strings.Join(clauses, " AND "), args
}

// ListFeedbackReports implements store.ReportReadStore. The page, the
// count, and the details are read in one REPEATABLE READ snapshot, so they
// agree: the count is a window over every selected row before the page is
// cut, and a page past the end reads the count alone in the same snapshot.
func (s *Store) ListFeedbackReports(ctx context.Context, query store.FeedbackReportQuery) (store.FeedbackReportPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackReports")
	query, err := store.NormalizeFeedbackReportQuery(query)
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	workspaceID := workspace(ctx)
	where, args := feedbackReportFilter(workspaceID, query)
	limit, offset := len(args)+1, len(args)+2
	sql := `SELECT ` + prefixFeedbackReportColumns("r") + `, count(*) OVER () FROM feedback_reports r WHERE ` + where +
		` ORDER BY r.last_seen DESC, r.id LIMIT $` + strconv.Itoa(limit) + ` OFFSET $` + strconv.Itoa(offset)
	tx, err := s.beginWith(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, sql, append(append([]any{}, args...), query.Limit, query.Offset)...)
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	reports, totals, err := scanFeedbackReportsWithTotal(rows)
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	page := store.FeedbackReportPage{Reports: reports}
	if len(totals) > 0 {
		page.Total = totals[0]
	} else if err := tx.QueryRow(ctx, `SELECT count(*) FROM feedback_reports r WHERE `+where, args...).Scan(&page.Total); err != nil {
		return store.FeedbackReportPage{}, err
	}
	if page.Details, err = readFeedbackReportDetailsTx(ctx, tx, workspaceID, reports); err != nil {
		return store.FeedbackReportPage{}, err
	}
	return page, nil
}

// ReadFeedbackReport implements store.ReportReadStore in one REPEATABLE
// READ snapshot.
func (s *Store) ReadFeedbackReport(ctx context.Context, reportID string) (store.FeedbackReport, store.FeedbackReportDetails, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackReport")
	workspaceID := workspace(ctx)
	tx, err := s.beginWith(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reports, err := queryFeedbackReports(ctx, tx, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=$1 AND id=$2`, workspaceID, reportID)
	if err != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, err
	}
	if len(reports) != 1 {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, fmt.Errorf("%w: feedback report %s", store.ErrNotFound, reportID)
	}
	details, err := readFeedbackReportDetailsTx(ctx, tx, workspaceID, reports)
	if err != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, err
	}
	return reports[0], details, nil
}

// prefixFeedbackReportColumns qualifies every report column with a table
// alias.
func prefixFeedbackReportColumns(alias string) string {
	columns := strings.Split(strings.ReplaceAll(feedbackReportColumns, "\n", " "), ",")
	for i, column := range columns {
		columns[i] = alias + "." + strings.TrimSpace(column)
	}
	return strings.Join(columns, ", ")
}

// scanFeedbackReportsWithTotal reads report rows that end with the window
// count. It decodes each report as queryFeedbackReports does.
func scanFeedbackReportsWithTotal(rows pgx.Rows) ([]store.FeedbackReport, []int, error) {
	defer rows.Close()
	reports := []store.FeedbackReport{}
	totals := []int{}
	for rows.Next() {
		var total int64
		report, err := decodeFeedbackReportRow(rows, &total)
		if err != nil {
			return nil, nil, err
		}
		reports = append(reports, report)
		totals = append(totals, int(total))
	}
	return reports, totals, rows.Err()
}

// GetFeedbackReport implements store.ReportReadStore.
func (s *Store) GetFeedbackReport(ctx context.Context, reportID string) (store.FeedbackReport, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetFeedbackReport")
	reports, err := queryFeedbackReports(ctx, s.boundary, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=$1 AND id=$2`, workspace(ctx), reportID)
	if err != nil {
		return store.FeedbackReport{}, err
	}
	if len(reports) != 1 {
		return store.FeedbackReport{}, fmt.Errorf("%w: feedback report %s", store.ErrNotFound, reportID)
	}
	return reports[0], nil
}

// ReadFeedbackReports implements store.ReportReadStore: the records and
// their details in one REPEATABLE READ snapshot.
func (s *Store) ReadFeedbackReports(ctx context.Context, reportIDs []string) ([]store.FeedbackReport, store.FeedbackReportDetails, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackReports")
	workspaceID := workspace(ctx)
	tx, err := s.beginWith(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, store.FeedbackReportDetails{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := queryFeedbackReports(ctx, tx, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=$1 AND id=ANY($2::text[])`, workspaceID, reportIDs)
	if err != nil {
		return nil, store.FeedbackReportDetails{}, err
	}
	byID := make(map[string]store.FeedbackReport, len(stored))
	for _, report := range stored {
		byID[report.ID] = report
	}
	reports := []store.FeedbackReport{}
	for _, id := range reportIDs {
		if report, ok := byID[id]; ok {
			reports = append(reports, report)
			delete(byID, id)
		}
	}
	details, err := readFeedbackReportDetailsTx(ctx, tx, workspaceID, reports)
	if err != nil {
		return nil, store.FeedbackReportDetails{}, err
	}
	return reports, details, nil
}

// readFeedbackReportDetailsTx reads the links, notes, tasks, and crates of
// reports in the caller's transaction.
func readFeedbackReportDetailsTx(ctx context.Context, tx pgx.Tx, workspaceID string, reports []store.FeedbackReport) (store.FeedbackReportDetails, error) {
	details := store.FeedbackReportDetails{Links: map[string][]store.FeedbackReportLink{}, Notes: map[string][]store.FeedbackReportNote{},
		Tasks: map[string]store.FeedbackReportTaskRef{}, Crates: map[string]store.FeedbackReportCrateRef{}}
	if len(reports) == 0 {
		return details, nil
	}
	ids := make([]string, len(reports))
	taskIDs, crateIDs := []string{}, []string{}
	for i, report := range reports {
		ids[i] = report.ID
		details.Links[report.ID], details.Notes[report.ID] = []store.FeedbackReportLink{}, []store.FeedbackReportNote{}
		if report.FixTaskID != "" {
			taskIDs = append(taskIDs, report.FixTaskID)
		}
	}
	links, err := scanFeedbackReportLinkRows(ctx, tx, `SELECT id, report_id, task_id, crate_id, state, evidence::text, created_at, created_by, confirmed_by, confirmed_at, removed_by, removed_at
FROM feedback_report_links WHERE workspace_id=$1 AND report_id=ANY($2::text[]) ORDER BY report_id, id`, workspaceID, ids)
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
	rows, err := tx.Query(ctx, `SELECT report_id, detail::text FROM feedback_report_notes WHERE workspace_id=$1 AND report_id=ANY($2::text[]) ORDER BY report_id, id`, workspaceID, ids)
	if err != nil {
		return store.FeedbackReportDetails{}, err
	}
	for rows.Next() {
		var reportID, detail string
		if err := rows.Scan(&reportID, &detail); err != nil {
			rows.Close()
			return store.FeedbackReportDetails{}, err
		}
		note, err := store.ParseFeedbackReportNote([]byte(detail))
		if err != nil {
			rows.Close()
			return store.FeedbackReportDetails{}, err
		}
		details.Notes[reportID] = append(details.Notes[reportID], note)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return store.FeedbackReportDetails{}, err
	}
	if details.Tasks, details.Crates, err = queryFeedbackReportRefs(ctx, tx, workspaceID, taskIDs, crateIDs); err != nil {
		return store.FeedbackReportDetails{}, err
	}
	return details, nil
}

// queryFeedbackReportRefs reads the tasks and crates the given IDs name,
// with the crate each task is a member of.
func queryFeedbackReportRefs(ctx context.Context, q environmentQuerier, workspaceID string, taskIDs, crateIDs []string) (map[string]store.FeedbackReportTaskRef, map[string]store.FeedbackReportCrateRef, error) {
	tasks := map[string]store.FeedbackReportTaskRef{}
	crates := map[string]store.FeedbackReportCrateRef{}
	if len(taskIDs) > 0 {
		rows, err := q.Query(ctx, `SELECT t.id, t.title, c.id, c.name FROM tasks t LEFT JOIN crates c ON c.workspace_id=t.workspace_id AND c.id=t.crate_id
WHERE t.workspace_id=$1 AND t.id=ANY($2::text[])`, workspaceID, taskIDs)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var ref store.FeedbackReportTaskRef
			var crateID, crateName *string
			if err := rows.Scan(&ref.ID, &ref.Title, &crateID, &crateName); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if crateID != nil && crateName != nil {
				ref.CrateID, ref.CrateName = *crateID, *crateName
				crates[ref.CrateID] = store.FeedbackReportCrateRef{ID: ref.CrateID, Name: ref.CrateName}
			}
			tasks[ref.ID] = ref
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	if len(crateIDs) > 0 {
		rows, err := q.Query(ctx, `SELECT id, name FROM crates WHERE workspace_id=$1 AND id=ANY($2::text[])`, workspaceID, crateIDs)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var ref store.FeedbackReportCrateRef
			if err := rows.Scan(&ref.ID, &ref.Name); err != nil {
				rows.Close()
				return nil, nil, err
			}
			crates[ref.ID] = ref
		}
		rows.Close()
		if err := rows.Err(); err != nil {
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
	rows, err := s.boundary.Query(ctx, `SELECT r.environment_id, count(*) FROM feedback_reports r WHERE r.workspace_id=$1 AND r.state='open' AND r.id IN (`+
		feedbackServedLinkReports("$1", "(lc.id=$2 OR tc.id=$2)")+`) GROUP BY r.environment_id`, workspace(ctx), crateID)
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

// RecordFeedbackReportAct implements store.ReportReadStore. It takes the
// environment row lock that a report request takes first, then the report
// row lock that the linking work takes, so an act, a report entry of the
// environment, and the end of linking run one at a time, and no two writers
// wait on each other in a cycle (component-feedback-reports invariants 6, 15,
// and 17).
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
	workspaceID := workspace(ctx)
	result := store.FeedbackReportActResult{}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		result = store.FeedbackReportActResult{}
		var environmentID string
		err := tx.QueryRow(ctx, `SELECT environment_id FROM feedback_reports WHERE workspace_id=$1 AND id=$2`, workspaceID, act.ReportID).Scan(&environmentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: feedback report %s", store.ErrNotFound, act.ReportID)
		}
		if err != nil {
			return err
		}
		// A report keeps its environment, so the lock taken here is the one
		// its report entries take.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM environments WHERE workspace_id=$1 AND id=$2 FOR NO KEY UPDATE`, workspaceID, environmentID); err != nil {
			return err
		}
		report, err := lockFeedbackReport(ctx, tx, workspaceID, act.ReportID)
		if err != nil {
			return err
		}
		stored, err := queryFeedbackReportLinks(ctx, tx, workspaceID, report.ID)
		if err != nil {
			return err
		}
		targets := store.FeedbackReportActTargets{Tasks: map[string]store.FeedbackReportActTask{}, Crates: map[string]bool{}}
		if ids := act.TaskIDs(); len(ids) > 0 {
			rows, err := tx.Query(ctx, `SELECT id, state FROM tasks WHERE workspace_id=$1 AND id=ANY($2::text[])`, workspaceID, ids)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, state string
				if err := rows.Scan(&id, &state); err != nil {
					rows.Close()
					return err
				}
				targets.Tasks[id] = store.FeedbackReportActTask{Exists: true, Merged: core.TaskState(state) == core.TaskMerged}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		if act.CrateID != "" {
			var held bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM crates WHERE workspace_id=$1 AND id=$2)`, workspaceID, act.CrateID).Scan(&held); err != nil {
				return err
			}
			targets.Crates[act.CrateID] = held
		}
		write, err := store.PlanFeedbackReportAct(report, stored, act, targets, actor)
		if err != nil || !write.Changed {
			return err
		}
		for _, link := range write.NewLinks {
			if _, err := tx.Exec(ctx, `INSERT INTO feedback_report_links (workspace_id, id, report_id, task_id, crate_id, state, evidence, created_at, created_by, confirmed_by, confirmed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7::text::jsonb, $8, $9, $10, $11)`,
				workspaceID, link.ID, link.ReportID, feedbackOptionalText(link.TaskID), feedbackOptionalText(link.CrateID), link.State, store.FeedbackReportEvidenceJSON(link.Evidence),
				link.CreatedAt, link.CreatedBy, feedbackOptionalText(link.ConfirmedBy), link.ConfirmedAt); err != nil {
				return err
			}
		}
		for _, link := range write.UpdatedLinks {
			var confirmedBy, removedBy *string
			if link.ConfirmedAt != nil {
				confirmedBy = &link.ConfirmedBy
			}
			if link.RemovedAt != nil {
				removedBy = &link.RemovedBy
			}
			tag, err := tx.Exec(ctx, `UPDATE feedback_report_links SET state=$3, confirmed_by=$4, confirmed_at=$5, removed_by=$6, removed_at=$7 WHERE workspace_id=$1 AND id=$2`,
				workspaceID, link.ID, link.State, confirmedBy, link.ConfirmedAt, removedBy, link.RemovedAt)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("update link %s: %d rows changed", link.ID, tag.RowsAffected())
			}
		}
		r := write.Report
		var closedBy, closeReason, reopenSource, reopenLabel, reopenActor, fixTaskID, fixTaskSetBy *string
		if r.ClosedAt != nil {
			closedBy, closeReason = &r.ClosedBy, &r.CloseReason
		}
		if r.ReopenedAt != nil && r.ReopenOrigin != nil {
			reopenSource, reopenLabel, reopenActor = &r.ReopenOrigin.Source, &r.ReopenOrigin.Label, &r.ReopenOrigin.Actor
		}
		if r.FixTaskSetAt != nil {
			fixTaskID, fixTaskSetBy = &r.FixTaskID, &r.FixTaskSetBy
		}
		tag, err := tx.Exec(ctx, `UPDATE feedback_reports SET state=$3, in_inbox=$4, closed_at=$5, closed_by=$6, close_reason=$7,
reopened_at=$8, reopen_source=$9, reopen_source_label=$10, reopen_actor_id=$11, fix_task_id=$12, fix_task_set_by=$13, fix_task_set_at=$14
WHERE workspace_id=$1 AND id=$2`,
			workspaceID, r.ID, r.State, r.InInbox, r.ClosedAt, closedBy, closeReason, r.ReopenedAt, reopenSource, reopenLabel, reopenActor, fixTaskID, fixTaskSetBy, r.FixTaskSetAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("update feedback report %s: %d rows changed", r.ID, tag.RowsAffected())
		}
		if err := insertWorkspaceEvent(ctx, q, write.Event); err != nil {
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

// scanFeedbackReportLinkRows reads link rows in the column order of
// queryFeedbackReportLinks.
func scanFeedbackReportLinkRows(ctx context.Context, q environmentQuerier, sql string, args ...any) ([]store.FeedbackReportLink, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return collectPostgresFeedbackReportLinks(rows)
}

func feedbackOptionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
