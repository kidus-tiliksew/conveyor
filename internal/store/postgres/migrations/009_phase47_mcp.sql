-- Phase 4.7 retires capacity/sandbox ownership and introduces the BYOA work
-- order protocol, requirements tree, and content-addressed context artifacts
-- (spec §21.4).
DROP TABLE IF EXISTS workspace_credentials;
DROP TABLE IF EXISTS workspace_vendor_policies;
DROP TABLE IF EXISTS credentials;
DROP TABLE IF EXISTS vendor_policies;

-- A sandbox attempt cannot survive the execution-plane removal. Preserve the
-- exact stage as the operator recovery point rather than redispatching it
-- under a new trust boundary without a decision.
UPDATE tasks t
SET state = 'awaiting_human',
    next_stage = '',
    recovery_stage = COALESCE((
        SELECT j.stage FROM jobs j
        WHERE j.task_id = t.id AND j.state IN ('booting', 'running', 'sandbox_boot_failed')
        ORDER BY j.started_at DESC, j.id DESC LIMIT 1
    ), t.recovery_stage),
    updated_at = now()
WHERE t.state = 'running'
  AND EXISTS (SELECT 1 FROM jobs j WHERE j.task_id = t.id AND j.state IN ('booting', 'running', 'sandbox_boot_failed'));

UPDATE jobs
SET state = 'failed', ended_at = COALESCE(ended_at, now()), updated_at = now()
WHERE state IN ('booting', 'running', 'sandbox_boot_failed');

ALTER TABLE jobs
    DROP COLUMN credential_id,
    DROP COLUMN sandbox_ref,
    DROP COLUMN boot_diagnostics;

CREATE TABLE features (
    id          text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    parent_id   text REFERENCES features(id) ON DELETE SET NULL,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, parent_id, name)
);

ALTER TABLE tasks
    ADD COLUMN feature_id text REFERENCES features(id) ON DELETE SET NULL;

CREATE TABLE work_orders (
    id                text PRIMARY KEY,
    workspace_id      text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    task_id           text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    job_id            text NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    stage             text NOT NULL CHECK (stage IN ('implement', 'review')),
    state             text NOT NULL CHECK (state IN ('queued', 'claimed', 'submitted', 'completed', 'cancelled')),
    claimant_id       text NOT NULL DEFAULT '',
    session_id        text NOT NULL DEFAULT '',
    client_token_hash text NOT NULL DEFAULT '',
    agent             text NOT NULL DEFAULT '',
    model             text NOT NULL DEFAULT '',
    lease_expires_at  timestamptz,
    progress          text NOT NULL DEFAULT '',
    cost_usd          double precision NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    tokens_in         bigint NOT NULL DEFAULT 0 CHECK (tokens_in >= 0),
    tokens_out        bigint NOT NULL DEFAULT 0 CHECK (tokens_out >= 0),
    self_reported     boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX work_orders_queue_idx ON work_orders (workspace_id, state, created_at);
CREATE INDEX work_orders_task_idx ON work_orders (task_id, created_at);

CREATE TABLE artifacts (
    id           text NOT NULL,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         text NOT NULL,
    content_type text NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   bigint NOT NULL CHECK (size_bytes >= 0),
    content      bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, id)
);

CREATE TABLE artifact_links (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    artifact_id text NOT NULL,
    task_id     text REFERENCES tasks(id) ON DELETE CASCADE,
    feature_id  text REFERENCES features(id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, artifact_id) REFERENCES artifacts(workspace_id, id) ON DELETE CASCADE,
    CHECK ((task_id IS NOT NULL)::int + (feature_id IS NOT NULL)::int <= 1)
);

CREATE UNIQUE INDEX artifact_links_workspace_unique ON artifact_links (workspace_id, artifact_id) WHERE task_id IS NULL AND feature_id IS NULL;
CREATE UNIQUE INDEX artifact_links_task_unique ON artifact_links (workspace_id, artifact_id, task_id) WHERE task_id IS NOT NULL;
CREATE UNIQUE INDEX artifact_links_feature_unique ON artifact_links (workspace_id, artifact_id, feature_id) WHERE feature_id IS NOT NULL;
CREATE INDEX artifact_links_task_idx ON artifact_links (task_id) WHERE task_id IS NOT NULL;
CREATE INDEX artifact_links_feature_idx ON artifact_links (feature_id) WHERE feature_id IS NOT NULL;
