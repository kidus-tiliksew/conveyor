-- component-environments, feature-production-feedback-live FB-1,
-- req-environments REQ-1 and REQ-2, req-security-boundaries AC-2.8, and
-- DEC-148(6). An environment is a workspace record for one place the
-- software runs. No dispatch, claim, gate, review, merge, next-action,
-- lineage, or work-order query reads these tables (component-environments
-- invariant 1). Every row names its workspace. No backfill.
CREATE TABLE IF NOT EXISTS environments (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    id text NOT NULL,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 40),
    -- The server's case fold of the name, so no collation decides uniqueness.
    name_fold text NOT NULL,
    tracked_branches jsonb NOT NULL CHECK (jsonb_typeof(tracked_branches) = 'object'),
    github_environments jsonb NOT NULL CHECK (jsonb_typeof(github_environments) = 'array' AND jsonb_array_length(github_environments) <= 10),
    version_rule_kind text NULL CHECK (version_rule_kind IN ('commit_pattern', 'tag_template')),
    version_rule_text text NULL CHECK (char_length(version_rule_text) <= 200),
    timing_window_minutes integer NOT NULL CHECK (timing_window_minutes BETWEEN 1 AND 1440),
    archived boolean NOT NULL DEFAULT false,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, id),
    CHECK ((version_rule_kind IS NULL) = (version_rule_text IS NULL))
);

-- component-environments invariant 3.
CREATE UNIQUE INDEX IF NOT EXISTS environments_active_name_idx
    ON environments (workspace_id, name_fold) WHERE NOT archived;

-- A key is stored only as the 32-byte SHA-256 of its value
-- (component-environments invariant 11).
CREATE TABLE IF NOT EXISTS environment_keys (
    workspace_id text NOT NULL,
    id text NOT NULL,
    environment_id text NOT NULL,
    label text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 60),
    key_hash bytea NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    last_used_at timestamptz NULL,
    revoked_at timestamptz NULL,
    PRIMARY KEY (workspace_id, id),
    FOREIGN KEY (workspace_id, environment_id) REFERENCES environments(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS environment_keys_environment_idx
    ON environment_keys (workspace_id, environment_id) WHERE revoked_at IS NULL;

-- One row for each suggested or ignored GitHub environment name. A row
-- ignored before any report has no repository and no report time
-- (component-environments invariant 16).
CREATE TABLE IF NOT EXISTS environment_suggestions (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name_fold text NOT NULL,
    github_environment text NOT NULL CHECK (char_length(github_environment) BETWEEN 1 AND 255),
    repository text NULL,
    last_reported_at timestamptz NULL,
    ignored boolean NOT NULL DEFAULT false,
    PRIMARY KEY (workspace_id, name_fold),
    CHECK (ignored OR (repository IS NOT NULL AND last_reported_at IS NOT NULL))
);

-- An environment change appends one taskless workspace event
-- (component-environments invariant 9). This keeps every kind migration 172
-- admits and admits the seven environment kinds.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN ('artifact.metadata_repaired',
    'workspace.github_app_connected','workspace.github_app_replaced','workspace.github_app_disconnected','workspace.github_app_installation_recorded',
    'workspace.github_webhook_secret_set',
    'config.updated','workspace.created','workspace.membership_granted','workspace.membership_revoked','workspace.invitation_resent','identity.legacy_token_rotated',
    'worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed','requirement.version_retired','requirement.version_dismissed','requirement.staleness_acknowledged',
    'requirement.archived','requirement.restored','requirement.title_changed',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'planning_bundle.finalized','planning_bundle.revised','planning_bundle.approved','planning_bundle.rejected',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'system_design.archived','system_design.restored','system_design.title_changed',
    'decision.proposed','decision.confirmed','decision.dismissed',
    'decision.supersession_sweep_opened','decision.supersession_sweep_reopened','decision.supersession_sweep_auto_cleared','decision.supersession_sweep_dismissed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt',
    'crate.created','crate.updated','crate.closed','crate.reopened',
    'environment.created','environment.updated','environment.archived','environment.restored',
    'environment.key_issued','environment.key_revoked','environment.suggestion_ignored'
  ) AND job_id IS NULL)
) NOT VALID;
