-- Direct operator dismissal preserves immutable versions while removing them
-- from actionable projections. Confirmation-side retirement still names the
-- later confirming version; a direct dismissal leaves that reference NULL.

ALTER TABLE requirement_versions
    DROP CONSTRAINT IF EXISTS requirement_versions_retirement_check;
ALTER TABLE requirement_versions
    ADD CONSTRAINT requirement_versions_retirement_check CHECK (
        (retired AND NOT confirmed AND retired_by <> '' AND retired_at IS NOT NULL
            AND (retired_by_version IS NULL OR retired_by_version > version))
        OR
        (NOT retired AND retired_by = '' AND retired_at IS NULL AND retired_by_version IS NULL)
    );

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','workspace.membership_granted','workspace.membership_revoked','identity.legacy_token_rotated',
    'workspace.forge_token_stored','workspace.forge_token_replaced','workspace.forge_token_deleted',
    'worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed','requirement.version_retired','requirement.version_dismissed','requirement.staleness_acknowledged',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'planning_bundle.finalized','planning_bundle.revised','planning_bundle.approved','planning_bundle.rejected',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'decision.proposed','decision.confirmed','decision.dismissed',
    'decision.supersession_sweep_opened','decision.supersession_sweep_reopened','decision.supersession_sweep_auto_cleared','decision.supersession_sweep_dismissed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);
