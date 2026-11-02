-- req-quiet-idle-runtime AC-3.2, AC-3.3, AC-3.6; DEC-99(5);
-- component-identity-membership (The webhook secret) and component-persistence
-- (Schema domains). The workspace GitHub webhook secret is sealed like the App
-- private key and leaves with its App row. Each accepted delivery is recorded
-- once per workspace; a repeated delivery ID is refused by the primary key.
-- No backfill, and no event row is written.
CREATE TABLE IF NOT EXISTS workspace_github_webhook_secrets (
    workspace_id text PRIMARY KEY REFERENCES workspace_github_apps(workspace_id) ON DELETE CASCADE,
    secret_nonce bytea NOT NULL CHECK (octet_length(secret_nonce) = 12),
    secret_ciphertext bytea NOT NULL CHECK (octet_length(secret_ciphertext) > 0),
    set_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS github_webhook_deliveries (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    delivery_id text NOT NULL CHECK (delivery_id <> ''),
    event text NOT NULL CHECK (event <> ''),
    received_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, delivery_id)
);

CREATE INDEX IF NOT EXISTS github_webhook_deliveries_received_idx
    ON github_webhook_deliveries (workspace_id, received_at DESC);

-- A webhook secret write appends the taskless, metadata-only
-- workspace.github_webhook_secret_set event. This keeps every kind migration
-- 143 admits and admits that one.
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
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
) NOT VALID;
