CREATE TABLE monitor_observations (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    identity text NOT NULL,
    repository text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('post_merge_failure','direct_push','external_pr_merge','revert')),
    occurrence_id text NOT NULL,
    source_url text NOT NULL,
    commit_sha text NOT NULL DEFAULT '',
    pull_request_number integer NOT NULL DEFAULT 0,
    check_run_id text NOT NULL DEFAULT '',
    feature_id text NOT NULL DEFAULT '',
    observed_at timestamptz NOT NULL,
    context_json jsonb NOT NULL DEFAULT '{}',
    hint_context_json jsonb,
    task_id text REFERENCES tasks(id),
    task_outcome text NOT NULL DEFAULT '' CHECK (task_outcome IN ('','created','reused')),
    state text NOT NULL DEFAULT 'observed',
    deduplicated_count integer NOT NULL DEFAULT 0,
    forge_error_category text NOT NULL DEFAULT '',
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, identity)
);

CREATE TABLE repository_drift (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    id text NOT NULL,
    repository text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('direct_push','external_pr_merge','revert')),
    source_url text NOT NULL,
    commit_sha text NOT NULL DEFAULT '',
    feature_id text NOT NULL DEFAULT '',
    task_id text NOT NULL REFERENCES tasks(id),
    detected_at timestamptz NOT NULL,
    resolved_at timestamptz,
    outcome text NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, id),
    CHECK ((resolved_at IS NULL AND outcome = '') OR
           (resolved_at IS NOT NULL AND outcome IN ('requirements_amended','conflict_resolved','change_reverted')))
);

CREATE INDEX repository_drift_unresolved
    ON repository_drift (workspace_id, detected_at)
    WHERE resolved_at IS NULL;

CREATE TABLE monitor_status (
    workspace_id text PRIMARY KEY REFERENCES workspaces(id),
    last_successful_at timestamptz,
    current_error text NOT NULL DEFAULT '',
    forge_error_category text NOT NULL DEFAULT '',
    backoff_until timestamptz
);

CREATE TABLE monitor_activity (
    id bigserial PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    kind text NOT NULL,
    payload_json jsonb NOT NULL DEFAULT '{}',
    at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX monitor_activity_workspace_at
    ON monitor_activity (workspace_id, at DESC, id DESC);
