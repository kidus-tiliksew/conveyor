-- Requirement delivery-staleness dismissal is an append-only, workspace-scoped
-- operator judgment. It belongs to the requirement timeline, not to a task.
ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed','requirement.staleness_acknowledged',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'planning_bundle.finalized','planning_bundle.revised','planning_bundle.approved','planning_bundle.rejected',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'decision.proposed','decision.confirmed','decision.dismissed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);
