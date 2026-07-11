-- name: UpsertWorkspace :exec
INSERT INTO workspaces (id, name, config_yaml)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name,
    config_yaml = EXCLUDED.config_yaml;

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
    id, workspace_id, source, title, body, class, escalation_level,
    repo_name, base_branch, branch, state, parent_task_id, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12, $13
)
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1 AND workspace_id = $2;

-- name: ListTasks :many
SELECT * FROM tasks WHERE workspace_id = $1 ORDER BY created_at, id;

-- name: UpdateTaskState :one
UPDATE tasks
SET state = sqlc.arg(state), updated_at = now()
WHERE id = sqlc.arg(id)
  AND workspace_id = sqlc.arg(workspace_id)
RETURNING *;

-- name: InsertJob :one
INSERT INTO jobs (
    id, task_id, stage, harness, model_tier, credential_id, auth_mode, runner, sandbox_ref,
    pack_version, confinement_tier, budget_usd, cost_usd, tokens_in,
    tokens_out, state, boot_diagnostics, started_at, ended_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9,
    $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19
)
RETURNING *;

-- name: UpdateJob :one
UPDATE jobs
SET stage = $2,
    harness = $3,
    model_tier = $4,
    credential_id = $5,
    auth_mode = $6,
    runner = $7,
    sandbox_ref = $8,
    pack_version = $9,
    confinement_tier = $10,
    budget_usd = $11,
    cost_usd = $12,
    tokens_in = $13,
    tokens_out = $14,
    state = $15,
    boot_diagnostics = $16,
    started_at = $17,
    ended_at = $18,
    updated_at = now()
WHERE jobs.id = $1
  AND EXISTS (
      SELECT 1 FROM tasks t
      WHERE t.id = jobs.task_id AND t.workspace_id = $19
  )
RETURNING jobs.*;

-- name: GetJob :one
SELECT j.* FROM jobs j
JOIN tasks t ON t.id = j.task_id
WHERE j.id = $1 AND t.workspace_id = $2;

-- name: ListJobs :many
SELECT j.* FROM jobs j
JOIN tasks t ON t.id = j.task_id
WHERE j.task_id = $1 AND t.workspace_id = $2
ORDER BY j.started_at, j.id;

-- name: GetLatestJob :one
SELECT j.* FROM jobs j
JOIN tasks t ON t.id = j.task_id
WHERE j.task_id = $1 AND t.workspace_id = $2
ORDER BY j.started_at DESC, j.id DESC
LIMIT 1;

-- name: InsertEvent :one
INSERT INTO events (task_id, job_id, kind, actor_id, actor_role, payload_json, at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
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

-- name: UpsertVendorPolicy :exec
INSERT INTO vendor_policies (
    vendor, harness, auth_mode, subscription_headless, reviewed_at, source_url
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (vendor, harness, auth_mode) DO UPDATE
SET subscription_headless = EXCLUDED.subscription_headless,
    reviewed_at = EXCLUDED.reviewed_at,
    source_url = EXCLUDED.source_url,
    updated_at = now();

-- name: UpsertCredential :exec
INSERT INTO credentials (
    id, owner_id, owner_kind, kind, vendor, harness, enc_ref
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE
SET owner_id = EXCLUDED.owner_id,
    owner_kind = EXCLUDED.owner_kind,
    kind = EXCLUDED.kind,
    vendor = EXCLUDED.vendor,
    harness = EXCLUDED.harness,
    enc_ref = EXCLUDED.enc_ref,
    updated_at = now();

-- name: ListWorkspaceCredentialIDs :many
SELECT credential_id
FROM workspace_credentials
WHERE workspace_id = $1
ORDER BY credential_id;

-- name: DeleteWorkspaceCredentialRefs :exec
DELETE FROM workspace_credentials WHERE workspace_id = $1;

-- name: InsertWorkspaceCredentialRef :exec
INSERT INTO workspace_credentials (workspace_id, credential_id)
VALUES ($1, $2);

-- name: EnableCredential :exec
UPDATE credentials
SET rate_limit_state = 'available', updated_at = now()
WHERE id = $1 AND rate_limit_state = 'disabled';

-- name: DisableCredentialIfUnreferenced :exec
UPDATE credentials c
SET rate_limit_state = 'disabled', updated_at = now()
WHERE c.id = $1
  AND c.rate_limit_state <> 'disabled'
  AND NOT EXISTS (
      SELECT 1 FROM workspace_credentials wc WHERE wc.credential_id = c.id
  );

-- name: ListWorkspaceVendorPolicyRefs :many
SELECT vendor, harness, auth_mode
FROM workspace_vendor_policies
WHERE workspace_id = $1
ORDER BY vendor, harness, auth_mode;

-- name: DeleteWorkspaceVendorPolicyRefs :exec
DELETE FROM workspace_vendor_policies WHERE workspace_id = $1;

-- name: InsertWorkspaceVendorPolicyRef :exec
INSERT INTO workspace_vendor_policies (workspace_id, vendor, harness, auth_mode)
VALUES ($1, $2, $3, $4);

-- name: RestrictVendorPolicyIfUnreferenced :exec
UPDATE vendor_policies p
SET subscription_headless = 'unknown', updated_at = now()
WHERE p.vendor = $1 AND p.harness = $2 AND p.auth_mode = $3
  AND p.subscription_headless <> 'unknown'
  AND NOT EXISTS (
      SELECT 1
      FROM workspace_vendor_policies wp
      WHERE wp.vendor = p.vendor
        AND wp.harness = p.harness
        AND wp.auth_mode = p.auth_mode
  );

-- name: ClaimCredential :one
WITH candidate AS (
    SELECT c.id
    FROM credentials c
    LEFT JOIN vendor_policies p
      ON p.vendor = c.vendor AND p.harness = c.harness AND p.auth_mode = c.kind
    WHERE c.harness = ANY(sqlc.arg(harnesses)::text[])
      AND (c.owner_kind = 'org' OR c.owner_id = sqlc.arg(owner_id))
      AND (c.kind = 'api' OR c.owner_kind = 'user')
      AND (c.leased_by = sqlc.arg(job_id) OR c.lease_until IS NULL OR c.lease_until <= now())
      AND (c.cooldown_until IS NULL OR c.cooldown_until <= now())
      AND c.rate_limit_state <> 'disabled'
      AND (
          c.kind = 'api'
          OR p.subscription_headless = 'allowed'
          OR (sqlc.arg(allow_restricted)::boolean AND p.subscription_headless = 'restricted')
      )
    ORDER BY array_position(sqlc.arg(harnesses)::text[], c.harness),
             CASE c.kind WHEN 'personal_sub' THEN 0 WHEN 'team_sub' THEN 1 ELSE 2 END,
             c.updated_at,
             c.id
    FOR UPDATE OF c SKIP LOCKED
    LIMIT 1
)
UPDATE credentials c
SET leased_by = sqlc.arg(job_id),
    lease_task_id = sqlc.arg(task_id),
    lease_until = now() + sqlc.arg(lease_seconds)::bigint * interval '1 second',
    rate_limit_state = 'available',
    cooldown_until = NULL,
    last_error = '',
    updated_at = now()
FROM candidate
WHERE c.id = candidate.id
RETURNING c.*;

-- name: RescueTaskCredentialLeases :execrows
UPDATE credentials
SET leased_by = '', lease_task_id = '', lease_until = NULL,
    last_error = 'rescued after abandoned task attempt', updated_at = now()
WHERE lease_task_id = sqlc.arg(task_id)
  AND leased_by <> ''
  AND leased_by <> sqlc.arg(current_job_id);

-- name: ReleaseCredential :execrows
UPDATE credentials
SET leased_by = '', lease_task_id = '', lease_until = NULL, last_error = sqlc.arg(last_error), updated_at = now()
WHERE id = sqlc.arg(id) AND leased_by = sqlc.arg(job_id);

-- name: ThrottleCredential :execrows
UPDATE credentials
SET leased_by = '',
    lease_task_id = '',
    lease_until = NULL,
    rate_limit_state = 'throttled',
    cooldown_until = now() + sqlc.arg(cooldown_seconds)::bigint * interval '1 second',
    last_error = sqlc.arg(last_error),
    updated_at = now()
WHERE id = sqlc.arg(id) AND leased_by = sqlc.arg(job_id);

-- name: ListCredentials :many
SELECT * FROM credentials ORDER BY harness, owner_id, id;

-- name: ListVendorPolicies :many
SELECT * FROM vendor_policies ORDER BY vendor, harness, auth_mode;
