-- DEC-41; req-security-boundaries AC-6.2. Existing migrations are immutable.
CREATE TABLE workspace_github_apps (
 workspace_id text PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
 app_id bigint NOT NULL,
 app_slug text NOT NULL,
 client_id text NOT NULL,
 private_key_nonce bytea NOT NULL CHECK (octet_length(private_key_nonce) = 12),
 private_key_ciphertext bytea NOT NULL,
 installation_id bigint,
 installation_account text,
 connected_by text NOT NULL,
 connected_at timestamptz NOT NULL DEFAULT now(),
 installation_recorded_at timestamptz
);

ALTER TABLE events DROP CONSTRAINT IF EXISTS events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'workspace.github_app_connected','workspace.github_app_replaced','workspace.github_app_disconnected','workspace.github_app_installation_recorded',
    'config.updated','workspace.created','workspace.membership_granted','workspace.membership_revoked','identity.legacy_token_rotated',
    'workspace.forge_token_stored','workspace.forge_token_replaced','workspace.forge_token_deleted',
    'worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed','requirement.version_retired','requirement.version_dismissed','requirement.staleness_acknowledged',
    'requirement.archived','requirement.restored',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'planning_bundle.finalized','planning_bundle.revised','planning_bundle.approved','planning_bundle.rejected',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'system_design.archived','system_design.restored',
    'decision.proposed','decision.confirmed','decision.dismissed',
    'decision.supersession_sweep_opened','decision.supersession_sweep_reopened','decision.supersession_sweep_auto_cleared','decision.supersession_sweep_dismissed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);
