-- Migration 119 repairs the known version-118 collision before advancing the
-- document archive schema. A database may have applied either version-118
-- migration, so this migration idempotently establishes both contracts.
CREATE TABLE IF NOT EXISTS task_dependency_additions (
    workspace_id       text NOT NULL REFERENCES workspaces(id),
    request_id         text NOT NULL,
    task_id            text NOT NULL,
    depends_on_task_id text NOT NULL,
    reason             text NOT NULL,
    actor_id           text NOT NULL,
    actor_role         text NOT NULL,
    added              boolean NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, request_id),
    FOREIGN KEY (workspace_id, task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, depends_on_task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);

ALTER TABLE requirements
    ADD COLUMN IF NOT EXISTS archived_at timestamptz,
    ADD COLUMN IF NOT EXISTS archived_by text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS superseding_document_ids text[] NOT NULL DEFAULT '{}';

ALTER TABLE system_designs
    ADD COLUMN IF NOT EXISTS archived_at timestamptz,
    ADD COLUMN IF NOT EXISTS archived_by text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS superseding_document_ids text[] NOT NULL DEFAULT '{}';

ALTER TABLE events DROP CONSTRAINT IF EXISTS events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
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
