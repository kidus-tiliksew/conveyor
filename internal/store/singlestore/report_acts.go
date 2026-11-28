package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.ReportReadStore = (*Store)(nil)

// feedbackReportMembership is the one read, inside a report read, of the
// tasks that links and fix tasks name and of the workspace's crates. Every
// filter of the read and every projected link take membership, titles, and
// crate names from it, so the filter and the projection agree on what
// belongs to a crate even while a task moves between crates
// (component-feedback-reports invariant 21). A task or crate the workspace
// does not hold has no entry.
type feedbackReportMembership struct {
	tasks  map[string]store.FeedbackReportTaskRef
	crates map[string]store.FeedbackReportCrateRef
}

// readFeedbackReportMembership reads the membership in the read's
// transaction, which holds the workspace environment claim.
func readFeedbackReportMembership(ctx context.Context, tx *sql.Tx, ws string) (feedbackReportMembership, error) {
	m := feedbackReportMembership{tasks: map[string]store.FeedbackReportTaskRef{}, crates: map[string]store.FeedbackReportCrateRef{}}
	rows, err := tx.QueryContext(ctx, `SELECT id,name FROM crates WHERE workspace_id=?`, ws)
	if err != nil {
		return m, err
	}
	err = func() error {
		defer rows.Close()
		for rows.Next() {
			var ref store.FeedbackReportCrateRef
			if err := rows.Scan(&ref.ID, &ref.Name); err != nil {
				return err
			}
			m.crates[ref.ID] = ref
		}
		return rows.Err()
	}()
	if err != nil {
		return m, err
	}
	taskRows, err := tx.QueryContext(ctx, `SELECT t.id,t.title,t.crate_id FROM tasks t WHERE t.workspace_id=? AND (
 t.id IN (SELECT l.task_id FROM feedback_report_links l WHERE l.workspace_id=? AND l.task_id IS NOT NULL)
 OR t.id IN (SELECT r.fix_task_id FROM feedback_reports r WHERE r.workspace_id=? AND r.fix_task_id IS NOT NULL))`, ws, ws, ws)
	if err != nil {
		return m, err
	}
	err = func() error {
		defer taskRows.Close()
		for taskRows.Next() {
			var ref store.FeedbackReportTaskRef
			var crateID sql.NullString
			if err := taskRows.Scan(&ref.ID, &ref.Title, &crateID); err != nil {
				return err
			}
			if crate, ok := m.crates[crateID.String]; crateID.Valid && ok {
				ref.CrateID, ref.CrateName = crate.ID, crate.Name
			}
			m.tasks[ref.ID] = ref
		}
		return taskRows.Err()
	}()
	return m, err
}

// members answers the tasks of the membership that are members of a crate,
// and of any crate when crateID is empty, sorted.
func (m feedbackReportMembership) members(crateID string) []string {
	ids := []string{}
	for id, ref := range m.tasks {
		if ref.CrateID != "" && (crateID == "" || ref.CrateID == crateID) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (m feedbackReportMembership) crateIDs() []string {
	ids := []string{}
	for id := range m.crates {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// servedLinkReports renders a subquery of the report IDs that have a served
// link meeting cond, a possible or confirmed link with exactly one target.
// Whether the target exists comes from the membership, which cond names.
func servedLinkReports(cond string) string {
	return `SELECT l.report_id FROM feedback_report_links l WHERE l.workspace_id=? AND l.state IN ('possible','confirmed') AND (` + cond + `)`
}

// inCondition renders column IN (...) with its arguments, or a false
// condition for an empty list.
func inCondition(column string, ids []string) (string, []any) {
	if len(ids) == 0 {
		return "1=0", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return column + " IN (" + inList(len(ids)) + ")", args
}

// crateLinkCondition renders the links that put a report in a crate, or in
// any crate when crateID is empty: a link to a crate the workspace holds, or
// to a task that the membership puts in such a crate (store.FeedbackReportInCrate).
func (m feedbackReportMembership) crateLinkCondition(crateID string) (string, []any) {
	crates := m.crateIDs()
	if crateID != "" {
		crates = []string{}
		if _, ok := m.crates[crateID]; ok {
			crates = []string{crateID}
		}
	}
	crateCond, crateArgs := inCondition("l.crate_id", crates)
	taskCond, taskArgs := inCondition("l.task_id", m.members(crateID))
	return "(l.task_id IS NULL AND " + crateCond + ") OR (l.crate_id IS NULL AND " + taskCond + ")", append(crateArgs, taskArgs...)
}

// feedbackReportFilter renders the WHERE clause of a list and its
// arguments, with every link filter decided against the membership read in
// the same transaction. Every filter is a predicate of the one statement, so
// it runs before the page and the count, and IN keeps a report that matches
// twice to one row.
func feedbackReportFilter(ws string, query store.FeedbackReportQuery, m feedbackReportMembership) (string, []any) {
	clauses := []string{"r.workspace_id=?", "r.state=?"}
	args := []any{ws, query.State}
	if query.EnvironmentID != "" {
		clauses = append(clauses, "r.environment_id=?")
		args = append(args, query.EnvironmentID)
	}
	if query.TaskID != "" {
		if _, ok := m.tasks[query.TaskID]; !ok {
			clauses = append(clauses, "1=0")
		} else {
			clauses = append(clauses, "r.id IN ("+servedLinkReports("l.crate_id IS NULL AND l.task_id=?")+")")
			args = append(args, ws, query.TaskID)
		}
	}
	if query.CrateID != "" {
		cond, condArgs := m.crateLinkCondition(query.CrateID)
		clauses = append(clauses, "r.id IN ("+servedLinkReports(cond)+")")
		args = append(append(args, ws), condArgs...)
	}
	if query.NoCrate {
		cond, condArgs := m.crateLinkCondition("")
		clauses = append(clauses, "r.id NOT IN ("+servedLinkReports(cond)+")")
		args = append(append(args, ws), condArgs...)
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

// feedbackReportViewStatement renders the statement of the report tables
// of a read: the count, the page's records, their links, and their notes.
// The count subquery is joined onto the page subquery, so a page past the
// end answers one row with no report and the total. The read runs it under
// the workspace environment claim, so no report write runs while it reads.
func feedbackReportViewStatement(ws, where string, whereArgs []any, limit, offset int) (string, []any) {
	statement := `SELECT ` + feedbackReportPageColumns() + `, t.total, la.links, na.notes
FROM (SELECT COUNT(*) AS total FROM feedback_reports r WHERE ` + where + `) t
LEFT JOIN (SELECT ` + prefixFeedbackReportColumns("r") + ` FROM feedback_reports r WHERE ` + where + ` ORDER BY r.last_seen DESC, r.id LIMIT ? OFFSET ?) p ON 1=1
LEFT JOIN (SELECT l.report_id, JSON_AGG(JSON_BUILD_OBJECT('id',l.id,'task_id',l.task_id,'crate_id',l.crate_id,'state',l.state,'evidence',l.evidence:>JSON,
 'created_at',l.created_at,'created_by',l.created_by,'confirmed_by',l.confirmed_by,'confirmed_at',l.confirmed_at,'removed_by',l.removed_by,'removed_at',l.removed_at) ORDER BY l.id) AS links
 FROM feedback_report_links l WHERE l.workspace_id=? AND l.report_id IN (SELECT r.id FROM feedback_reports r WHERE ` + where + `) GROUP BY l.report_id) la ON la.report_id=p.id
LEFT JOIN (SELECT n.report_id, JSON_AGG(n.detail:>JSON ORDER BY n.id) AS notes FROM feedback_report_notes n
 WHERE n.workspace_id=? AND n.report_id IN (SELECT r.id FROM feedback_reports r WHERE ` + where + `) GROUP BY n.report_id) na ON na.report_id=p.id
ORDER BY p.last_seen DESC, p.id`
	args := append([]any{}, whereArgs...)
	args = append(append(args, whereArgs...), limit, offset)
	args = append(append(args, ws), whereArgs...)
	args = append(append(args, ws), whereArgs...)
	return statement, args
}

// feedbackReportViewLink is one link of the view statement.
type feedbackReportViewLink struct {
	ID          string          `json:"id"`
	TaskID      *string         `json:"task_id"`
	CrateID     *string         `json:"crate_id"`
	State       string          `json:"state"`
	Evidence    json.RawMessage `json:"evidence"`
	CreatedAt   string          `json:"created_at"`
	CreatedBy   string          `json:"created_by"`
	ConfirmedBy *string         `json:"confirmed_by"`
	ConfirmedAt *string         `json:"confirmed_at"`
	RemovedBy   *string         `json:"removed_by"`
	RemovedAt   *string         `json:"removed_at"`
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

// addFeedbackReportViewLinks decodes the links of one report into details,
// with each named task and crate as the membership holds it.
func addFeedbackReportViewLinks(details *store.FeedbackReportDetails, m feedbackReportMembership, reportID string, raw sql.NullString) error {
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
		addFeedbackReportRefs(details, m, link.TaskID, link.CrateID)
		details.Links[reportID] = append(details.Links[reportID], link)
	}
	return nil
}

// addFeedbackReportRefs copies the task and the crate a link or fix task
// names from the membership into details.
func addFeedbackReportRefs(details *store.FeedbackReportDetails, m feedbackReportMembership, taskID, crateID string) {
	if ref, ok := m.tasks[taskID]; ok && taskID != "" {
		details.Tasks[taskID] = ref
		if ref.CrateID != "" {
			details.Crates[ref.CrateID] = m.crates[ref.CrateID]
		}
	}
	if ref, ok := m.crates[crateID]; ok && crateID != "" {
		details.Crates[crateID] = ref
	}
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

func emptyFeedbackReportDetails() store.FeedbackReportDetails {
	return store.FeedbackReportDetails{Links: map[string][]store.FeedbackReportLink{}, Notes: map[string][]store.FeedbackReportNote{},
		Tasks: map[string]store.FeedbackReportTaskRef{}, Crates: map[string]store.FeedbackReportCrateRef{}}
}

// readFeedbackReportView reads one consistent view of reports. The
// transaction takes the workspace environment claim, the lock every report
// write takes (report entries, the linking work, and the acts), and waits
// until the last write under it is visible, so no report write runs while
// the view is read. SingleStore's read committed isolation keeps no view
// across statements, or even across the tables of one statement, so the
// claim is what makes the record, its links, its notes, and the count one
// view. Membership is read once in the same transaction and decides the
// filters and the projection (component-feedback-reports invariants 18 and
// 21; component-persistence invariant 8).
func (s *Store) readFeedbackReportView(ctx context.Context, ws string, filter func(feedbackReportMembership) (string, []any), limit, offset int) (store.FeedbackReportPage, error) {
	page := store.FeedbackReportPage{}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		page = store.FeedbackReportPage{Reports: []store.FeedbackReport{}, Details: emptyFeedbackReportDetails()}
		if _, err := claimEnvironments(ctx, tx, ws); err != nil {
			return err
		}
		m, err := readFeedbackReportMembership(ctx, tx, ws)
		if err != nil {
			return err
		}
		where, whereArgs := filter(m)
		statement, args := feedbackReportViewStatement(ws, where, whereArgs, limit, offset)
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var total int64
			var links, notes sql.NullString
			report, err := decodeFeedbackReportRow(rows, &total, &links, &notes)
			if err != nil {
				return err
			}
			page.Total = int(total)
			if report.ID == "" {
				continue
			}
			page.Reports = append(page.Reports, report)
			page.Details.Links[report.ID], page.Details.Notes[report.ID] = []store.FeedbackReportLink{}, []store.FeedbackReportNote{}
			if err := addFeedbackReportViewLinks(&page.Details, m, report.ID, links); err != nil {
				return err
			}
			if err := addFeedbackReportViewNotes(&page.Details, report.ID, notes); err != nil {
				return err
			}
			addFeedbackReportRefs(&page.Details, m, report.FixTaskID, "")
		}
		return rows.Err()
	})
	if err != nil {
		return store.FeedbackReportPage{}, err
	}
	return page, nil
}

// ListFeedbackReports implements store.ReportReadStore with one consistent
// view (readFeedbackReportView).
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
	return s.readFeedbackReportView(ctx, ws, func(m feedbackReportMembership) (string, []any) { return feedbackReportFilter(ws, query, m) }, query.Limit, query.Offset)
}

// ReadFeedbackReports implements store.ReportReadStore with one consistent
// view (readFeedbackReportView).
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
	where := "r.workspace_id=? AND r.id IN (" + inList(len(reportIDs)) + ")"
	page, err := s.readFeedbackReportView(ctx, ws, func(feedbackReportMembership) (string, []any) { return where, args }, len(reportIDs), 0)
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
// crate filter of the list, decided against one membership read, under the
// workspace environment claim, so the count and the list agree on what
// belongs to a crate (component-feedback-reports invariants 21 and 23).
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
		m, err := readFeedbackReportMembership(ctx, tx, ws)
		if err != nil {
			return err
		}
		cond, condArgs := m.crateLinkCondition(crateID)
		rows, err := tx.QueryContext(ctx, `SELECT r.environment_id,COUNT(*) FROM feedback_reports r WHERE r.workspace_id=? AND r.state='open' AND r.id IN (`+
			servedLinkReports(cond)+`) GROUP BY r.environment_id`, append([]any{ws, ws}, condArgs...)...)
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
