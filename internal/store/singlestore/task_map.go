package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskMapColumns is taskColumns with the body replaced by an empty string,
// so the map read never transfers task bodies (req-board-map AC-3.1).
var taskMapColumns = strings.Replace(taskColumns, ",body,", ",'' AS body,", 1)

func taskMapMarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func taskMapArgs(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func (s *Store) scanTaskMapRows(ctx context.Context, query string, args ...any) ([]core.Task, error) {
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

// ListTaskMap reads the workspace task map in at most five set-based
// queries without correlated subqueries: open and scope-selected task rows,
// the workspace's dependency edges, any finished dependency rows of selected
// open tasks, the requirement add/remove events with their task states, and
// the workspace's requirement titles (component-persistence).
func (s *Store) ListTaskMap(ctx context.Context, query store.TaskMapQuery) (store.TaskMap, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return store.TaskMap{}, err
	}
	finishedClause := "FALSE"
	args := []any{ws}
	if query.IncludeFinished {
		finishedClause = "TRUE"
		if len(query.Repositories) > 0 {
			finishedClause = "t.repo_name IN (" + taskMapMarks(len(query.Repositories)) + ")"
			args = append(args, taskMapArgs(query.Repositories)...)
		}
	}
	loaded, err := s.scanTaskMapRows(ctx, `SELECT `+taskMapColumns+` FROM tasks t
WHERE t.workspace_id=? AND (t.state NOT IN ('merged','closed') OR `+finishedClause+`)
ORDER BY t.id`, args...)
	if err != nil {
		return store.TaskMap{}, err
	}
	loadedByID := make(map[string]bool, len(loaded))
	for _, task := range loaded {
		loadedByID[task.ID] = true
	}
	edgeRows, err := documentRows(ctx, s.db, `SELECT task_id, depends_on_task_id FROM task_dependencies
WHERE workspace_id=? ORDER BY task_id, depends_on_task_id`, ws)
	if err != nil {
		return store.TaskMap{}, err
	}
	allEdges := map[string][]string{}
	for edgeRows.Next() {
		var taskID, dependencyID string
		if err = edgeRows.Scan(&taskID, &dependencyID); err != nil {
			edgeRows.Close()
			return store.TaskMap{}, err
		}
		allEdges[taskID] = append(allEdges[taskID], dependencyID)
	}
	err = edgeRows.Err()
	edgeRows.Close()
	if err != nil {
		return store.TaskMap{}, err
	}
	var missing []string
	for _, task := range loaded {
		if core.TaskTerminal(task.State) || !store.TaskMapSelects(store.TaskMapQuery{Repositories: query.Repositories}, task, false) {
			continue
		}
		for _, dependencyID := range allEdges[task.ID] {
			if !loadedByID[dependencyID] {
				loadedByID[dependencyID] = true
				missing = append(missing, dependencyID)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		finished, err := s.scanTaskMapRows(ctx, `SELECT `+taskMapColumns+` FROM tasks t
WHERE t.workspace_id=? AND t.id IN (`+taskMapMarks(len(missing))+`) ORDER BY t.id`, append([]any{ws}, taskMapArgs(missing)...)...)
		if err != nil {
			return store.TaskMap{}, err
		}
		loaded = append(loaded, finished...)
	}
	edges := map[string][]string{}
	for _, task := range loaded {
		if dependencies := allEdges[task.ID]; len(dependencies) > 0 {
			edges[task.ID] = dependencies
		}
	}
	attachments, err := s.listTaskMapAttachments(ctx, ws)
	if err != nil {
		return store.TaskMap{}, err
	}
	return store.AssembleTaskMap(query, loaded, edges, attachments), nil
}

// listTaskMapAttachments reads every requirement add/remove event of the
// workspace's tasks in (at, id) order, folds each task's events with
// store.ActiveTaskContextReferences, and keeps requirements with a confirmed
// version, the documents store.TaskContextFromEvents resolves.
func (s *Store) listTaskMapAttachments(ctx context.Context, ws string) ([]store.TaskMapAttachment, error) {
	rows, err := documentRows(ctx, s.db, `SELECT e.id, e.task_id, e.kind, e.payload_json, e.at, t.state
FROM events e JOIN tasks t ON t.workspace_id=e.workspace_id AND t.id=e.task_id
WHERE e.workspace_id=? AND e.task_id IS NOT NULL AND e.kind IN (?,?)
ORDER BY e.task_id, e.at, e.id`, ws, store.TaskContextRequirementAdded, store.TaskContextRequirementRemoved)
	if err != nil {
		return nil, err
	}
	eventsByTask := map[string][]core.Event{}
	states := map[string]core.TaskState{}
	var taskOrder []string
	for rows.Next() {
		var event core.Event
		var state string
		if err = rows.Scan(&event.ID, &event.TaskID, &event.Kind, &event.Payload, &event.At, &state); err != nil {
			rows.Close()
			return nil, err
		}
		if _, seen := eventsByTask[event.TaskID]; !seen {
			taskOrder = append(taskOrder, event.TaskID)
		}
		eventsByTask[event.TaskID] = append(eventsByTask[event.TaskID], event)
		states[event.TaskID] = core.TaskState(state)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(taskOrder) == 0 {
		return nil, nil
	}
	requirementRows, err := documentRows(ctx, s.db, `SELECT id, title, COALESCE(current_version,0) FROM requirements WHERE workspace_id=?`, ws)
	if err != nil {
		return nil, err
	}
	type requirementHead struct {
		title   string
		current int
	}
	requirements := map[string]requirementHead{}
	for requirementRows.Next() {
		var id string
		var head requirementHead
		if err = requirementRows.Scan(&id, &head.title, &head.current); err != nil {
			requirementRows.Close()
			return nil, err
		}
		requirements[id] = head
	}
	err = requirementRows.Err()
	requirementRows.Close()
	if err != nil {
		return nil, err
	}
	var out []store.TaskMapAttachment
	for _, taskID := range taskOrder {
		active, _ := store.ActiveTaskContextReferences(eventsByTask[taskID])
		ids := make([]string, 0, len(active))
		for id := range active {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			head, ok := requirements[id]
			if !ok || head.current <= 0 {
				continue
			}
			out = append(out, store.TaskMapAttachment{TaskID: taskID, TaskState: states[taskID], RequirementID: id, RequirementTitle: head.title})
		}
	}
	return out, nil
}

// ListNextActionEvents reads only the assembler's event kinds for the
// requested tasks in one query, in the (at, id) order ListEvents returns.
func (s *Store) ListNextActionEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	out := map[string][]core.Event{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	args := append([]any{ws}, taskMapArgs(taskIDs)...)
	args = append(args, taskMapArgs(store.NextActionEventKinds)...)
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events
WHERE workspace_id=? AND task_id IN (`+taskMapMarks(len(taskIDs))+`) AND kind IN (`+taskMapMarks(len(store.NextActionEventKinds))+`)
ORDER BY task_id,at,id`, args...)
	if err != nil {
		return nil, err
	}
	events, err := scanDocumentEvents(rows)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		out[event.TaskID] = append(out[event.TaskID], event)
	}
	return out, nil
}

// ListLatestSpecVersionNumbers reads each requested task's latest plan
// version in one query, the version GetLatestSpecVersion returns.
func (s *Store) ListLatestSpecVersionNumbers(ctx context.Context, taskIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT task_id, MAX(version) FROM task_specs
WHERE workspace_id=? AND task_id IN (`+taskMapMarks(len(taskIDs))+`) GROUP BY task_id`, append([]any{ws}, taskMapArgs(taskIDs)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var version int
		if err = rows.Scan(&taskID, &version); err != nil {
			return nil, err
		}
		out[taskID] = version
	}
	return out, rows.Err()
}

// ListClaimBlockingProposalsForTasks applies claimBlockingProposalsTx's
// predicate to every requested task in one query.
func (s *Store) ListClaimBlockingProposalsForTasks(ctx context.Context, taskIDs []string) (map[string][]store.ClaimBlockingProposal, error) {
	out := map[string][]store.ClaimBlockingProposal{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	marks := taskMapMarks(len(taskIDs))
	args := append([]any{ws, string(core.SystemDesignOriginImplementation)}, taskMapArgs(taskIDs)...)
	args = append(args, ws, string(core.RequirementOriginImplementation))
	args = append(args, taskMapArgs(taskIDs)...)
	rows, err := documentRows(ctx, s.db, `SELECT origin_task_id,'system_design' AS tier,document_id AS id,version FROM system_design_versions
WHERE workspace_id=? AND origin=? AND origin_task_id IN (`+marks+`) AND NOT confirmed AND NOT dismissed
UNION ALL SELECT origin_task_id,'requirement',requirement_id,version FROM requirement_versions
WHERE workspace_id=? AND origin=? AND origin_task_id IN (`+marks+`) AND NOT confirmed AND NOT retired`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var item store.ClaimBlockingProposal
		if err = rows.Scan(&taskID, &item.Tier, &item.ID, &item.Version); err != nil {
			return nil, err
		}
		out[taskID] = append(out[taskID], item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return store.GroupClaimBlockingProposals(out), nil
}

// ListTaskHeads reads the requested tasks' identity, title, and state in one
// query.
func (s *Store) ListTaskHeads(ctx context.Context, taskIDs []string) (map[string]store.TaskHead, error) {
	out := map[string]store.TaskHead{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,title,state FROM tasks WHERE workspace_id=? AND id IN (`+taskMapMarks(len(taskIDs))+`)`, append([]any{ws}, taskMapArgs(taskIDs)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var head store.TaskHead
		var state string
		if err = rows.Scan(&head.ID, &head.Title, &state); err != nil {
			return nil, fmt.Errorf("scan task head: %w", err)
		}
		head.State = core.TaskState(state)
		out[head.ID] = head
	}
	return out, rows.Err()
}

func scanNextActionRows(rows *documentResultSet) ([]core.Event, error) {
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		var event core.Event
		if err := rows.Scan(&event.ID, &event.TaskID, &event.Kind, &event.Payload, &event.At); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// positiveInt64SQL accepts a JSON number token that Go decodes into a
// positive int: digits alone, without fraction or exponent, at most
// 9223372036854775807.
func positiveInt64SQL(text string) string {
	return `(` + text + ` RLIKE '^[1-9][0-9]*$' AND (LENGTH(` + text + `) < 19 OR (LENGTH(` + text + `) = 19 AND ` + text + ` <= '9223372036854775807')))`
}

// int64SQL accepts a JSON number token that Go decodes into an int.
func int64SQL(text string) string {
	return `((` + text + ` RLIKE '^[0-9]+$' AND (LENGTH(` + text + `) < 19 OR (LENGTH(` + text + `) = 19 AND ` + text + ` <= '9223372036854775807')))` +
		` OR (` + text + ` RLIKE '^-[0-9]+$' AND (LENGTH(` + text + `) < 20 OR (LENGTH(` + text + `) = 20 AND ` + text + ` <= '-9223372036854775808'))))`
}

// jsonStringOrNullSQL reports a missing, null, or string JSON field.
func jsonStringOrNullSQL(field string) string {
	return `(` + field + ` IS NULL OR ` + field + ` = 'null' OR LEFT(` + field + `,1) = '"')`
}

// retryTimePattern is the shape time.Time's JSON decoder accepts in Go 1.24,
// as the PostgreSQL backend states it.
const retryTimePattern = `^"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{1,2}:[0-9]{2}:[0-9]{2}([.,][0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})"$`

// retryNumberSQL reads the digits at a fixed position of an accepted value.
func retryNumberSQL(start, length string) string {
	return `CAST(SUBSTRING(raw, ` + start + `, ` + length + `) AS SIGNED)`
}

// retryAcceptedSQL reports whether the JSON text raw of a failure's
// next_retry_at decodes, with the PostgreSQL backend's range checks; hp is
// the hour's digit count.
var retryAcceptedSQL = `(raw IS NULL OR raw = 'null' OR (CASE WHEN hp IS NULL THEN 0 ELSE
	` + retryNumberSQL("7", "2") + ` BETWEEN 1 AND 12
	AND ` + retryNumberSQL("10", "2") + ` BETWEEN 1 AND (CASE ` + retryNumberSQL("7", "2") + `
		WHEN 2 THEN CASE WHEN MOD(` + retryNumberSQL("2", "4") + `, 4) = 0 AND (MOD(` + retryNumberSQL("2", "4") + `, 100) <> 0 OR MOD(` + retryNumberSQL("2", "4") + `, 400) = 0) THEN 29 ELSE 28 END
		WHEN 4 THEN 30 WHEN 6 THEN 30 WHEN 9 THEN 30 WHEN 11 THEN 30 ELSE 31 END)
	AND ` + retryNumberSQL("13", "hp") + ` < 24
	AND ` + retryNumberSQL("14 + hp", "2") + ` < 60
	AND ` + retryNumberSQL("17 + hp", "2") + ` < 60
	AND (RIGHT(raw, 2) = 'Z"' OR (` + retryNumberSQL("LENGTH(raw) - 5", "2") + ` <= 24 AND ` + retryNumberSQL("LENGTH(raw) - 2", "2") + ` <= 60))
END))`

// retryNormalizedSQL keeps at most nine fraction digits of an accepted retry
// time, so the transferred value is at most 37 bytes.
const retryNormalizedSQL = `CONCAT(LEFT(raw, 18 + hp), LEFT(SUBSTRING(raw, 19 + hp, LENGTH(raw) - 18 - hp - CASE WHEN RIGHT(raw, 2) = 'Z"' THEN 2 ELSE 7 END), 10), CASE WHEN RIGHT(raw, 2) = 'Z"' THEN 'Z"' ELSE RIGHT(raw, 7) END)`

// mergeConflictSummarySQL computes store.MergeConflictHistory per task, as
// the PostgreSQL backend does, without correlated subqueries.
var mergeConflictSummarySQL = `WITH m AS (
	SELECT task_id, kind, payload_json AS p,
		ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY at, id) AS rn,
		((LEFT(payload_json :> TEXT, 1) = '{' OR (payload_json :> TEXT) = 'null')
			AND ` + jsonStringOrNullSQL(`JSON_EXTRACT_JSON(payload_json,'approved_head')`) + `
			AND ` + jsonStringOrNullSQL(`JSON_EXTRACT_JSON(payload_json,'new_head')`) + `) AS decodes,
		COALESCE(JSON_EXTRACT_STRING(payload_json,'approved_head'),'') AS approved_head,
		COALESCE(JSON_EXTRACT_STRING(payload_json,'new_head'),'') AS new_head
	FROM events
	WHERE workspace_id = ? AND task_id IN (%s) AND kind IN (%s)
), r AS (
	SELECT task_id, kind, p, rn, decodes, approved_head, new_head,
		MAX(CASE WHEN kind IN ('merge.blocked','merge.conflict_cleared') THEN rn END)
			OVER (PARTITION BY task_id ORDER BY rn ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS reset_rn
	FROM m
), lb AS (
	SELECT task_id, MAX(rn) AS rn FROM m WHERE kind = 'merge.blocked' GROUP BY task_id
), cl AS (
	SELECT task_id, MAX(rn) AS rn FROM m WHERE kind = 'merge.conflict_cleared' GROUP BY task_id
), s AS (
	SELECT m.task_id, m.rn, m.approved_head, m.new_head, (cl.rn IS NOT NULL AND cl.rn > m.rn) AS cleared
	FROM m JOIN lb ON lb.task_id = m.task_id AND lb.rn = m.rn
	LEFT JOIN cl ON cl.task_id = m.task_id
	WHERE m.decodes
), same AS (
	SELECT s.task_id, s.cleared, x.kind, x.rn, x.p, x.reset_rn, s.rn AS latest_rn
	FROM s JOIN r x ON x.task_id = s.task_id AND x.decodes AND x.approved_head = s.approved_head AND x.new_head = s.new_head
), ex AS (
	SELECT DISTINCT task_id FROM same WHERE kind = 'merge.conflict_dispatch_exhausted' AND rn > latest_rn
), episode AS (
	SELECT task_id, rn, kind, CASE WHEN kind = 'merge.conflict_dispatch_failed' THEN JSON_EXTRACT_JSON(p,'next_retry_at') END AS raw
	FROM same
	WHERE NOT cleared AND rn > latest_rn AND kind IN ('merge.conflict_fix_dispatched','merge.conflict_dispatch_failed')
), steps AS (
	SELECT task_id, rn, kind, raw, CASE WHEN raw RLIKE '` + retryTimePattern + `' THEN LOCATE(':', SUBSTRING(raw, 13)) - 1 END AS hp
	FROM episode
), effective AS (
	SELECT task_id, rn, kind, raw, hp,
		ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY rn DESC) AS position
	FROM steps
	WHERE kind = 'merge.conflict_fix_dispatched' OR ` + retryAcceptedSQL + `
), failures AS (
	SELECT task_id, COUNT(*) AS n FROM effective WHERE kind = 'merge.conflict_dispatch_failed' GROUP BY task_id
), lst AS (
	SELECT task_id,
		CASE WHEN kind = 'merge.conflict_dispatch_failed' AND hp IS NOT NULL THEN ` + retryNormalizedSQL + ` END AS retry
	FROM effective WHERE position = 1
)
SELECT 0 AS row_kind, s.task_id, s.cleared, (NOT s.cleared AND ex.task_id IS NOT NULL) AS exhausted,
	COALESCE(failures.n, 0) AS failures, lst.retry, '' AS value
FROM s
LEFT JOIN ex ON ex.task_id = s.task_id
LEFT JOIN failures ON failures.task_id = s.task_id
LEFT JOIN lst ON lst.task_id = s.task_id
UNION ALL
SELECT DISTINCT 2, x.task_id, 0, 0, 0, NULL, JSON_EXTRACT_STRING(x.p,'work_order_id')
FROM same x
JOIN same b ON b.task_id = x.task_id AND b.rn = x.reset_rn AND b.kind = 'merge.blocked'
JOIN work_orders wo ON wo.workspace_id = ? AND wo.task_id = x.task_id AND wo.id = JSON_EXTRACT_STRING(x.p,'work_order_id')
WHERE x.kind = 'merge.conflict_fix_dispatched' AND LEFT(JSON_EXTRACT_JSON(x.p,'work_order_id'),1) = '"'`

// ListNextActionHistory computes store.NextActionHistory in two queries:
// the typed latest values of the park-command, pull-request, and bounce
// folds, ranked with ROW_NUMBER, and the fixed-size merge-conflict summary
// (component-persistence).
func (s *Store) ListNextActionHistory(ctx context.Context, taskIDs []string) (map[string]store.NextActionHistory, error) {
	out := map[string]store.NextActionHistory{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	marks := taskMapMarks(len(taskIDs))
	window := `JSON_EXTRACT_JSON(payload_json,'window')`
	// Each row carries only a typed value: the command's first 64
	// characters, the number's digits, or the window's digits when the fold
	// accepts it and 0 otherwise.
	rows, err := documentRows(ctx, s.db, `SELECT task_id, kind, value FROM (
	SELECT task_id, kind,
		CASE
			WHEN kind='task.state_changed' THEN LEFT(JSON_EXTRACT_STRING(payload_json,'command'), 64)
			WHEN kind='pull_request.opened' THEN JSON_EXTRACT_JSON(payload_json,'number')
			WHEN (LEFT(payload_json :> TEXT, 1) = '{' OR (payload_json :> TEXT) = 'null') AND `+positiveInt64SQL(window)+` THEN `+window+`
			ELSE '0'
		END AS value,
		ROW_NUMBER() OVER (PARTITION BY task_id,kind ORDER BY at DESC,id DESC) AS position
	FROM events
	WHERE workspace_id=? AND task_id IN (`+marks+`) AND (
		(kind='task.state_changed' AND LEFT(JSON_EXTRACT_JSON(payload_json,'command'),1)='"' AND JSON_EXTRACT_STRING(payload_json,'command')<>'')
		OR (kind='pull_request.opened' AND `+positiveInt64SQL(`JSON_EXTRACT_JSON(payload_json,'number')`)+`)
		OR kind='pipeline.bounce_limit')
) latest
WHERE position=1`, append([]any{ws}, taskMapArgs(taskIDs)...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var taskID, kind, value string
		if err = rows.Scan(&taskID, &kind, &value); err != nil {
			rows.Close()
			return nil, err
		}
		history := out[taskID]
		if err = store.ApplyLatestHistoryValue(&history, kind, value); err != nil {
			rows.Close()
			return nil, err
		}
		out[taskID] = history
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	args := append([]any{ws}, taskMapArgs(taskIDs)...)
	args = append(args, taskMapArgs(store.MergeConflictEventKinds)...)
	args = append(args, ws)
	summary, err := documentRows(ctx, s.db, fmt.Sprintf(mergeConflictSummarySQL, marks, taskMapMarks(len(store.MergeConflictEventKinds))), args...)
	if err != nil {
		return nil, err
	}
	defer summary.Close()
	for summary.Next() {
		var rowKind int
		var taskID, value string
		var cleared, exhausted bool
		var failures int64
		var retry sql.NullString
		if err = summary.Scan(&rowKind, &taskID, &cleared, &exhausted, &failures, &retry, &value); err != nil {
			return nil, err
		}
		history := out[taskID]
		if rowKind == 0 {
			history.MergeConflict = store.MergeConflictHistory{Open: true, Cleared: cleared, Exhausted: exhausted, Failures: int(failures), DispatchedOrderIDs: history.MergeConflict.DispatchedOrderIDs}
			if retry.Valid {
				history.MergeConflict.NextRetryAt = json.RawMessage(retry.String)
			}
		} else {
			history.MergeConflict.DispatchedOrderIDs = append(history.MergeConflict.DispatchedOrderIDs, value)
		}
		out[taskID] = history
	}
	if err = summary.Err(); err != nil {
		return nil, err
	}
	for taskID, history := range out {
		sort.Strings(history.MergeConflict.DispatchedOrderIDs)
		out[taskID] = history
	}
	return store.CompactNextActionHistories(out), nil
}

// ListOpenDependents reads, in one query, the heads of the workspace's open
// tasks with a dependency edge to any requested task.
func (s *Store) ListOpenDependents(ctx context.Context, taskIDs []string) ([]store.TaskHead, error) {
	out := []store.TaskHead{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT DISTINCT t.id,t.title,t.state FROM tasks t
JOIN task_dependencies edge ON edge.workspace_id=t.workspace_id AND edge.task_id=t.id
WHERE t.workspace_id=? AND t.state NOT IN ('merged','closed') AND edge.depends_on_task_id IN (`+taskMapMarks(len(taskIDs))+`)
ORDER BY t.id`, append([]any{ws}, taskMapArgs(taskIDs)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var head store.TaskHead
		var state string
		if err = rows.Scan(&head.ID, &head.Title, &state); err != nil {
			return nil, fmt.Errorf("scan dependent head: %w", err)
		}
		head.State = core.TaskState(state)
		out = append(out, head)
	}
	return out, rows.Err()
}

// ListTaskContextDesignPins selects, in one query, the latest decodable
// design add or remove per task and design ID and keeps the adds, so only
// current pins cross the boundary (component-persistence).
func (s *Store) ListTaskContextDesignPins(ctx context.Context, taskIDs, documentIDs []string) (map[string]map[string]int, error) {
	out := map[string]map[string]int{}
	if len(taskIDs) == 0 || len(documentIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	version := `JSON_EXTRACT_JSON(payload_json,'version')`
	args := append([]any{ws}, taskMapArgs(taskIDs)...)
	args = append(args, store.TaskContextDesignAdded, store.TaskContextDesignRemoved)
	args = append(args, taskMapArgs(documentIDs)...)
	args = append(args, store.TaskContextDesignAdded)
	rows, err := documentRows(ctx, s.db, `SELECT task_id, design_id, version FROM (
	SELECT task_id, kind, COALESCE(JSON_EXTRACT_STRING(payload_json,'id'),'') AS design_id,
		CASE WHEN `+version+` IS NULL OR `+version+` = 'null' THEN 0 ELSE CAST(`+version+` AS SIGNED) END AS version,
		ROW_NUMBER() OVER (PARTITION BY task_id, COALESCE(JSON_EXTRACT_STRING(payload_json,'id'),'') ORDER BY at DESC, id DESC) AS position
	FROM events
	WHERE workspace_id=? AND task_id IN (`+taskMapMarks(len(taskIDs))+`) AND kind IN (?,?)
		AND COALESCE(JSON_EXTRACT_STRING(payload_json,'id'),'') IN (`+taskMapMarks(len(documentIDs))+`)
		AND (LEFT(payload_json :> TEXT, 1) = '{' OR (payload_json :> TEXT) = 'null')
		AND `+jsonStringOrNullSQL(`JSON_EXTRACT_JSON(payload_json,'id')`)+`
		AND (`+version+` IS NULL OR `+version+` = 'null' OR `+int64SQL(version)+`)
) latest
WHERE position=1 AND kind=?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, designID string
		var pinned int64
		if err = rows.Scan(&taskID, &designID, &pinned); err != nil {
			return nil, err
		}
		if out[taskID] == nil {
			out[taskID] = map[string]int{}
		}
		out[taskID][designID] = int(pinned)
	}
	return out, rows.Err()
}

// ListDocumentHeads reads requirement and System Design heads in one query;
// a requirement wins over a System Design with the same ID.
func (s *Store) ListDocumentHeads(ctx context.Context, ids []string) (map[string]store.DocumentHead, error) {
	out := map[string]store.DocumentHead{}
	if len(ids) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	marks := taskMapMarks(len(ids))
	args := append([]any{ws}, taskMapArgs(ids)...)
	args = append(args, ws)
	args = append(args, taskMapArgs(ids)...)
	rows, err := documentRows(ctx, s.db, `SELECT id,'requirement' AS tier,COALESCE(current_version,0),archived_at IS NOT NULL FROM requirements
WHERE workspace_id=? AND id IN (`+marks+`)
UNION ALL SELECT id,'system_design',COALESCE(current_version,0),archived_at IS NOT NULL FROM system_designs
WHERE workspace_id=? AND id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var head store.DocumentHead
		if err = rows.Scan(&head.ID, &head.Tier, &head.CurrentVersion, &head.Archived); err != nil {
			return nil, err
		}
		if existing, ok := out[head.ID]; ok && existing.Tier == store.DocumentHeadRequirement {
			continue
		}
		out[head.ID] = head
	}
	return out, rows.Err()
}

// ReadVerificationReviews reads every verification table once for all
// requested tasks and the requested orders in one query, inside one
// transaction, with ReadVerificationReview's per-pair result and errors.
func (s *Store) ReadVerificationReviews(ctx context.Context, refs []store.VerificationReviewRef) (map[string]store.VerificationReviewState, error) {
	out := make(map[string]store.VerificationReviewState, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	var taskIDs, orderIDs []string
	for _, ref := range refs {
		if !slices.Contains(taskIDs, ref.TaskID) {
			taskIDs = append(taskIDs, ref.TaskID)
		}
		orderIDs = append(orderIDs, ref.OrderID)
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ws := documentWorkspace(ctx)
		byTask := map[string][]store.VerificationRow{}
		for _, table := range store.VerificationTables {
			condition := "task_id IN (" + taskMapMarks(len(taskIDs)) + ")"
			if table == "verification_operations" {
				condition = "(" + condition + " OR true)"
			}
			rows, err := tx.QueryContext(ctx, "SELECT id,task_id,context_id,run_id,logical_key,state,body,expires_at FROM "+table+" WHERE workspace_id=? AND "+condition+" ORDER BY id", append([]any{ws}, taskMapArgs(taskIDs)...)...)
			if err != nil {
				return err
			}
			var shared []store.VerificationRow
			for rows.Next() {
				v := store.VerificationRow{Table: table}
				if err = rows.Scan(&v.ID, &v.TaskID, &v.ContextID, &v.RunID, &v.LogicalKey, &v.State, &v.Body, &v.ExpiresAt); err != nil {
					rows.Close()
					return err
				}
				if table == "verification_operations" {
					shared = append(shared, v)
					continue
				}
				byTask[v.TaskID] = append(byTask[v.TaskID], v)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			// Operations are read for the whole workspace, as the
			// per-task read does, so every task receives them in ID order.
			if table == "verification_operations" {
				for _, id := range taskIDs {
					byTask[id] = append(byTask[id], shared...)
				}
			}
		}
		rows, err := documentRows(ctx, tx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=? AND id IN (`+taskMapMarks(len(orderIDs))+`)`, append([]any{ws}, taskMapArgs(orderIDs)...)...)
		if err != nil {
			return err
		}
		orders := map[string]core.WorkOrder{}
		for rows.Next() {
			order, err := scanWorkOrder(rows)
			if err != nil {
				rows.Close()
				return err
			}
			orders[order.ID] = order
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, ref := range refs {
			order, ok := orders[ref.OrderID]
			if !ok {
				return fmt.Errorf("%w: work order %s", store.ErrNotFound, ref.OrderID)
			}
			if order.TaskID != ref.TaskID {
				return store.ErrVerificationAccess
			}
			out[ref.OrderID] = store.VerificationReviewState{Rows: byTask[ref.TaskID], Order: order}
		}
		return nil
	})
	return out, err
}
