-- Phase 8.3 planning delivery bundles (spec §21.58 change 5).
CREATE TABLE planning_bundles (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id text NOT NULL,
  session_id text NOT NULL,
  title text NOT NULL,
  documents jsonb NOT NULL CHECK (jsonb_typeof(documents)='array'),
  tasks jsonb NOT NULL CHECK (jsonb_typeof(tasks)='array'),
  status text NOT NULL CHECK (status IN ('pending','approved','rejected')),
  created_by text,
  decided_by text,
  created_at timestamptz NOT NULL,
  decided_at timestamptz,
  PRIMARY KEY (workspace_id,id),
  FOREIGN KEY (workspace_id,session_id) REFERENCES planning_sessions(workspace_id,id),
  CHECK ((status='pending' AND decided_by IS NULL AND decided_at IS NULL)
      OR (status<>'pending' AND decided_at IS NOT NULL))
);

ALTER TABLE planning_sessions DROP CONSTRAINT planning_sessions_goal_check;
ALTER TABLE planning_sessions ADD CONSTRAINT planning_sessions_goal_check
  CHECK (goal IN ('requirement','system_design','blueprint','bundle','open'));
ALTER TABLE planning_sessions ADD COLUMN produced_bundle_id text;
ALTER TABLE planning_sessions ADD CONSTRAINT planning_sessions_produced_bundle_fk
  FOREIGN KEY (workspace_id,produced_bundle_id) REFERENCES planning_bundles(workspace_id,id);

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'planning_bundle.finalized','planning_bundle.approved','planning_bundle.rejected',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'decision.proposed','decision.confirmed','decision.dismissed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);
