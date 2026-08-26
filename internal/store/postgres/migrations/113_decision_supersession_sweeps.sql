-- Decision supersession sweeps are a signal-only projection. They do not
-- participate in any lifecycle guard, queue predicate, or confirmation gate.
CREATE TABLE decision_supersession_sweeps (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  decision_id text NOT NULL,
  superseded_decision_id text NOT NULL,
  document_tier text NOT NULL CHECK (document_tier IN ('requirement','system_design','reference_document')),
  document_id text NOT NULL,
  status text NOT NULL CHECK (status IN ('open','dismissed','auto_cleared')),
  detected_by text NOT NULL,
  detected_at timestamptz NOT NULL,
  resolved_by text NOT NULL DEFAULT '',
  resolved_at timestamptz,
  PRIMARY KEY (workspace_id,decision_id,document_tier,document_id),
  FOREIGN KEY (workspace_id,decision_id) REFERENCES decisions(workspace_id,id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id,superseded_decision_id) REFERENCES decisions(workspace_id,id),
  CHECK ((status='open' AND resolved_by='' AND resolved_at IS NULL)
      OR (status IN ('dismissed','auto_cleared') AND resolved_by<>'' AND resolved_at IS NOT NULL))
);

CREATE INDEX decision_supersession_sweeps_open_idx
  ON decision_supersession_sweeps(workspace_id,decision_id)
  WHERE status='open';

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','workspace.membership_granted','workspace.membership_revoked','identity.legacy_token_rotated',
    'worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed','requirement.version_retired','requirement.staleness_acknowledged',
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

WITH current_corpus AS (
  SELECT r.workspace_id, 'requirement'::text AS document_tier, r.id AS document_id, v.content
  FROM requirements r
  JOIN requirement_versions v ON v.workspace_id=r.workspace_id AND v.requirement_id=r.id AND v.version=r.current_version
  WHERE v.confirmed
  UNION ALL
  SELECT d.workspace_id, 'system_design', d.id, v.content
  FROM system_designs d
  JOIN system_design_versions v ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version
  WHERE v.confirmed
  UNION ALL
  SELECT d.workspace_id, 'reference_document', d.id, v.content
  FROM reference_documents d
  JOIN reference_document_versions v ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version
  WHERE d.deleted_at IS NULL
)
INSERT INTO decision_supersession_sweeps (
  workspace_id,decision_id,superseded_decision_id,document_tier,document_id,
  status,detected_by,detected_at
)
SELECT d.workspace_id,d.id,d.supersedes,c.document_tier,c.document_id,
       'open','migration-113',now()
FROM decisions d
JOIN current_corpus c ON c.workspace_id=d.workspace_id
WHERE d.supersedes IS NOT NULL
  AND d.confirmed_at IS NOT NULL
  AND c.content ~ ('\m' || d.supersedes || '\M')
ON CONFLICT DO NOTHING;
