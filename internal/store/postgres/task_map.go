package postgres

import (
	"context"
	"fmt"

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

// ListTaskMap reads the workspace task map in five set-based queries: task
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
