-- Repair Phase 8.2 decision and system-design lifecycle semantics.
ALTER TABLE decisions DROP CONSTRAINT decisions_workspace_id_supersedes_key;
CREATE UNIQUE INDEX decisions_confirmed_supersedes_key
  ON decisions(workspace_id,supersedes)
  WHERE status='confirmed' AND supersedes IS NOT NULL;

ALTER TABLE system_design_versions ADD COLUMN dismissed boolean NOT NULL DEFAULT false;
ALTER TABLE system_design_versions ADD COLUMN dismissed_by text;
ALTER TABLE system_design_versions ADD COLUMN dismissed_at timestamptz;
ALTER TABLE system_design_versions ADD CONSTRAINT system_design_versions_lifecycle_check CHECK (
  (confirmed AND NOT dismissed AND confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL AND dismissed_by IS NULL AND dismissed_at IS NULL)
  OR (dismissed AND NOT confirmed AND dismissed_by IS NOT NULL AND dismissed_at IS NOT NULL AND confirmed_by IS NULL AND confirmed_at IS NULL)
  OR (NOT confirmed AND NOT dismissed AND confirmed_by IS NULL AND confirmed_at IS NULL AND dismissed_by IS NULL AND dismissed_at IS NULL)
);

ALTER TABLE system_design_versions DROP CONSTRAINT system_design_versions_workspace_id_document_id_fkey;
ALTER TABLE system_design_versions ADD CONSTRAINT system_design_versions_workspace_id_document_id_fkey
  FOREIGN KEY (workspace_id,document_id) REFERENCES system_designs(workspace_id,id) ON DELETE CASCADE;

ALTER TABLE planning_sessions DROP CONSTRAINT planning_sessions_system_design_context_fk;
ALTER TABLE planning_sessions ADD CONSTRAINT planning_sessions_system_design_context_fk
  FOREIGN KEY (workspace_id,system_design_context_id) REFERENCES system_designs(workspace_id,id)
  ON DELETE SET NULL (system_design_context_id);
ALTER TABLE planning_sessions DROP CONSTRAINT planning_sessions_produced_system_design_fk;
ALTER TABLE planning_sessions ADD CONSTRAINT planning_sessions_produced_system_design_fk
  FOREIGN KEY (workspace_id,produced_system_design_id) REFERENCES system_designs(workspace_id,id)
  ON DELETE SET NULL (produced_system_design_id);

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'decision.proposed','decision.confirmed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);
