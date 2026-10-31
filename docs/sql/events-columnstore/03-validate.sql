-- Operator-run only, while all application traffic remains stopped.
-- Every discrepancy count must be zero; counts alone are not proof.
SELECT
 (SELECT COUNT(*) FROM events) AS source_rows,
 (SELECT COUNT(*) FROM events_columnstore) AS destination_rows,
 (SELECT COUNT(*) FROM events s LEFT JOIN events_columnstore d
   ON d.workspace_id=s.workspace_id AND d.id=s.id
   WHERE d.id IS NULL) AS missing_rows,
 (SELECT COUNT(*) FROM events_columnstore d LEFT JOIN events s
   ON s.workspace_id=d.workspace_id AND s.id=d.id
   WHERE s.id IS NULL) AS extra_rows,
 (SELECT COUNT(*) FROM events s JOIN events_columnstore d
   ON d.workspace_id=s.workspace_id AND d.id=s.id
   WHERE NOT (BINARY d.task_id <=> BINARY s.task_id)
      OR NOT (BINARY d.job_id <=> BINARY s.job_id)
      OR NOT (BINARY d.kind <=> BINARY s.kind)
      OR NOT (BINARY d.actor_id <=> BINARY s.actor_id)
      OR NOT (BINARY d.actor_role <=> BINARY s.actor_role)
      OR NOT (BINARY CAST(d.payload_json AS CHAR) <=> BINARY CAST(s.payload_json AS CHAR))
      OR NOT (d.at <=> s.at)
      OR NOT (BINARY d.workspace_id <=> BINARY s.workspace_id)
      OR NOT (BINARY d.event_shard_key <=> BINARY COALESCE(NULLIF(s.task_id, ''), CONCAT('event/', CAST(s.id AS CHAR))))) AS changed_rows,
 (SELECT COUNT(*) FROM (
   SELECT workspace_id,id FROM events_columnstore GROUP BY workspace_id,id HAVING COUNT(*)<>1
 ) duplicate_ids) AS duplicate_workspace_ids;

SELECT workspace_id, COUNT(*) AS rows_in_workspace,
 SUM(task_id IS NULL) AS taskless_rows, MIN(id) AS first_id, MAX(id) AS last_id
FROM events GROUP BY workspace_id ORDER BY workspace_id;
SELECT workspace_id, COUNT(*) AS rows_in_workspace,
 SUM(task_id IS NULL) AS taskless_rows, MIN(id) AS first_id, MAX(id) AS last_id
FROM events_columnstore GROUP BY workspace_id ORDER BY workspace_id;
