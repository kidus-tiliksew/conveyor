-- Task 261007-9d50e0; component-persistence (schema domains, migration
-- runners), component-artifacts (attachment targets), component-lineage
-- (retired feature history). Drop the retired features entity. Requirement
-- documents replaced the curated feature tree in migration 046, and 047
-- cleared every task assignment. Recorded events, requirement origins, and
-- retained historical_feature_assignment links are history and stay unchanged;
-- this migration writes no event.

-- A residual feature-owned attachment link becomes a workspace-unattached link
-- with the same workspace, artifact, and role. Artifact rows and bytes, and
-- every task, requirement, and planning-session link, are untouched. Insert
-- one unattached link per distinct triple that has none, then remove the
-- feature-owned links, so the unattached uniqueness rule holds when it is
-- recreated below. A detached link has no task owner, so it never becomes task
-- context or task verification evidence.
INSERT INTO artifact_links (workspace_id, artifact_id, role)
SELECT DISTINCT residual.workspace_id, residual.artifact_id, residual.role
FROM artifact_links residual
WHERE residual.feature_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM artifact_links unattached
      WHERE unattached.workspace_id = residual.workspace_id
        AND unattached.artifact_id = residual.artifact_id
        AND unattached.role = residual.role
        AND unattached.task_id IS NULL
        AND unattached.feature_id IS NULL
        AND unattached.requirement_id IS NULL
        AND unattached.planning_session_id IS NULL
  );
DELETE FROM artifact_links WHERE feature_id IS NOT NULL;

-- The owner-exclusivity check and the unattached uniqueness index name the
-- feature column; drop them before the column and recreate them over the
-- three remaining owners.
ALTER TABLE artifact_links DROP CONSTRAINT artifact_links_check;
DROP INDEX artifact_links_workspace_unique;
DROP INDEX IF EXISTS artifact_links_feature_unique;
DROP INDEX IF EXISTS artifact_links_feature_idx;

ALTER TABLE artifact_links DROP COLUMN feature_id;
ALTER TABLE tasks DROP COLUMN feature_id;
DROP TABLE features;

ALTER TABLE artifact_links ADD CONSTRAINT artifact_links_check CHECK (
    (task_id IS NOT NULL)::int
    + (requirement_id IS NOT NULL)::int
    + (planning_session_id IS NOT NULL)::int <= 1
);
CREATE UNIQUE INDEX artifact_links_workspace_unique
    ON artifact_links (workspace_id, artifact_id, role)
    WHERE task_id IS NULL AND requirement_id IS NULL
      AND planning_session_id IS NULL;
