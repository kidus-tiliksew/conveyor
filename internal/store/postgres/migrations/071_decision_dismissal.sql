ALTER TABLE decisions DROP CONSTRAINT decisions_status_check;
ALTER TABLE decisions ADD COLUMN dismissed_by text;
ALTER TABLE decisions ADD COLUMN dismissed_at timestamptz;
ALTER TABLE decisions ADD CONSTRAINT decisions_status_check
  CHECK (status IN ('proposed','confirmed','dismissed','superseded'));
ALTER TABLE decisions ADD CONSTRAINT decisions_lifecycle_check CHECK (
  (status='proposed' AND confirmed_by IS NULL AND confirmed_at IS NULL AND dismissed_by IS NULL AND dismissed_at IS NULL AND superseded_by IS NULL)
  OR (status='confirmed' AND confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL AND dismissed_by IS NULL AND dismissed_at IS NULL AND superseded_by IS NULL)
  OR (status='dismissed' AND confirmed_by IS NULL AND confirmed_at IS NULL AND dismissed_by IS NOT NULL AND dismissed_at IS NOT NULL AND superseded_by IS NULL)
  OR (status='superseded' AND confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL AND dismissed_by IS NULL AND dismissed_at IS NULL AND superseded_by IS NOT NULL)
);

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'decision.proposed','decision.confirmed','decision.dismissed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);
