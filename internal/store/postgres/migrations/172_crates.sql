-- component-crates; feature-crates CR-1; req-crates REQ-1, REQ-2. A crate is a
-- workspace record that groups tasks for tracking and reporting. It holds a
-- name, an optional target date, a declared requirement list, and a state of
-- open or closed. A task belongs to at most one crate through the nullable
-- tasks.crate_id column. Existing tasks keep a NULL crate. No dispatch,
-- claim, gate, review, merge, next-action, or lineage query reads either
-- table or the column (component-crates invariant 1; DEC-141).
CREATE TABLE IF NOT EXISTS crates (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    id text NOT NULL,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    target_date date NULL,
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    closed_at timestamptz NULL,
    PRIMARY KEY (workspace_id, id)
);

-- The declared list. An edit replaces it whole, and it changes no task's
-- context attachments (component-crates invariant 5; req-crates AC-3.3).
CREATE TABLE IF NOT EXISTS crate_requirements (
    workspace_id text NOT NULL,
    crate_id text NOT NULL,
    requirement_id text NOT NULL,
    PRIMARY KEY (workspace_id, crate_id, requirement_id),
    FOREIGN KEY (workspace_id, crate_id) REFERENCES crates(workspace_id, id) ON DELETE CASCADE
);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS crate_id text NULL;

DO $$
BEGIN
    -- pg_constraint spans every schema, so the check names this schema's tasks.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_crate_fk' AND conrelid = 'tasks'::regclass) THEN
        ALTER TABLE tasks ADD CONSTRAINT tasks_crate_fk
            FOREIGN KEY (workspace_id, crate_id) REFERENCES crates(workspace_id, id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS tasks_workspace_crate_idx ON tasks (workspace_id, crate_id)
    WHERE crate_id IS NOT NULL;

-- A crate change appends one taskless workspace event: crate.created,
-- crate.updated, crate.closed, or crate.reopened (component-crates invariant
-- 8). This keeps every kind migration 154 admits and admits those four.
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
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt',
    'crate.created','crate.updated','crate.closed','crate.reopened'
  ) AND job_id IS NULL)
) NOT VALID;
