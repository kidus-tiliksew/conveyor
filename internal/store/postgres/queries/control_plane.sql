-- name: InsertWorkspace :one
INSERT INTO workspaces (id, name, config_yaml)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetWorkspaceConfig :one
SELECT * FROM workspaces WHERE id = $1;

-- name: UpdateWorkspaceConfig :one
UPDATE workspaces
SET config_yaml = sqlc.arg(config_yaml),
    config_version = config_version + 1
WHERE id = sqlc.arg(id)
  AND config_version = sqlc.arg(expected_version)
RETURNING *;

-- name: UpsertRepo :exec
INSERT INTO repos (workspace_id, name, url, github_slug, default_base, devcontainer_path)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (workspace_id, name) DO UPDATE
SET url = EXCLUDED.url,
    github_slug = EXCLUDED.github_slug,
    default_base = EXCLUDED.default_base,
    devcontainer_path = EXCLUDED.devcontainer_path;

-- name: InsertTask :one
INSERT INTO tasks (
    id, workspace_id, source, title, body, class, escalation_level, mode, hold, spec_approval, merge_approval, policy_version,
    setup_name, setup_contract, reviewed_head_sha, approved_head_sha, approval_stale,
    refresh_baseline_sha, refresh_head_sha, refresh_review_scope,
    repo_name, base_branch, branch, state, next_stage, recovery_stage, parent_task_id,
    origin_spec_version, origin_sub_id, feature_id, intake_key, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18, $19, $20,
    $21, $22, $23, $24, $25, $26, sqlc.narg(parent_task_id),
    sqlc.arg(origin_spec_version), sqlc.arg(origin_sub_id),
    sqlc.arg(feature_id), sqlc.arg(intake_key), sqlc.arg(created_at)
)
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1 AND workspace_id = $2;

-- name: GetTaskByIntakeKey :one
SELECT * FROM tasks WHERE workspace_id = $1 AND intake_key = $2;

-- name: ListTasks :many
SELECT sqlc.embed(t),
       EXISTS (
           SELECT 1 FROM task_dependencies edge
           WHERE edge.workspace_id = t.workspace_id AND edge.task_id = t.id
       ) AS has_dependencies,
       EXISTS (
           SELECT 1 FROM tasks child
           WHERE child.workspace_id = t.workspace_id AND child.parent_task_id = t.id
       ) AS has_children
FROM tasks t
WHERE t.workspace_id = $1
ORDER BY t.created_at DESC, t.id;

-- name: ListCheckpointContextCandidates :many
SELECT t.id, t.title, t.state
FROM tasks t
JOIN LATERAL (
    SELECT w.state, w.last_attempt_outcome, w.last_failure_message
    FROM work_orders w
    WHERE w.workspace_id = t.workspace_id AND w.task_id = t.id
    ORDER BY w.created_at DESC, w.id DESC
    LIMIT 1
) latest ON true
WHERE t.workspace_id = sqlc.arg(workspace_id)
  AND t.state NOT IN ('merged', 'closed')
  AND latest.state = 'queued'
  AND latest.last_attempt_outcome = 'released'
  AND latest.last_failure_message = 'operator checkpoint reached'
  AND COALESCE((
      SELECT e.kind
      FROM events e
      WHERE e.workspace_id = t.workspace_id
        AND e.task_id = t.id
        AND e.kind IN ('task.context_requirement_added', 'task.context_requirement_removed')
        AND e.payload_json ->> 'id' = sqlc.arg(requirement_id)::text
      ORDER BY e.id DESC
      LIMIT 1
  ), '') <> 'task.context_requirement_added'
ORDER BY t.id;

-- The shared Tasks/Board filter (AC-2.4) is evaluated here rather than in Go, so
-- neither surface ever loads the workspace to narrow it (AC-2.3). Each member
-- short-circuits on its own empty argument, leaving the unfiltered plan as it
-- was. The state, repository, requirement, and design members take arrays — a
-- task matches on any listed value, and an empty array leaves that member
-- inactive — mirroring the memory store's slice predicate exactly.
-- `strpos(lower(...))` is a literal case-insensitive substring test, so an
-- operator typing `%` or `_` searches for that character instead of a wildcard.
-- The created-at bounds compare the task's persisted creation instant directly;
-- later events cannot change whether the task matches. The requirement and design bounds take the latest add or
-- remove per listed document, which is the SQL spelling of the
-- store.ActiveTaskContextReferences fold, over migration 073's
-- events_task_context_task_idx.

-- name: ListTaskOperationsTasks :many
SELECT sqlc.embed(t),
       EXISTS (
           SELECT 1 FROM task_dependencies edge
           WHERE edge.workspace_id = t.workspace_id AND edge.task_id = t.id
       ) AS has_dependencies,
       EXISTS (
           SELECT 1 FROM tasks child
           WHERE child.workspace_id = t.workspace_id AND child.parent_task_id = t.id
       ) AS has_children
FROM tasks t
WHERE t.workspace_id = sqlc.arg(workspace_id)
  AND (cardinality(sqlc.arg(task_states)::text[]) = 0 OR t.state = ANY(sqlc.arg(task_states)::text[]))
  AND (cardinality(sqlc.arg(repositories)::text[]) = 0 OR t.repo_name = ANY(sqlc.arg(repositories)::text[]))
  AND (sqlc.arg(search)::text = '' OR
       strpos(lower(t.title), lower(sqlc.arg(search))) > 0 OR
       strpos(lower(t.id), lower(sqlc.arg(search))) > 0 OR
       strpos(lower(t.source), lower(sqlc.arg(search))) > 0 OR
       strpos(lower(t.branch), lower(sqlc.arg(search))) > 0)
  AND (sqlc.narg(created_from)::timestamptz IS NULL OR t.created_at >= sqlc.narg(created_from))
  AND (sqlc.narg(created_to)::timestamptz IS NULL OR t.created_at < sqlc.narg(created_to))
  AND (cardinality(sqlc.arg(serves_requirements)::text[]) = 0 OR EXISTS (
           SELECT 1 FROM unnest(sqlc.arg(serves_requirements)::text[]) AS wanted(document_id)
           WHERE (
               SELECT e.kind FROM events e
               WHERE e.workspace_id = t.workspace_id AND e.task_id = t.id
                 AND e.kind IN ('task.context_requirement_added', 'task.context_requirement_removed')
                 AND e.payload_json ->> 'id' = wanted.document_id
               ORDER BY e.id DESC LIMIT 1
           ) = 'task.context_requirement_added'
       ))
  AND (cardinality(sqlc.arg(governing_designs)::text[]) = 0 OR EXISTS (
           SELECT 1 FROM unnest(sqlc.arg(governing_designs)::text[]) AS wanted(document_id)
           WHERE (
               SELECT e.kind FROM events e
               WHERE e.workspace_id = t.workspace_id AND e.task_id = t.id
                 AND e.kind IN ('task.context_design_added', 'task.context_design_removed')
                 AND e.payload_json ->> 'id' = wanted.document_id
               ORDER BY e.id DESC LIMIT 1
           ) = 'task.context_design_added'
       ))
ORDER BY t.created_at DESC, t.id
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0)
OFFSET sqlc.arg(page_offset)::int;

-- name: CountTaskOperationsTasks :one
SELECT count(*)::bigint
FROM tasks t
WHERE t.workspace_id = sqlc.arg(workspace_id)
  AND (cardinality(sqlc.arg(task_states)::text[]) = 0 OR t.state = ANY(sqlc.arg(task_states)::text[]))
  AND (cardinality(sqlc.arg(repositories)::text[]) = 0 OR t.repo_name = ANY(sqlc.arg(repositories)::text[]))
  AND (sqlc.arg(search)::text = '' OR
       strpos(lower(t.title), lower(sqlc.arg(search))) > 0 OR
       strpos(lower(t.id), lower(sqlc.arg(search))) > 0 OR
       strpos(lower(t.source), lower(sqlc.arg(search))) > 0 OR
       strpos(lower(t.branch), lower(sqlc.arg(search))) > 0)
  AND (sqlc.narg(created_from)::timestamptz IS NULL OR t.created_at >= sqlc.narg(created_from))
  AND (sqlc.narg(created_to)::timestamptz IS NULL OR t.created_at < sqlc.narg(created_to))
  AND (cardinality(sqlc.arg(serves_requirements)::text[]) = 0 OR EXISTS (
           SELECT 1 FROM unnest(sqlc.arg(serves_requirements)::text[]) AS wanted(document_id)
           WHERE (
               SELECT e.kind FROM events e
               WHERE e.workspace_id = t.workspace_id AND e.task_id = t.id
                 AND e.kind IN ('task.context_requirement_added', 'task.context_requirement_removed')
                 AND e.payload_json ->> 'id' = wanted.document_id
               ORDER BY e.id DESC LIMIT 1
           ) = 'task.context_requirement_added'
       ))
  AND (cardinality(sqlc.arg(governing_designs)::text[]) = 0 OR EXISTS (
           SELECT 1 FROM unnest(sqlc.arg(governing_designs)::text[]) AS wanted(document_id)
           WHERE (
               SELECT e.kind FROM events e
               WHERE e.workspace_id = t.workspace_id AND e.task_id = t.id
                 AND e.kind IN ('task.context_design_added', 'task.context_design_removed')
                 AND e.payload_json ->> 'id' = wanted.document_id
               ORDER BY e.id DESC LIMIT 1
           ) = 'task.context_design_added'
       ));

-- name: ListTaskOperationsEvents :many
SELECT e.id, e.task_id, e.job_id, e.kind, e.actor_id, e.actor_role, e.payload_json, e.at, e.workspace_id
FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE t.workspace_id = sqlc.arg(workspace_id)
  AND e.task_id = ANY(sqlc.arg(task_ids)::text[])
  AND e.kind IN (
    'task.context_requirement_added', 'task.context_requirement_removed',
    'task.context_design_added', 'task.context_design_removed',
    'task.state_changed'
  )
ORDER BY e.task_id, e.at, e.id;

-- name: ListTaskOperationsLatestPlans :many
SELECT DISTINCT ON (s.task_id) s.*
FROM task_specs s
JOIN tasks t ON t.id = s.task_id
WHERE t.workspace_id = sqlc.arg(workspace_id)
  AND s.task_id = ANY(sqlc.arg(task_ids)::text[])
ORDER BY s.task_id, s.version DESC;

-- name: UpdateTaskState :one
UPDATE tasks
SET state = sqlc.arg(state), updated_at = now()
WHERE id = sqlc.arg(id)
  AND workspace_id = sqlc.arg(workspace_id)
RETURNING *;

-- name: UpdateTaskHold :one
UPDATE tasks
SET hold = sqlc.arg(hold), updated_at = now()
WHERE id = sqlc.arg(id)
  AND workspace_id = sqlc.arg(workspace_id)
RETURNING *;

-- name: BindTaskApproval :one
UPDATE tasks
SET reviewed_head_sha = sqlc.arg(head_sha),
    approved_head_sha = sqlc.arg(head_sha),
    approval_stale = false,
    refresh_baseline_sha = '', refresh_head_sha = '', refresh_review_scope = '',
    updated_at = now()
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
RETURNING *;

-- name: MarkTaskApprovalStale :one
UPDATE tasks
SET approved_head_sha = sqlc.arg(approved_head_sha), approval_stale = true,
    refresh_baseline_sha = sqlc.arg(approved_head_sha),
    refresh_head_sha = sqlc.arg(new_head_sha),
    refresh_review_scope = sqlc.arg(refresh_review_scope), updated_at = now()
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
  AND NOT (approval_stale
    AND refresh_baseline_sha = sqlc.arg(approved_head_sha)
    AND refresh_head_sha = sqlc.arg(new_head_sha))
RETURNING *;

-- name: SkipTaskRefresh :one
UPDATE tasks
SET reviewed_head_sha = sqlc.arg(head_sha), approved_head_sha = sqlc.arg(head_sha),
    approval_stale = false, refresh_baseline_sha = '', refresh_head_sha = '',
    refresh_review_scope = '', updated_at = now()
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
RETURNING *;

-- name: UpdateTaskTransition :one
UPDATE tasks
SET state = sqlc.arg(state),
    next_stage = sqlc.arg(next_stage),
    recovery_stage = sqlc.arg(recovery_stage),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND workspace_id = sqlc.arg(workspace_id)
RETURNING *;

-- name: UpdateTaskClassification :one
UPDATE tasks
SET class = sqlc.arg(class), updated_at = now()
WHERE id = sqlc.arg(id)
  AND workspace_id = sqlc.arg(workspace_id)
RETURNING *;

-- name: InsertJob :one
INSERT INTO jobs (
    id, task_id, stage, harness, model_tier, auth_mode, runner,
    pack_version, confinement_tier, cost_usd, tokens_in,
    tokens_out, state, started_at, ended_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14, $15
)
RETURNING *;

-- name: UpdateJob :one
UPDATE jobs
SET stage = $2,
    harness = $3,
    model_tier = $4,
    auth_mode = $5,
    runner = $6,
    pack_version = $7,
    confinement_tier = $8,
    cost_usd = $9,
    tokens_in = $10,
    tokens_out = $11,
    state = $12,
    started_at = $13,
    ended_at = $14,
    updated_at = now()
WHERE jobs.id = $1
  AND EXISTS (
      SELECT 1 FROM tasks t
      WHERE t.id = jobs.task_id AND t.workspace_id = $15
  )
RETURNING jobs.*;

-- name: GetJob :one
SELECT j.* FROM jobs j
JOIN tasks t ON t.id = j.task_id
WHERE j.id = $1 AND t.workspace_id = $2;

-- name: ListJobs :many
SELECT j.* FROM jobs j
JOIN tasks t ON t.id = j.task_id
LEFT JOIN work_orders wo ON wo.job_id = j.id
WHERE j.task_id = $1 AND t.workspace_id = $2
ORDER BY COALESCE(j.started_at, wo.queue_entered_at), j.id;

-- name: GetLatestJob :one
SELECT j.* FROM jobs j
JOIN tasks t ON t.id = j.task_id
LEFT JOIN work_orders wo ON wo.job_id = j.id
WHERE j.task_id = $1 AND t.workspace_id = $2
ORDER BY COALESCE(j.started_at, wo.queue_entered_at) DESC, j.id DESC
LIMIT 1;

-- name: InsertEvent :one
INSERT INTO events (workspace_id, task_id, job_id, kind, actor_id, actor_role, payload_json, at)
SELECT t.workspace_id, sqlc.arg(task_id), sqlc.arg(job_id), sqlc.arg(kind),
       sqlc.arg(actor_id), sqlc.arg(actor_role), sqlc.arg(payload_json), sqlc.arg(at)
FROM tasks t
WHERE t.id = sqlc.arg(task_id)
RETURNING *;

-- name: InsertWorkspaceEvent :one
INSERT INTO events (workspace_id, task_id, job_id, kind, actor_id, actor_role, payload_json, at)
VALUES ($1, NULL, NULL, $2, $3, $4, $5, $6)
RETURNING *;

-- name: InsertLineageLink :exec
INSERT INTO links (
    workspace_id, src_type, src_id, dst_type, dst_id, kind,
    created_by_event_id, created_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (workspace_id, src_type, src_id, dst_type, dst_id, kind)
DO UPDATE SET
	created_by_event_id = LEAST(COALESCE(links.created_by_event_id, EXCLUDED.created_by_event_id), EXCLUDED.created_by_event_id),
    created_at = LEAST(links.created_at, EXCLUDED.created_at),
    legacy_created_by_event = NULL;

-- name: DeleteLineageLink :exec
DELETE FROM links
WHERE workspace_id = $1
  AND src_type = $2
  AND src_id = $3
  AND dst_type = $4
  AND dst_id = $5
  AND kind = $6;

-- name: ListLineageLinks :many
SELECT * FROM links
WHERE workspace_id = $1
ORDER BY created_by_event_id, src_type, src_id, dst_type, dst_id, kind;

-- name: DeleteLineageLinks :execrows
-- Canonical directions include planning_session -consulted-> reference_document_version,
-- requirement_version -derived_from-> reference_document_version,
-- system_design_version -governs-> repository_path,
-- system_design_version -proposed_by-> task/planning_session, and
-- planning_session -produced_design-> system_design,
-- planning_session -produced_bundle-> planning_bundle,
-- planning_bundle -proposes-> requirement_version/system_design_version/decision,
-- and planning_bundle -creates-> task.
DELETE FROM links WHERE workspace_id = $1
  AND created_by_event_id IS NOT NULL
  AND kind = ANY(ARRAY[
    'consulted','depends_on','derived_from','dispatches','materializes','merged_range','produced_blueprint',
    'produced_requirement','produced_verdict','serves','submitted_as','submitted_range',
    'supersedes','supports','versions','governs','proposed_by','produced_design','produced_bundle','proposes','creates'
  ]::text[]);

-- name: ListWorkspaceEvents :many
SELECT * FROM events
WHERE workspace_id = $1
ORDER BY id;

-- name: ListEvents :many
SELECT e.* FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE e.task_id = $1 AND t.workspace_id = $2
ORDER BY e.at, e.id;

-- name: ListRequirementEvents :many
SELECT e.* FROM events e
WHERE e.workspace_id = sqlc.arg(workspace_id)
  AND e.task_id IS NULL
  AND e.payload_json->>'requirement_id' = sqlc.arg(requirement_id)::text
ORDER BY e.at, e.id;

-- name: ListEventsAfter :many
SELECT e.* FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE e.task_id = $1 AND t.workspace_id = $2 AND e.id > $3
ORDER BY e.id;

-- name: CountEvents :one
SELECT count(*)::bigint FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE e.task_id = $1 AND e.kind = $2 AND t.workspace_id = $3;

-- name: CountEventsSinceHumanIntervention :one
-- The check-in window: events of a kind recorded after the latest
-- human intervention on the task (all of them when no human has intervened).
SELECT count(*)::bigint FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE e.task_id = $1 AND e.kind = $2 AND t.workspace_id = $3
  AND e.at > COALESCE((
      SELECT max(i.at) FROM interventions i
      WHERE i.task_id = $1 AND i.actor_role = 'human'
  ), '-infinity'::timestamptz);

-- name: ListActivityMarkers :many
SELECT
    t.id AS task_id,
    COALESCE((
        SELECT j.stage FROM jobs j
        WHERE j.task_id = t.id
        ORDER BY j.started_at DESC, j.id DESC
        LIMIT 1
    ), '')::text AS latest_stage,
    COALESCE((
        SELECT e.at FROM events e
        WHERE e.task_id = t.id
        ORDER BY e.id DESC
        LIMIT 1
    ), t.created_at)::timestamptz AS last_event_at
FROM tasks t
WHERE t.workspace_id = $1
ORDER BY t.created_at DESC, t.id;

-- name: InsertIntervention :one
INSERT INTO interventions (
    task_id, job_id, actor_id, actor_role, action, reason_code, comment, at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListInterventions :many
SELECT i.* FROM interventions i
JOIN tasks t ON t.id = i.task_id
WHERE i.task_id = $1 AND t.workspace_id = $2
ORDER BY i.at, i.id;

-- name: UpsertTranscript :one
INSERT INTO transcripts (job_id, uri, redaction_stats, created_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (job_id) DO UPDATE
SET uri = EXCLUDED.uri,
    redaction_stats = EXCLUDED.redaction_stats
RETURNING *;

-- name: GetTranscript :one
SELECT tr.* FROM transcripts tr
JOIN jobs j ON j.id = tr.job_id
JOIN tasks t ON t.id = j.task_id
WHERE tr.job_id = $1 AND t.workspace_id = $2;

-- name: InsertSpecVersion :one
INSERT INTO task_specs (
    task_id, version, content, acceptance_count, acceptance, decomposition,
    approved, approved_at, created_at, agent, model
)
SELECT
    sqlc.arg(task_id),
    COALESCE(MAX(version), 0) + 1,
    sqlc.arg(content),
    sqlc.arg(acceptance_count),
    sqlc.arg(acceptance),
    sqlc.arg(decomposition),
    false,
    NULL,
    sqlc.arg(created_at),
    sqlc.arg(agent),
    sqlc.arg(model)
FROM task_specs
WHERE task_id = sqlc.arg(task_id)
RETURNING *;

-- name: GetLatestSpecVersion :one
SELECT s.* FROM task_specs s
JOIN tasks t ON t.id = s.task_id
WHERE s.task_id = $1 AND t.workspace_id = $2
ORDER BY s.version DESC
LIMIT 1;

-- name: ApproveLatestSpecVersion :one
UPDATE task_specs s
SET approved = true, approved_at = now()
FROM tasks t
WHERE s.task_id = t.id
  AND s.task_id = sqlc.arg(task_id)
  AND s.version = sqlc.arg(version)
  AND t.workspace_id = sqlc.arg(workspace_id)
  AND s.version = (SELECT MAX(version) FROM task_specs WHERE task_id = sqlc.arg(task_id))
RETURNING s.*;
