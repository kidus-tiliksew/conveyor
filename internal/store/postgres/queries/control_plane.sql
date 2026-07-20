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
    id, workspace_id, source, title, body, class, escalation_level, mode, spec_approval, merge_approval, policy_version,
    setup_name, setup_contract, repo_name, base_branch, branch, state, next_stage, recovery_stage, parent_task_id, feature_id, intake_key, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
    $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23
)
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1 AND workspace_id = $2;

-- name: GetTaskByIntakeKey :one
SELECT * FROM tasks WHERE workspace_id = $1 AND intake_key = $2;

-- name: ListTasks :many
SELECT * FROM tasks WHERE workspace_id = $1 ORDER BY created_at, id;

-- name: UpdateTaskState :one
UPDATE tasks
SET state = sqlc.arg(state), updated_at = now()
WHERE id = sqlc.arg(id)
  AND workspace_id = sqlc.arg(workspace_id)
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

-- name: ListEvents :many
SELECT e.* FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE e.task_id = $1 AND t.workspace_id = $2
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
-- The §21.17 check-in window: events of a kind recorded after the latest
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
ORDER BY t.created_at, t.id;

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
    approved, approved_at, created_at
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
    sqlc.arg(created_at)
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
