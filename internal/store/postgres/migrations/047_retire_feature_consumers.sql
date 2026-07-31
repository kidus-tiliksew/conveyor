-- Complete the live feature-tree retirement after migration 046 preserved and
-- re-homed every historical reference (spec §21.46 changes 5, 7, and 9).

ALTER TABLE monitor_observations
    RENAME COLUMN feature_id TO requirement_id;
ALTER TABLE repository_drift
    RENAME COLUMN feature_id TO requirement_id;

-- Migration 046 guarantees that every feature named by monitor history seeded
-- the deterministic req-<feature-id> document before this projection changes.
UPDATE monitor_observations observation
SET requirement_id = 'req-' || observation.requirement_id
WHERE observation.requirement_id <> ''
  AND EXISTS (
      SELECT 1
      FROM requirements requirement
      WHERE requirement.workspace_id = observation.workspace_id
        AND requirement.id = 'req-' || observation.requirement_id
  );

UPDATE repository_drift drift
SET requirement_id = 'req-' || drift.requirement_id
WHERE drift.requirement_id <> ''
  AND EXISTS (
      SELECT 1
      FROM requirements requirement
      WHERE requirement.workspace_id = drift.workspace_id
        AND requirement.id = 'req-' || drift.requirement_id
  );

-- Historical assignments now live in links. Keeping the deprecated column
-- physically present makes old task rows readable during the compatibility
-- window, but no task remains assigned to the retired taxonomy.
UPDATE tasks SET feature_id = NULL WHERE feature_id IS NOT NULL;
