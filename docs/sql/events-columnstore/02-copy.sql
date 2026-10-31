-- Operator-run only, AFTER all numbered schema migrations and legacy helpers.
-- Stop every old and corrected writer/reader before this statement; comments
-- do not enforce the freeze. The target must be empty and the source immutable.
-- One full scan, not repeated global id-only range scans of a workspace/id key.
-- Never use IGNORE, REPLACE, or ON DUPLICATE KEY to hide missing/duplicate history.
INSERT INTO events_columnstore
 (id, event_shard_key, task_id, job_id, kind, actor_id, actor_role, payload_json, at, workspace_id)
SELECT id, COALESCE(NULLIF(task_id, ''), CONCAT('event/', CAST(id AS CHAR))),
 task_id, job_id, kind, actor_id, actor_role, payload_json, at, workspace_id
FROM events;
