-- Migration 094: make superseded, unconfirmed requirement revisions an
-- explicit audited lifecycle instead of leaving them as permanent proposals.

ALTER TABLE requirement_versions
    ADD COLUMN IF NOT EXISTS retired boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS retired_by text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS retired_at timestamptz,
    ADD COLUMN IF NOT EXISTS retired_by_version integer;

ALTER TABLE requirement_versions
    DROP CONSTRAINT IF EXISTS requirement_versions_retirement_check;
ALTER TABLE requirement_versions
    ADD CONSTRAINT requirement_versions_retirement_check CHECK (
        (retired AND NOT confirmed AND retired_by <> '' AND retired_at IS NOT NULL
            AND retired_by_version IS NOT NULL AND retired_by_version > version)
        OR
        (NOT retired AND retired_by = '' AND retired_at IS NULL AND retired_by_version IS NULL)
    );

ALTER TABLE requirement_versions
    DROP CONSTRAINT IF EXISTS requirement_versions_retired_by_version_fk;
ALTER TABLE requirement_versions
    ADD CONSTRAINT requirement_versions_retired_by_version_fk
        FOREIGN KEY (workspace_id, requirement_id, retired_by_version)
        REFERENCES requirement_versions (workspace_id, requirement_id, version);

DROP INDEX IF EXISTS requirement_versions_pending_idx;
CREATE INDEX requirement_versions_pending_idx
    ON requirement_versions (workspace_id, requirement_id, version DESC)
    WHERE NOT confirmed AND NOT retired;

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
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);

CREATE TEMPORARY TABLE migration_094_retired_requirement_versions ON COMMIT DROP AS
SELECT version.workspace_id, version.requirement_id, version.version,
       requirement.current_version AS confirmed_version
FROM requirement_versions version
JOIN requirements requirement
  ON requirement.workspace_id = version.workspace_id
 AND requirement.id = version.requirement_id
WHERE requirement.current_version IS NOT NULL
  AND version.version < requirement.current_version
  AND NOT version.confirmed
  AND NOT version.retired;

UPDATE requirement_versions version
SET retired = true,
    retired_by = 'migration-094',
    retired_at = now(),
    retired_by_version = repair.confirmed_version
FROM migration_094_retired_requirement_versions repair
WHERE version.workspace_id = repair.workspace_id
  AND version.requirement_id = repair.requirement_id
  AND version.version = repair.version;
