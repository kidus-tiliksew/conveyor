-- Migration 050: forward-only repair for the Phase 6.2 migration projections.
-- Migrations 046 and 047 are immutable once recorded; pending 046 executions
-- receive only the guarded application overlay in migrate.go.

-- Requirement ownership makes these links attached, so two legacy features
-- may retain separate links to the same content-addressed artifact.
DROP INDEX IF EXISTS artifact_links_workspace_unique;
CREATE UNIQUE INDEX artifact_links_workspace_unique
    ON artifact_links (workspace_id, artifact_id, role)
    WHERE task_id IS NULL AND feature_id IS NULL AND requirement_id IS NULL;

-- Repair and feature-retirement audit rows are workspace-scoped. Keep the
-- complete migration-048 allowlist while adding only these repair kinds.
ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
    task_id IS NOT NULL
    OR (kind IN (
        'config.updated', 'workspace.created', 'worker.pairing_issued',
        'worker.enrolled', 'worker.revoked', 'worker.heartbeat',
        'requirement.created', 'requirement.version_proposed',
        'requirement.version_confirmed', 'planning_session.created',
        'planning_session.message_appended', 'planning_session.finalized',
        'planning_session.abandoned', 'planning_session.repo_pinned',
        'migration.feature_node_dropped',
        'migration.requirement_reference_repaired'
    ) AND job_id IS NULL)
);

-- Migration-created documents and their immutable first versions are
-- projections too. Backfill the two events ordinary CreateRequirement writes,
-- without duplicating any event already written by a repaired pending upgrade.
INSERT INTO events
    (workspace_id, task_id, job_id, kind, actor_id, actor_role, payload_json, at)
SELECT requirement.workspace_id, NULL, NULL, 'requirement.created',
       'migration-050', 'system',
       jsonb_build_object(
           'workspace_id', requirement.workspace_id,
           'requirement_id', requirement.id,
           'slug', requirement.slug,
           'title', requirement.title,
           'origin', 'feature_migration'
       ),
       now()
FROM requirements requirement
JOIN requirement_versions version
  ON version.workspace_id = requirement.workspace_id
 AND version.requirement_id = requirement.id
 AND version.version = 1
 AND version.origin = 'feature_migration'
WHERE NOT EXISTS (
    SELECT 1 FROM events event
    WHERE event.workspace_id = requirement.workspace_id
      AND event.task_id IS NULL
      AND event.kind = 'requirement.created'
      AND event.payload_json->>'requirement_id' = requirement.id
);

INSERT INTO events
    (workspace_id, task_id, job_id, kind, actor_id, actor_role, payload_json, at)
SELECT version.workspace_id, NULL, NULL, 'requirement.version_proposed',
       'migration-050', 'system',
       jsonb_build_object(
           'workspace_id', version.workspace_id,
           'requirement_id', version.requirement_id,
           'version', version.version,
           'origin', version.origin,
           'origin_session_id', version.origin_session_id,
           'origin_drift_id', version.origin_drift_id,
           'statement_count', jsonb_array_length(version.statements_json)
       ),
       now()
FROM requirement_versions version
WHERE version.version = 1
  AND version.origin = 'feature_migration'
  AND NOT EXISTS (
      SELECT 1 FROM events event
      WHERE event.workspace_id = version.workspace_id
        AND event.task_id IS NULL
        AND event.kind = 'requirement.version_proposed'
        AND event.payload_json->>'requirement_id' = version.requirement_id
        AND event.payload_json->>'version' = version.version::text
  );

-- Pending-046 upgrades retain empty-node identity in this staging table until
-- this forward migration can append an event under the final allowlist. An
-- already-046 database cannot reconstruct rows that its historical migration
-- deleted, so the table is empty there and the repair remains honest.
CREATE TABLE IF NOT EXISTS conveyor_migration_046_dropped_features (
    workspace_id text NOT NULL,
    feature_id text NOT NULL,
    parent_id text,
    name text NOT NULL,
    description text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, feature_id)
);
INSERT INTO events
    (workspace_id, task_id, job_id, kind, actor_id, actor_role, payload_json, at)
SELECT dropped.workspace_id, NULL, NULL, 'migration.feature_node_dropped',
       'migration-050', 'system',
       jsonb_build_object(
           'workspace_id', dropped.workspace_id,
           'feature_id', dropped.feature_id,
           'parent_id', coalesce(dropped.parent_id, ''),
           'name', dropped.name,
           'description', dropped.description,
           'feature_created_at', dropped.created_at
       ),
       now()
FROM conveyor_migration_046_dropped_features dropped;
DROP TABLE conveyor_migration_046_dropped_features;

-- Migration 047 renamed plain-text feature references without validating
-- them. Preserve the bad value in audit before nulling it, then make absence a
-- real NULL and enforce workspace-scoped referential integrity.
ALTER TABLE monitor_observations
    ALTER COLUMN requirement_id DROP NOT NULL,
    ALTER COLUMN requirement_id DROP DEFAULT;
ALTER TABLE repository_drift
    ALTER COLUMN requirement_id DROP NOT NULL,
    ALTER COLUMN requirement_id DROP DEFAULT;

CREATE TEMPORARY TABLE migration_050_invalid_observation_requirements AS
SELECT observation.workspace_id, observation.identity,
       observation.requirement_id AS invalid_requirement_id
FROM monitor_observations observation
WHERE nullif(observation.requirement_id, '') IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM requirements requirement
      WHERE requirement.workspace_id = observation.workspace_id
        AND requirement.id = observation.requirement_id
  );

CREATE TEMPORARY TABLE migration_050_invalid_drift_requirements AS
SELECT drift.workspace_id, drift.id AS drift_id,
       drift.requirement_id AS invalid_requirement_id
FROM repository_drift drift
WHERE nullif(drift.requirement_id, '') IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM requirements requirement
      WHERE requirement.workspace_id = drift.workspace_id
        AND requirement.id = drift.requirement_id
  );

INSERT INTO events
    (workspace_id, task_id, job_id, kind, actor_id, actor_role, payload_json, at)
SELECT invalid.workspace_id, NULL, NULL,
       'migration.requirement_reference_repaired', 'migration-050', 'system',
       jsonb_build_object(
           'workspace_id', invalid.workspace_id,
           'record_type', 'monitor_observation',
           'record_id', invalid.identity,
           'invalid_requirement_id', invalid.invalid_requirement_id,
           'repair', 'nulled'
       ),
       now()
FROM migration_050_invalid_observation_requirements invalid;

INSERT INTO events
    (workspace_id, task_id, job_id, kind, actor_id, actor_role, payload_json, at)
SELECT invalid.workspace_id, NULL, NULL,
       'migration.requirement_reference_repaired', 'migration-050', 'system',
       jsonb_build_object(
           'workspace_id', invalid.workspace_id,
           'record_type', 'repository_drift',
           'record_id', invalid.drift_id,
           'invalid_requirement_id', invalid.invalid_requirement_id,
           'repair', 'nulled'
       ),
       now()
FROM migration_050_invalid_drift_requirements invalid;

UPDATE monitor_observations observation
SET requirement_id = NULL
WHERE observation.requirement_id = ''
   OR EXISTS (
       SELECT 1 FROM migration_050_invalid_observation_requirements invalid
       WHERE invalid.workspace_id = observation.workspace_id
         AND invalid.identity = observation.identity
   );

UPDATE repository_drift drift
SET requirement_id = NULL
WHERE drift.requirement_id = ''
   OR EXISTS (
       SELECT 1 FROM migration_050_invalid_drift_requirements invalid
       WHERE invalid.workspace_id = drift.workspace_id
         AND invalid.drift_id = drift.id
   );

ALTER TABLE monitor_observations
    ADD CONSTRAINT monitor_observations_requirement_fk
        FOREIGN KEY (workspace_id, requirement_id)
        REFERENCES requirements(workspace_id, id);
ALTER TABLE repository_drift
    ADD CONSTRAINT repository_drift_requirement_fk
        FOREIGN KEY (workspace_id, requirement_id)
        REFERENCES requirements(workspace_id, id);

CREATE INDEX monitor_observations_requirement_idx
    ON monitor_observations (workspace_id, requirement_id)
    WHERE requirement_id IS NOT NULL;
CREATE INDEX repository_drift_requirement_idx
    ON repository_drift (workspace_id, requirement_id)
    WHERE requirement_id IS NOT NULL;
