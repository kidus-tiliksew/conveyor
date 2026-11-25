-- component-production-feedback, feature-production-feedback-live FB-2,
-- req-production-feedback REQ-1 and REQ-2, req-environments AC-2.4, and
-- DEC-149(1). These rows are records for people to read. No dispatch, claim,
-- gate, review, merge, next-action, lineage, or work-order query reads these
-- tables (component-production-feedback invariant 1). Every row names its
-- workspace. An empty reported value was not stated. No backfill.

-- The newest accepted signal of each environment. The listening state is
-- computed from it at each read (component-production-feedback invariant 13).
CREATE TABLE IF NOT EXISTS feedback_signals (
    workspace_id text NOT NULL,
    environment_id text NOT NULL,
    last_signal_at timestamptz NOT NULL,
    next_signal_seconds integer NOT NULL CHECK (next_signal_seconds BETWEEN 10 AND 86400),
    source text NOT NULL CHECK (source = 'signal'),
    source_label text NOT NULL,
    actor_id text NOT NULL,
    PRIMARY KEY (workspace_id, environment_id),
    FOREIGN KEY (workspace_id, environment_id) REFERENCES environments(workspace_id, id) ON DELETE CASCADE
);

-- One row for each deploy. A version that comes back after it stopped is a
-- new deploy (component-production-feedback invariants 5 and 6).
CREATE TABLE IF NOT EXISTS feedback_deploys (
    workspace_id text NOT NULL,
    id text NOT NULL,
    environment_id text NOT NULL,
    service text NOT NULL CHECK (char_length(service) BETWEEN 1 AND 100),
    reported_repository text NOT NULL,
    reported_commit text NOT NULL CHECK (char_length(reported_commit) <= 40),
    reported_version text NOT NULL CHECK (char_length(reported_version) <= 200),
    resolved boolean NOT NULL DEFAULT false,
    deployed_at timestamptz NOT NULL,
    source text NOT NULL CHECK (source IN ('signal', 'github')),
    source_label text NOT NULL,
    actor_id text NOT NULL,
    stopped_at timestamptz NULL,
    stop_source text NULL CHECK (stop_source IN ('signal', 'github')),
    stop_source_label text NULL,
    stop_actor_id text NULL,
    PRIMARY KEY (workspace_id, id),
    FOREIGN KEY (workspace_id, environment_id) REFERENCES environments(workspace_id, id) ON DELETE CASCADE,
    CHECK (num_nonnulls(stopped_at, stop_source, stop_source_label, stop_actor_id) IN (0, 4))
);

CREATE INDEX IF NOT EXISTS feedback_deploys_environment_idx
    ON feedback_deploys (workspace_id, environment_id, deployed_at);

-- What each service runs. The digests are the SHA-256 of the exact service
-- name and of the exact reported repository, commit, and version, so no
-- collation decides which versions are equal.
CREATE TABLE IF NOT EXISTS feedback_running_versions (
    workspace_id text NOT NULL,
    environment_id text NOT NULL,
    service_digest text NOT NULL,
    version_digest text NOT NULL,
    service text NOT NULL,
    reported_repository text NOT NULL,
    reported_commit text NOT NULL,
    reported_version text NOT NULL,
    resolved boolean NOT NULL DEFAULT false,
    deploy_id text NOT NULL,
    deployed_at timestamptz NOT NULL,
    -- The arrival of the newest signal that named the service.
    reported_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, environment_id, service_digest, version_digest),
    FOREIGN KEY (workspace_id, environment_id) REFERENCES environments(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, deploy_id) REFERENCES feedback_deploys(workspace_id, id) ON DELETE CASCADE
);

-- Every accepted signal, with the digests of the services it named. A signal
-- that reaches the store after a later arrival ends at the next arrival that
-- named its service, which can be one that changed nothing, and no limit on
-- that delay exists (component-production-feedback invariants 4 and 5).
CREATE TABLE IF NOT EXISTS feedback_accepted_signals (
    workspace_id text NOT NULL,
    environment_id text NOT NULL,
    arrived_at timestamptz NOT NULL,
    id text NOT NULL,
    source_label text NOT NULL,
    service_digests jsonb NOT NULL CHECK (jsonb_typeof(service_digests) = 'array'),
    PRIMARY KEY (workspace_id, environment_id, arrived_at, id),
    FOREIGN KEY (workspace_id, environment_id) REFERENCES environments(workspace_id, id) ON DELETE CASCADE
);
