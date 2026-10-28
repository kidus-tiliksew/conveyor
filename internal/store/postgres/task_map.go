package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.NextActionInputStore = (*Store)(nil)

// taskMapColumns is db.Task's column set with the body replaced by an empty
// string, so the map read never transfers task bodies (req-board-map AC-3.1).
const taskMapColumns = `t.supersedes, t.superseded_by, t.intake_operator_direction, t.id, t.workspace_id,
	t.source, t.title, ''::text AS body, t.class, t.escalation_level, t.repo_name, t.base_branch, t.branch,
	t.state, t.parent_task_id, t.created_at, t.updated_at, t.next_stage, t.recovery_stage, t.intake_key,
	t.mode, t.spec_approval, t.merge_approval, t.policy_version, t.setup_name, t.setup_contract, t.hold,
	t.reviewed_head_sha, t.approved_head_sha, t.approval_stale, t.refresh_baseline_sha, t.refresh_head_sha,
	t.refresh_review_scope, t.origin_spec_version, t.origin_sub_id, t.assignee_user_id, t.gate_kind`

// ListTaskMap reads the workspace task map in four set-based queries: task
// rows, assignees, dependency edges, and active requirement attachments with
// their titles (component-persistence). Every open workspace task is loaded,
// because the next-action dependency view needs dependents outside the
// repository selection; finished rows load only when the scope selects them.
func (s *Store) ListTaskMap(ctx context.Context, query store.TaskMapQuery) (store.TaskMap, error) {
	workspaceID := workspace(ctx)
	repositories := emptyIfNil(query.Repositories)
	rows, err := s.boundary.Query(ctx, `SELECT `+taskMapColumns+`
FROM tasks t
WHERE t.workspace_id=$1 AND (
	t.state NOT IN ('merged','closed')
	OR ($2::boolean AND (cardinality($3::text[])=0 OR t.repo_name=ANY($3::text[])))
	OR EXISTS (
		SELECT 1 FROM task_dependencies edge
		JOIN tasks dependent ON dependent.workspace_id=edge.workspace_id AND dependent.id=edge.task_id
		WHERE edge.workspace_id=t.workspace_id AND edge.depends_on_task_id=t.id
		  AND dependent.state NOT IN ('merged','closed')
		  AND (cardinality($3::text[])=0 OR dependent.repo_name=ANY($3::text[]))
	)
)
ORDER BY t.id`, workspaceID, query.IncludeFinished, repositories)
	if err != nil {
		return store.TaskMap{}, err
	}
	persisted, err := pgx.CollectRows(rows, pgx.RowToStructByName[db.Task])
	if err != nil {
		return store.TaskMap{}, err
	}
	loaded := make([]core.Task, len(persisted))
	ids := make([]string, len(persisted))
	for i := range persisted {
		loaded[i] = taskFromDB(persisted[i])
		ids[i] = loaded[i].ID
	}
	if len(loaded) > 0 {
		if err = s.hydrateTaskAssignees(ctx, loaded); err != nil {
			return store.TaskMap{}, err
		}
	}
	edges := map[string][]string{}
	if len(ids) > 0 {
		edgeRows, err := s.boundary.Query(ctx, `SELECT task_id, depends_on_task_id FROM task_dependencies
WHERE workspace_id=$1 AND task_id=ANY($2::text[])
ORDER BY task_id, depends_on_task_id`, workspaceID, ids)
		if err != nil {
			return store.TaskMap{}, err
		}
		for edgeRows.Next() {
			var taskID, dependencyID string
			if err = edgeRows.Scan(&taskID, &dependencyID); err != nil {
				edgeRows.Close()
				return store.TaskMap{}, err
			}
			edges[taskID] = append(edges[taskID], dependencyID)
		}
		edgeRows.Close()
		if err = edgeRows.Err(); err != nil {
			return store.TaskMap{}, err
		}
	}
	attachments, err := s.listTaskMapAttachments(ctx, workspaceID)
	if err != nil {
		return store.TaskMap{}, err
	}
	return store.AssembleTaskMap(query, loaded, edges, attachments), nil
}

// listTaskMapAttachments selects, in one query, the latest requirement
// add/remove event per task and requirement in (at, id) order, the order
// store.ActiveTaskContextReferences folds, and keeps the adds whose
// requirement has a confirmed version, the documents
// store.TaskContextFromEvents resolves. A task-context proposal writes no
// add event until it is confirmed, so a pending proposal is not an
// attachment.
func (s *Store) listTaskMapAttachments(ctx context.Context, workspaceID string) ([]store.TaskMapAttachment, error) {
	rows, err := s.boundary.Query(ctx, `SELECT latest.task_id, latest.state, r.id, r.title
FROM (
	SELECT DISTINCT ON (e.task_id, e.payload_json->>'id')
		e.task_id, t.state, e.payload_json->>'id' AS requirement_id, e.kind
	FROM tasks t
	JOIN events e ON e.task_id=t.id
	WHERE t.workspace_id=$1
	  AND e.kind IN ('task.context_requirement_added','task.context_requirement_removed')
	ORDER BY e.task_id, e.payload_json->>'id', e.at DESC, e.id DESC
) latest
JOIN requirements r ON r.workspace_id=$1 AND r.id=latest.requirement_id AND COALESCE(r.current_version,0)>0
WHERE latest.kind='task.context_requirement_added'
ORDER BY latest.task_id, r.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.TaskMapAttachment
	for rows.Next() {
		var attachment store.TaskMapAttachment
		var state string
		if err = rows.Scan(&attachment.TaskID, &state, &attachment.RequirementID, &attachment.RequirementTitle); err != nil {
			return nil, err
		}
		attachment.TaskState = core.TaskState(state)
		out = append(out, attachment)
	}
	return out, rows.Err()
}

// ListNextActionEvents reads only the assembler's event kinds for the
// requested tasks in one query, in the (at, id) order ListEvents returns.
func (s *Store) ListNextActionEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	out := map[string][]core.Event{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT e.id, e.task_id, e.job_id, e.kind, e.actor_id, e.actor_role, e.payload_json, e.at, e.workspace_id
FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE t.workspace_id = $1 AND e.task_id = ANY($2::text[]) AND e.kind = ANY($3::text[])
ORDER BY e.task_id, e.at, e.id`, workspace(ctx), taskIDs, store.NextActionEventKinds)
	if err != nil {
		return nil, err
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByName[db.Event])
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		converted := eventFromDB(event)
		out[converted.TaskID] = append(out[converted.TaskID], converted)
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
	rows, err := s.boundary.Query(ctx, `SELECT s.task_id, max(s.version)::int
FROM task_specs s
JOIN tasks t ON t.id = s.task_id
WHERE t.workspace_id = $1 AND s.task_id = ANY($2::text[])
GROUP BY s.task_id`, workspace(ctx), taskIDs)
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
	rows, err := s.boundary.Query(ctx, `
SELECT origin_task_id, 'system_design'::text AS tier, document_id AS id, version FROM system_design_versions
WHERE workspace_id=$1 AND origin=$2 AND origin_task_id=ANY($3::text[]) AND NOT confirmed AND NOT dismissed
UNION ALL
SELECT origin_task_id, 'requirement'::text, requirement_id, version FROM requirement_versions
WHERE workspace_id=$1 AND origin=$4 AND origin_task_id=ANY($3::text[]) AND NOT confirmed AND NOT retired`,
		workspace(ctx), string(core.SystemDesignOriginImplementation), taskIDs, string(core.RequirementOriginImplementation))
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
	rows, err := s.boundary.Query(ctx, `SELECT id, title, state FROM tasks WHERE workspace_id=$1 AND id=ANY($2::text[])`, workspace(ctx), taskIDs)
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

// positiveInt64SQL accepts a JSON number token that Go decodes into a
// positive int: digits alone, without fraction or exponent, at most
// 9223372036854775807.
func positiveInt64SQL(text string) string {
	return `(` + text + ` ~ '^[1-9][0-9]*$' AND (length(` + text + `) < 19 OR (length(` + text + `) = 19 AND ` + text + ` COLLATE "C" <= '9223372036854775807')))`
}

// int64SQL accepts a JSON number token that Go decodes into an int.
func int64SQL(text string) string {
	return `((` + text + ` ~ '^[0-9]+$' AND (length(` + text + `) < 19 OR (length(` + text + `) = 19 AND ` + text + ` COLLATE "C" <= '9223372036854775807')))` +
		` OR (` + text + ` ~ '^-[0-9]+$' AND (length(` + text + `) < 20 OR (length(` + text + `) = 20 AND ` + text + ` COLLATE "C" <= '-9223372036854775808'))))`
}

// retryTimePattern is the shape time.Time's JSON decoder accepts in Go 1.24,
// whose RFC 3339 parse falls back to the lenient time.Parse: a quoted
// four-digit year, two-digit month and day, a one- or two-digit hour, two-digit
// minute and second, an optional '.' or ',' fraction, and Z or a +hh:mm or
// -hh:mm offset. retryAcceptedSQL adds the decoder's range checks.
const retryTimePattern = `^"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{1,2}:[0-9]{2}:[0-9]{2}([.,][0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})"$`

// retryAcceptedSQL reports whether the JSON text raw of a failure's
// next_retry_at decodes: missing, null, or a string of retryTimePattern whose
// month is 1-12, day 1 to the month's length (Gregorian leap years), hour
// below 24, minute and second below 60, and offset hour and minute at most 24
// and 60. hp is the hour's digit count.
const retryAcceptedSQL = `(raw IS NULL OR raw = 'null' OR (CASE WHEN hp IS NULL THEN false ELSE
	substr(raw, 7, 2)::int BETWEEN 1 AND 12
	AND substr(raw, 10, 2)::int BETWEEN 1 AND (CASE substr(raw, 7, 2)::int
		WHEN 2 THEN CASE WHEN substr(raw, 2, 4)::int % 4 = 0 AND (substr(raw, 2, 4)::int % 100 <> 0 OR substr(raw, 2, 4)::int % 400 = 0) THEN 29 ELSE 28 END
		WHEN 4 THEN 30 WHEN 6 THEN 30 WHEN 9 THEN 30 WHEN 11 THEN 30 ELSE 31 END)
	AND substr(raw, 13, hp)::int < 24
	AND substr(raw, 14 + hp, 2)::int < 60
	AND substr(raw, 17 + hp, 2)::int < 60
	AND (right(raw, 2) = 'Z"' OR (substr(raw, length(raw) - 5, 2)::int <= 24 AND substr(raw, length(raw) - 2, 2)::int <= 60))
END))`

// retryNormalizedSQL keeps at most nine fraction digits of an accepted retry
// time, so the transferred value is at most 37 bytes.
const retryNormalizedSQL = `left(raw, 18 + hp) || left(substr(raw, 19 + hp, length(raw) - 18 - hp - CASE WHEN right(raw, 2) = 'Z"' THEN 2 ELSE 7 END), 10) || CASE WHEN right(raw, 2) = 'Z"' THEN 'Z"' ELSE right(raw, 7) END`

// mergeConflictSummarySQL computes store.MergeConflictHistory per task in
// SQL: one fixed-size summary row for a decodable latest merge.blocked (clear
// and exhaustion flags, the accepted-failure count, and the normalized retry
// time of the last effective step) and one row per task order a same-heads
// dispatch named inside an open episode.
var mergeConflictSummarySQL = `WITH m AS (
	SELECT e.task_id, e.kind, e.payload_json AS p,
		row_number() OVER (PARTITION BY e.task_id ORDER BY e.at, e.id) AS rn,
		(jsonb_typeof(e.payload_json) IN ('object','null')
			AND COALESCE(jsonb_typeof(e.payload_json->'approved_head'),'null') IN ('string','null')
			AND COALESCE(jsonb_typeof(e.payload_json->'new_head'),'null') IN ('string','null')) AS decodes,
		COALESCE(e.payload_json->>'approved_head','') AS approved_head,
		COALESCE(e.payload_json->>'new_head','') AS new_head
	FROM events e
	JOIN tasks t ON t.id = e.task_id
	WHERE t.workspace_id = $1 AND e.task_id = ANY($2::text[]) AND e.kind = ANY($3::text[])
), r AS (
	SELECT m.*, max(CASE WHEN kind IN ('merge.blocked','merge.conflict_cleared') THEN rn END)
		OVER (PARTITION BY task_id ORDER BY rn ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS reset_rn
	FROM m
), l AS (
	SELECT DISTINCT ON (task_id) task_id, rn, decodes, approved_head, new_head
	FROM m WHERE kind = 'merge.blocked' ORDER BY task_id, rn DESC
), cleared AS (
	SELECT task_id, max(rn) AS rn FROM m WHERE kind = 'merge.conflict_cleared' GROUP BY task_id
), s AS (
	SELECT l.task_id, l.rn, l.approved_head, l.new_head, COALESCE(cleared.rn > l.rn, false) AS cleared
	FROM l LEFT JOIN cleared ON cleared.task_id = l.task_id
	WHERE l.decodes
), same AS (
	SELECT s.task_id, s.cleared, x.kind, x.rn, x.p, x.reset_rn, s.rn AS latest_rn
	FROM s JOIN r x ON x.task_id = s.task_id AND x.decodes AND x.approved_head = s.approved_head AND x.new_head = s.new_head
), exhausted AS (
	SELECT DISTINCT task_id FROM same WHERE kind = 'merge.conflict_dispatch_exhausted' AND rn > latest_rn
), steps AS (
	SELECT task_id, rn, kind, raw, CASE WHEN raw ~ '` + retryTimePattern + `' THEN strpos(substr(raw, 13), ':') - 1 END AS hp
	FROM (
		SELECT task_id, rn, kind, CASE WHEN kind = 'merge.conflict_dispatch_failed' THEN (p->'next_retry_at')::text END AS raw
		FROM same
		WHERE NOT cleared AND rn > latest_rn AND kind IN ('merge.conflict_fix_dispatched','merge.conflict_dispatch_failed')
	) episode
), effective AS (
	SELECT task_id, rn, kind, raw, hp FROM steps
	WHERE kind = 'merge.conflict_fix_dispatched' OR ` + retryAcceptedSQL + `
), failures AS (
	SELECT task_id, count(*) AS n FROM effective WHERE kind = 'merge.conflict_dispatch_failed' GROUP BY task_id
), last AS (
	SELECT DISTINCT ON (task_id) task_id,
		CASE WHEN kind = 'merge.conflict_dispatch_failed' AND hp IS NOT NULL THEN ` + retryNormalizedSQL + ` END AS retry
	FROM effective ORDER BY task_id, rn DESC
)
SELECT 0 AS row_kind, s.task_id, s.cleared, (NOT s.cleared AND exhausted.task_id IS NOT NULL) AS exhausted,
	COALESCE(failures.n, 0)::bigint AS failures,
	last.retry,
	'' AS value
FROM s
LEFT JOIN exhausted ON exhausted.task_id = s.task_id
LEFT JOIN failures ON failures.task_id = s.task_id
LEFT JOIN last ON last.task_id = s.task_id
UNION ALL
SELECT DISTINCT 2, x.task_id, false, false, 0::bigint, NULL::text, x.p->>'work_order_id'
FROM same x
JOIN same b ON b.task_id = x.task_id AND b.rn = x.reset_rn AND b.kind = 'merge.blocked'
JOIN work_orders wo ON wo.workspace_id = $1 AND wo.task_id = x.task_id AND wo.id = x.p->>'work_order_id'
WHERE x.kind = 'merge.conflict_fix_dispatched' AND jsonb_typeof(x.p->'work_order_id') = 'string'`

// latestHistorySQL selects, per task and kind, the latest row each
// park-command, pull-request, and bounce fold accepts, and transfers only a
// typed value: the command's first 64 characters, the number's digits, or the
// window's digits when the fold accepts it and 0 otherwise.
var latestHistorySQL = `SELECT DISTINCT ON (e.task_id, e.kind) e.task_id, e.kind,
	CASE
		WHEN e.kind = 'task.state_changed' THEN left(e.payload_json->>'command', 64)
		WHEN e.kind = 'pull_request.opened' THEN (e.payload_json->'number')::text
		WHEN jsonb_typeof(e.payload_json) IN ('object','null') AND ` + positiveInt64SQL(`(e.payload_json->'window')::text`) + ` THEN (e.payload_json->'window')::text
		ELSE '0'
	END AS value
FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE t.workspace_id = $1 AND e.task_id = ANY($2::text[]) AND (
	(e.kind = 'task.state_changed' AND jsonb_typeof(e.payload_json->'command') = 'string' AND e.payload_json->>'command' <> '')
	OR (e.kind = 'pull_request.opened' AND ` + positiveInt64SQL(`(e.payload_json->'number')::text`) + `)
	OR e.kind = 'pipeline.bounce_limit')
ORDER BY e.task_id, e.kind, e.at DESC, e.id DESC`

// ListNextActionHistory computes store.NextActionHistory in two queries:
// the typed latest values of the park-command, pull-request, and bounce
// folds, and the fixed-size merge-conflict summary (component-persistence).
func (s *Store) ListNextActionHistory(ctx context.Context, taskIDs []string) (map[string]store.NextActionHistory, error) {
	out := map[string]store.NextActionHistory{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, latestHistorySQL, workspace(ctx), taskIDs)
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
	if rows, err = s.boundary.Query(ctx, mergeConflictSummarySQL, workspace(ctx), taskIDs, store.MergeConflictEventKinds); err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rowKind int
		var taskID, value string
		var cleared, exhausted bool
		var failures int64
		var retry *string
		if err = rows.Scan(&rowKind, &taskID, &cleared, &exhausted, &failures, &retry, &value); err != nil {
			return nil, err
		}
		history := out[taskID]
		if rowKind == 0 {
			history.MergeConflict = store.MergeConflictHistory{Open: true, Cleared: cleared, Exhausted: exhausted, Failures: int(failures), DispatchedOrderIDs: history.MergeConflict.DispatchedOrderIDs}
			if retry != nil {
				history.MergeConflict.NextRetryAt = json.RawMessage(*retry)
			}
		} else {
			history.MergeConflict.DispatchedOrderIDs = append(history.MergeConflict.DispatchedOrderIDs, value)
		}
		out[taskID] = history
	}
	if err = rows.Err(); err != nil {
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
	rows, err := s.boundary.Query(ctx, `SELECT t.id, t.title, t.state FROM tasks t
WHERE t.workspace_id=$1 AND t.state NOT IN ('merged','closed') AND EXISTS (
	SELECT 1 FROM task_dependencies edge
	WHERE edge.workspace_id=t.workspace_id AND edge.task_id=t.id AND edge.depends_on_task_id=ANY($2::text[])
)
ORDER BY t.id`, workspace(ctx), taskIDs)
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

// designPinsSQL selects the latest decodable design add or remove per task
// and design ID and keeps the adds, with the shapes ActiveTaskContextReferences
// decodes: an object or null payload, a string, null, or missing id, and a
// null, missing, or int version.
var designPinsSQL = `SELECT task_id, design_id, version FROM (
	SELECT DISTINCT ON (e.task_id, COALESCE(e.payload_json->>'id',''))
		e.task_id, COALESCE(e.payload_json->>'id','') AS design_id, e.kind,
		CASE WHEN jsonb_typeof(e.payload_json->'version') = 'number' THEN (e.payload_json->>'version')::bigint ELSE 0 END AS version
	FROM events e
	JOIN tasks t ON t.id = e.task_id
	WHERE t.workspace_id = $1 AND e.task_id = ANY($2::text[]) AND e.kind IN ($3, $4)
		AND COALESCE(e.payload_json->>'id','') = ANY($5::text[])
		AND jsonb_typeof(e.payload_json) IN ('object','null')
		AND COALESCE(jsonb_typeof(e.payload_json->'id'),'null') IN ('string','null')
		AND (COALESCE(jsonb_typeof(e.payload_json->'version'),'null') = 'null' OR ` + int64SQL(`(e.payload_json->'version')::text`) + `)
	ORDER BY e.task_id, COALESCE(e.payload_json->>'id',''), e.at DESC, e.id DESC
) latest
WHERE kind = $3`

// ListTaskContextDesignPins selects, in one query, the latest decodable
// design add or remove per task and design ID and keeps the adds, so only
// current pins cross the boundary (component-persistence).
func (s *Store) ListTaskContextDesignPins(ctx context.Context, taskIDs, documentIDs []string) (map[string]map[string]int, error) {
	out := map[string]map[string]int{}
	if len(taskIDs) == 0 || len(documentIDs) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, designPinsSQL, workspace(ctx), taskIDs, store.TaskContextDesignAdded, store.TaskContextDesignRemoved, documentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, designID string
		var version int64
		if err = rows.Scan(&taskID, &designID, &version); err != nil {
			return nil, err
		}
		if out[taskID] == nil {
			out[taskID] = map[string]int{}
		}
		out[taskID][designID] = int(version)
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
	rows, err := s.boundary.Query(ctx, `SELECT id, 'requirement'::text AS tier, COALESCE(current_version,0)::int, archived_at IS NOT NULL FROM requirements
WHERE workspace_id=$1 AND id=ANY($2::text[])
UNION ALL
SELECT id, 'system_design'::text, COALESCE(current_version,0)::int, archived_at IS NOT NULL FROM system_designs
WHERE workspace_id=$1 AND id=ANY($2::text[])`, workspace(ctx), ids)
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
	taskIDs := make([]string, 0, len(refs))
	orderIDs := make([]string, 0, len(refs))
	for _, ref := range refs {
		if !slices.Contains(taskIDs, ref.TaskID) {
			taskIDs = append(taskIDs, ref.TaskID)
		}
		orderIDs = append(orderIDs, ref.OrderID)
	}
	err := s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		ws := workspace(ctx)
		byTask := map[string][]db.VerificationRecord{}
		for _, table := range store.VerificationTables {
			condition := "task_id=ANY($2::text[])"
			if table == "verification_operations" {
				condition = "(task_id=ANY($2::text[]) OR true)"
			}
			rows, err := tx.Query(ctx, "SELECT id,task_id,context_id,run_id,logical_key,key_hash,state,body,expires_at FROM "+table+" WHERE workspace_id=$1 AND "+condition+" ORDER BY id", ws, taskIDs)
			if err != nil {
				return err
			}
			var shared []db.VerificationRecord
			for rows.Next() {
				v := db.VerificationRecord{Table: table, WorkspaceID: ws}
				if err = rows.Scan(&v.ID, &v.TaskID, &v.ContextID, &v.RunID, &v.LogicalKey, &v.KeyHash, &v.State, &v.Body, &v.ExpiresAt); err != nil {
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
		rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=ANY($2::text[])", ws, orderIDs)
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
			out[ref.OrderID] = store.VerificationReviewState{Rows: verificationRows(byTask[ref.TaskID]), Order: order}
		}
		return nil
	})
	return out, err
}
