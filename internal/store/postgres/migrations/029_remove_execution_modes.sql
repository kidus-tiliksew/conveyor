-- Remove execution modes in favor of a per-task hold (spec §21.31). The mode
-- column survives as an unconstrained legacy record: new tasks write the
-- empty string and nothing reads it for behavior.
ALTER TABLE tasks ADD COLUMN hold boolean NOT NULL DEFAULT false;
ALTER TABLE tasks DROP CONSTRAINT tasks_mode_check;

-- Non-terminal Manual tasks keep their reservation semantics as hold=true,
-- each recorded with a migration audit event (§21.31 change 6).
UPDATE tasks SET hold = true
WHERE mode = 'manual' AND state NOT IN ('merged', 'closed');

INSERT INTO events (workspace_id, task_id, kind, actor_id, actor_role, payload_json)
SELECT workspace_id, id, 'task.hold.migrated', 'migration-021-31', 'system',
       jsonb_build_object('from_mode', 'manual', 'hold', true)
FROM tasks
WHERE mode = 'manual' AND state NOT IN ('merged', 'closed');
