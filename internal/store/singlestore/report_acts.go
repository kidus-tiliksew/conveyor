package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.ReportReadStore = (*Store)(nil)

// feedbackServedLink is a predicate on the report r: it has a served link
// meeting cond, a possible or confirmed link to a task or a crate the
// workspace holds. tc is the crate a linked task is a member of at the read,
// and lc the crate a link names. It states the rule of
// store.FeedbackReportServed and store.FeedbackReportInCrate
// (component-feedback-reports invariant 21). It is correlated on the report,
// so each candidate report probes its own links through
// feedback_report_links_report_idx and the predicate keeps no set of report
// IDs, and it transfers no row.
func feedbackServedLink(cond string) string {
	return `EXISTS (SELECT 1 FROM feedback_report_links l
LEFT JOIN tasks t ON t.workspace_id=l.workspace_id AND t.id=l.task_id
LEFT JOIN crates lc ON lc.workspace_id=l.workspace_id AND lc.id=l.crate_id
LEFT JOIN crates tc ON tc.workspace_id=t.workspace_id AND tc.id=t.crate_id
WHERE l.workspace_id=r.workspace_id AND l.report_id=r.id AND l.state IN ('possible','confirmed')
AND ((l.task_id IS NOT NULL AND l.crate_id IS NULL AND t.id IS NOT NULL) OR (l.crate_id IS NOT NULL AND l.task_id IS NULL AND lc.id IS NOT NULL))
AND ` + cond + `)`
}

// feedbackReportFilter renders the WHERE clause of a list and its arguments.
// Every filter is a predicate of each statement, so it runs before the page
// and the count, and EXISTS keeps a report that matches twice to one row.
func feedbackReportFilter(ws string, query store.FeedbackReportQuery) (string, []any) {
	clauses := []string{"r.workspace_id=?", "r.state=?"}
	args := []any{ws, query.State}
	if query.EnvironmentID != "" {
		clauses = append(clauses, "r.environment_id=?")
		args = append(args, query.EnvironmentID)
	}
	if query.TaskID != "" {
		clauses = append(clauses, feedbackServedLink("l.task_id=?"))
		args = append(args, query.TaskID)
	}
	if query.CrateID != "" {
		clauses = append(clauses, feedbackServedLink("(lc.id=? OR tc.id=?)"))
		args = append(args, query.CrateID, query.CrateID)
	}
	if query.NoCrate {
		clauses = append(clauses, "NOT "+feedbackServedLink("(lc.id IS NOT NULL OR tc.id IS NOT NULL)"))
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

// feedbackReportViewAttempts bounds the reads of one view when a task
// change commits while the view is read.
const feedbackReportViewAttempts = 3

// errFeedbackReportViewMoved reports that a task change committed while the
// view was read, so the view is read again.
var errFeedbackReportViewMoved = errors.New("a task changed during the report read")

// feedbackReportTaskPosition reads the workspace's task event position: the
// counter every transaction that commits a task event advances once, under
// its row lock, just before it commits (change_notices_positions.go). A
// task's crate, title, and existence change only in such a transaction. The
// counter row is sharded by workspace like the task, crate, and report
// tables, so it is on the same partition as everything the view reads.
func feedbackReportTaskPosition(ctx context.Context, tx *sql.Tx, ws string) (int64, error) {
	var position int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),0) FROM task_event_positions WHERE workspace_id=?`, ws).Scan(&position)
	return position, err
}

// readFeedbackReportView reads one answer for a page of reports, with the
// work bounded by the page, as component-feedback-reports v4 invariant 21
// states it. SingleStore's read committed isolation keeps no view across
// statements, or even across the scans of one statement, so:
//
//   - One state of the store gives each report's record, its served links
//     with their evidence, its notes, its fix task, and the crate membership
//     that decides the filters and the count. The workspace environment
//     claim is the lock every write of a report, a link, a note, and a fix
//     task takes, and it waits until the last write under it is visible, so
//     none of these change while the claim is held. A task's crate changes
//     under the task's lock, in a transaction that advances the workspace's
//     task event position. The view reads the position first and last, and
//     a change in between reads the whole view again, at most
//     feedbackReportViewAttempts times, and then answers store.ErrRetryable.
//   - A task's title, a crate's name, an environment's name, and the
//     listening state with the last signal time are read once for the
//     answer and can be newer than that state. Each crate's name comes from
//     one statement, so the answer holds one name for each crate. The read
//     takes no claim, lock, or retry for them.
//
// Under the claim it runs: the position; the count, an aggregate that keeps
// one number, which stops the read when it passes refuseAbove; the page,
// ORDER BY ... LIMIT, a top-N sort that keeps Offset+Limit rows; the links,
// notes, tasks, crates, environments, and signals of the page only; the read
// clock; and the position again. When validate is set, each page report is
// also checked against the filters with the details read for it, and the
// page against the total; a failed check reads the view again in the same
// way (component-feedback-reports v4 invariants 18, 21, and 24;
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
		page = store.FeedbackReportPage{Reports: []store.FeedbackReport{}, Details: store.NewFeedbackReportDetails()}
		if _, err := claimEnvironments(ctx, tx, ws); err != nil {
			return err
		}
		before, err := feedbackReportTaskPosition(ctx, tx, ws)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_reports r WHERE `+where, whereArgs...).Scan(&page.Total); err != nil {
			return err
		}
		refused := refuseAbove > 0 && page.Total > refuseAbove
		if !refused && page.Total > offset {
			if page.Reports, err = queryFeedbackReportPage(ctx, tx, `SELECT `+prefixFeedbackReportColumns("r")+` FROM feedback_reports r WHERE `+where+
				` ORDER BY r.last_seen DESC, r.id LIMIT ? OFFSET ?`, append(append([]any{}, whereArgs...), limit, offset)...); err != nil {
				return err
			}
			// A task that leaves a crate after the count can leave the page
			// empty; the last position read then reads the view again.
			if len(page.Reports) > 0 {
				if page.Details, err = readFeedbackReportPageDetails(ctx, tx, ws, page.Reports); err != nil {
					return err
				}
			}
		}
		// The read clock is read once for the answer; the listening state is
		// decided at it.
		page.Details.ReadAt = store.FeedbackReadClock(ctx)
		after, err := feedbackReportTaskPosition(ctx, tx, ws)
		if err != nil {
			return err
		}
		if after != before {
			return errFeedbackReportViewMoved
		}
		if validate == nil || refused {
			return nil
		}
		// A second check from the page alone: a page that holds reports ends
		// at or before the total, a page that ends before its limit ends at
		// the total, and each page report matches the filters by its details.
		served := offset + len(page.Reports)
		if (len(page.Reports) > 0 && page.Total < served) || (len(page.Reports) < limit && page.Total > served) {
			return errFeedbackReportViewMoved
		}
		for _, report := range page.Reports {
			if !store.FeedbackReportMatches(*validate, page.Details.Links[report.ID], page.Details.Tasks, page.Details.Crates) {
				return errFeedbackReportViewMoved
			}
		}
		return nil
	})
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	return page, nil
}

// queryFeedbackReportPage reads report rows in the order of
// feedbackReportColumns.
func queryFeedbackReportPage(ctx context.Context, tx *sql.Tx, statement string, args ...any) ([]store.FeedbackReport, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reports := []store.FeedbackReport{}
	for rows.Next() {
		report, err := decodeFeedbackReportRow(rows)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

// readFeedbackReportPageDetails reads, in the view's transaction, the links
// and notes of the given reports, the tasks and crates they name, and the
// reports' environments with their newest signals, and nothing else.
func readFeedbackReportPageDetails(ctx context.Context, tx *sql.Tx, ws string, reports []store.FeedbackReport) (store.FeedbackReportDetails, error) {
	details := store.NewFeedbackReportDetails()
	if len(reports) == 0 {
		return details, nil
	}
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
	// The tasks are read without their crates: a task's crate ID is covered
	// by the task event position, and every crate name is read below in one
	// statement.
	taskCrates := map[string]string{}
	if len(taskIDs) > 0 {
		args := []any{ws}
		for _, id := range taskIDs {
			args = append(args, id)
		}
		taskRows, err := tx.QueryContext(ctx, `SELECT id,title,crate_id FROM tasks WHERE workspace_id=? AND id IN (`+inList(len(taskIDs))+`)`, args...)
		if err != nil {
			return details, err
		}
		err = func() error {
			defer taskRows.Close()
			for taskRows.Next() {
				var ref store.FeedbackReportTaskRef
				var crateID sql.NullString
				if err := taskRows.Scan(&ref.ID, &ref.Title, &crateID); err != nil {
					return err
				}
				details.Tasks[ref.ID] = ref
				if crateID.Valid && crateID.String != "" {
					taskCrates[ref.ID] = crateID.String
					crateIDs = append(crateIDs, crateID.String)
				}
			}
			return taskRows.Err()
		}()
		if err != nil {
			return details, err
		}
	}
	if len(crateIDs) > 0 {
		// Every crate name, of linked crates and of linked tasks' crates, is
		// read once in this one statement, so the answer holds one name for
		// each crate. A name can be newer than the state of the records
		// (component-feedback-reports v4 invariant 21).
		slices.Sort(crateIDs)
		crateIDs = slices.Compact(crateIDs)
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
		for taskID, crateID := range taskCrates {
			if crate, ok := details.Crates[crateID]; ok {
				ref := details.Tasks[taskID]
				ref.CrateID, ref.CrateName = crate.ID, crate.Name
				details.Tasks[taskID] = ref
			}
		}
	}
	environmentIDs := []any{ws}
	seen := map[string]bool{}
	for _, report := range reports {
		if !seen[report.EnvironmentID] {
			seen[report.EnvironmentID] = true
			environmentIDs = append(environmentIDs, report.EnvironmentID)
		}
	}
	environmentRows, err := tx.QueryContext(ctx, `SELECT e.id,e.name,s.last_signal_at,s.next_signal_seconds,s.source,s.source_label,s.actor_id
FROM environments e LEFT JOIN feedback_signals s ON s.workspace_id=e.workspace_id AND s.environment_id=e.id
WHERE e.workspace_id=? AND e.id IN (`+inList(len(environmentIDs)-1)+`)`, environmentIDs...)
	if err != nil {
		return details, err
	}
	defer environmentRows.Close()
	for environmentRows.Next() {
		var ref store.FeedbackReportEnvironment
		var at sql.NullTime
		var seconds sql.NullInt64
		var source, label, actor sql.NullString
		if err := environmentRows.Scan(&ref.ID, &ref.Name, &at, &seconds, &source, &label, &actor); err != nil {
			return details, err
		}
		if at.Valid && seconds.Valid && source.Valid && label.Valid && actor.Valid {
			ref.Signal = &store.FeedbackSignalState{EnvironmentID: ref.ID, LastSignalAt: store.EnvironmentTime(at.Time), NextSignalSeconds: int(seconds.Int64),
				Origin: store.FeedbackOrigin{Source: source.String, Label: label.String, Actor: actor.String}}
		}
		details.Environments[ref.ID] = ref
	}
	return details, environmentRows.Err()
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

// ListInboxFeedbackReports implements store.ReportReadStore with one
// consistent view (readFeedbackReportView) of every open report with
// in_inbox true, with no row ceiling. The count-only form is one aggregate
// statement with the same predicate and loads no report.
func (s *Store) ListInboxFeedbackReports(ctx context.Context, countOnly bool) (store.FeedbackReportPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListInboxFeedbackReports")
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	const where = "r.workspace_id=? AND r.state=? AND r.in_inbox=1"
	args := []any{ws, store.FeedbackReportOpen}
	if countOnly {
		page := store.FeedbackReportPage{Reports: []store.FeedbackReport{}, Details: store.NewFeedbackReportDetails()}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_reports r WHERE `+where, args...).Scan(&page.Total); err != nil {
			return store.FeedbackReportPage{}, err
		}
		page.Details.ReadAt = store.FeedbackReadClock(ctx)
		return page, nil
	}
	// The view reads the count and then the rows under the environment
	// claim, which serializes report writes, so the limit only has to hold
	// every row the count saw.
	return s.readFeedbackReportView(ctx, ws, where, args, math.MaxInt32, 0, 0, nil)
}

// ReadFeedbackReports implements store.ReportReadStore with one consistent
// view of the given reports (readFeedbackReportView).
func (s *Store) ReadFeedbackReports(ctx context.Context, reportIDs []string) ([]store.FeedbackReport, store.FeedbackReportDetails, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackReports")
	if len(reportIDs) == 0 {
		return []store.FeedbackReport{}, store.NewFeedbackReportDetails(), nil
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
// row for each environment. It reads as readFeedbackReportView does: under
// the workspace environment claim, between two reads of the task event
// position, and again when a task change commits in between, at most
// feedbackReportViewAttempts times, so the counts are what one state of the
// store gives and agree with the list on what belongs to a crate
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
	for range feedbackReportViewAttempts {
		err = s.withTx(ctx, func(tx *sql.Tx) error {
			counts = map[string]int{}
			if _, err := claimEnvironments(ctx, tx, ws); err != nil {
				return err
			}
			before, err := feedbackReportTaskPosition(ctx, tx, ws)
			if err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT r.environment_id,COUNT(*) FROM feedback_reports r WHERE r.workspace_id=? AND r.state='open' AND `+
				feedbackServedLink("(lc.id=? OR tc.id=?)")+` GROUP BY r.environment_id`, ws, crateID, crateID)
			if err != nil {
				return err
			}
			err = func() error {
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
			}()
			if err != nil {
				return err
			}
			after, err := feedbackReportTaskPosition(ctx, tx, ws)
			if err != nil {
				return err
			}
			if after != before {
				return errFeedbackReportViewMoved
			}
			return nil
		})
		if !errors.Is(err, errFeedbackReportViewMoved) {
			break
		}
	}
	if errors.Is(err, errFeedbackReportViewMoved) {
		return nil, fmt.Errorf("%w: %v", store.ErrRetryable, err)
	}
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
		if err != nil {
			return err
		}
		if write.Changed {
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
		}
		// The answer is read in the same transaction, under the same claim,
		// before it commits, so no read after the commit can fail the act.
		result.Report, result.Details, err = readFeedbackReportActAnswer(ctx, tx, ws, act.ReportID)
		return err
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

// readFeedbackReportActAnswer reads, in the act's transaction and under its
// claim, the report as it stands after the act with its details and the read
// clock. A report whose environment the workspace does not hold refuses the
// act, which then rolls back with nothing written.
func readFeedbackReportActAnswer(ctx context.Context, tx *sql.Tx, ws, reportID string) (store.FeedbackReport, store.FeedbackReportDetails, error) {
	reports, err := queryFeedbackReportPage(ctx, tx, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=? AND id=?`, ws, reportID)
	if err != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, err
	}
	if len(reports) != 1 {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, fmt.Errorf("%w: feedback report %s", store.ErrNotFound, reportID)
	}
	details, err := readFeedbackReportPageDetails(ctx, tx, ws, reports)
	if err != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, err
	}
	if _, ok := details.Environments[reports[0].EnvironmentID]; !ok {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, fmt.Errorf("%w: report %s", store.ErrFeedbackReportEnvironmentMissing, reportID)
	}
	details.ReadAt = store.FeedbackReadClock(ctx)
	return reports[0], details, nil
}
