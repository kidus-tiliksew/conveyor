CREATE TABLE workspaces (
    id          text PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    config_yaml text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE repos (
    workspace_id      text NOT NULL REFERENCES workspaces(id),
    name              text NOT NULL,
    url               text NOT NULL,
    github_slug       text NOT NULL DEFAULT '',
    default_base      text NOT NULL,
    devcontainer_path text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, name)
);

CREATE TABLE users (
    id                    text PRIMARY KEY,
    identity_provider_ref text NOT NULL UNIQUE,
    role                  text NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE credentials (
    id               text PRIMARY KEY,
    owner_id         text NOT NULL,
    owner_kind       text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    kind             text NOT NULL CHECK (kind IN ('personal_sub', 'team_sub', 'api')),
    harness          text NOT NULL,
    enc_ref          text NOT NULL,
    rate_limit_state text NOT NULL DEFAULT 'available',
    policy_flag      text NOT NULL DEFAULT 'unknown',
    cooldown_until   timestamptz,
    last_error       text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX credentials_routing_idx
    ON credentials (harness, policy_flag, rate_limit_state, cooldown_until);

CREATE TABLE vendor_policies (
    vendor                text NOT NULL,
    harness               text NOT NULL,
    auth_mode             text NOT NULL,
    subscription_headless text NOT NULL CHECK (subscription_headless IN ('allowed', 'restricted', 'disallowed', 'unknown')),
    reviewed_at           date NOT NULL,
    source_url            text NOT NULL,
    updated_at            timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (vendor, harness, auth_mode)
);

CREATE TABLE tasks (
    id               text PRIMARY KEY,
    workspace_id     text NOT NULL REFERENCES workspaces(id),
    source           text NOT NULL,
    title            text NOT NULL,
    body             text NOT NULL DEFAULT '',
    class            text NOT NULL DEFAULT '',
    escalation_level text NOT NULL,
    repo_name        text NOT NULL,
    base_branch      text NOT NULL,
    branch           text NOT NULL UNIQUE,
    state            text NOT NULL,
    parent_task_id   text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, repo_name) REFERENCES repos(workspace_id, name)
);

CREATE INDEX tasks_stage_feed_idx ON tasks (state, updated_at DESC);
CREATE INDEX tasks_source_idx ON tasks (source);

CREATE TABLE jobs (
    id               text PRIMARY KEY,
    task_id          text NOT NULL REFERENCES tasks(id),
    stage            text NOT NULL,
    harness          text NOT NULL,
    model_tier       text NOT NULL DEFAULT '',
    runner           text NOT NULL,
    sandbox_ref      text NOT NULL DEFAULT '',
    pack_version     text NOT NULL DEFAULT '',
    confinement_tier text NOT NULL,
    budget_usd       double precision NOT NULL DEFAULT 0,
    cost_usd         double precision NOT NULL DEFAULT 0,
    tokens_in        bigint NOT NULL DEFAULT 0,
    tokens_out       bigint NOT NULL DEFAULT 0,
    state            text NOT NULL,
    boot_diagnostics jsonb,
    started_at       timestamptz NOT NULL,
    ended_at         timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX jobs_task_started_idx ON jobs (task_id, started_at);

CREATE TABLE events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id      text NOT NULL REFERENCES tasks(id),
    job_id       text REFERENCES jobs(id),
    kind         text NOT NULL,
    actor_id     text NOT NULL,
    actor_role   text NOT NULL,
    payload_json jsonb NOT NULL,
    at           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX events_task_timeline_idx ON events (task_id, at, id);
CREATE INDEX events_job_idx ON events (job_id, at, id) WHERE job_id IS NOT NULL;

CREATE TABLE transcripts (
    job_id          text PRIMARY KEY REFERENCES jobs(id),
    uri             text NOT NULL,
    redaction_stats jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE interventions (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id     text NOT NULL REFERENCES tasks(id),
    job_id      text REFERENCES jobs(id),
    actor_id    text NOT NULL,
    actor_role  text NOT NULL,
    action      text NOT NULL CHECK (action IN ('approve', 'reject', 'redirect', 'pull_to_local')),
    reason_code text NOT NULL,
    comment     text NOT NULL DEFAULT '',
    at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX interventions_task_idx ON interventions (task_id, at, id);

CREATE FUNCTION conveyor_reject_append_only_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER events_append_only
BEFORE UPDATE OR DELETE ON events
FOR EACH ROW EXECUTE FUNCTION conveyor_reject_append_only_mutation();

CREATE TRIGGER interventions_append_only
BEFORE UPDATE OR DELETE ON interventions
FOR EACH ROW EXECUTE FUNCTION conveyor_reject_append_only_mutation();
