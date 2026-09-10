-- DEC-41: retire stored personal credentials; preserve historical events.
DROP TABLE IF EXISTS user_forge_tokens;
DROP TABLE IF EXISTS workspace_forge_tokens;

ALTER TABLE events DROP CONSTRAINT IF EXISTS events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'workspace.github_app_connected','workspace.github_app_replaced','workspace.github_app_disconnected','workspace.github_app_installation_recorded',
    'config.updated','workspace.created','workspace.membership_granted','workspace.membership_revoked','identity.legacy_token_rotated',
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
) NOT VALID;

-- Display-name changes are caller-owned, append-only identity audit events.
ALTER TABLE deployment_events DROP CONSTRAINT deployment_events_kind_check;
ALTER TABLE deployment_events ADD CONSTRAINT deployment_events_kind_check CHECK (kind IN (
    'identity.legacy_token_rotated', 'identity.legacy_bindings_healed',
    'identity.personal_token_issued', 'identity.personal_token_revoked',
    'identity.signin_link_issued', 'identity.signin_link_redeemed',
    'identity.invitation_delivery_sent', 'identity.invitation_delivery_failed',
    'identity.invitation_delivery_fallback',
    'identity.dashboard_session_created', 'identity.dashboard_session_revoked',
    'identity.password_set', 'identity.password_changed', 'identity.display_name_changed'
)) NOT VALID;
