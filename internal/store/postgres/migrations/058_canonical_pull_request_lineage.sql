-- Canonicalize legacy pull-request identities without guessing. Migration 057
-- intentionally retained bare-number submitted_as destinations when immutable
-- pull_request.opened payloads lacked repository identity.
WITH repository_counts AS (
  SELECT workspace_id, count(*) AS count,
         min(COALESCE(NULLIF(github_slug,''),name)) AS sole_repository
  FROM repos GROUP BY workspace_id
), candidates AS (
  SELECT l.*,
         COALESCE(
           NULLIF(e.payload_json->>'repository',''),
           NULLIF(COALESCE(NULLIF(task_repo.github_slug,''),task_repo.name),''),
           CASE WHEN repository_counts.count=1 THEN repository_counts.sole_repository END
         ) AS repository
  FROM links l
  LEFT JOIN events e
    ON e.workspace_id=l.workspace_id AND e.id=l.created_by_event_id
   AND e.kind='pull_request.opened' AND e.task_id=l.src_id
  LEFT JOIN tasks t
    ON t.workspace_id=l.workspace_id AND t.id=l.src_id AND l.src_type='task'
  LEFT JOIN repos task_repo
    ON task_repo.workspace_id=t.workspace_id AND task_repo.name=t.repo_name
  LEFT JOIN repository_counts ON repository_counts.workspace_id=l.workspace_id
  WHERE l.kind='submitted_as' AND l.dst_type='pull_request' AND l.dst_id !~ '#'
), eligible AS (
  SELECT * FROM candidates
  WHERE dst_id ~ '^[1-9][0-9]*$' AND repository IS NOT NULL
)
INSERT INTO links
  (workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at,created_by_event_id)
SELECT workspace_id,src_type,src_id,dst_type,repository || '#' || dst_id,kind,
       legacy_created_by_event,created_at,created_by_event_id
FROM eligible
ON CONFLICT (workspace_id,src_type,src_id,dst_type,dst_id,kind) DO UPDATE SET
  created_by_event_id=CASE
    WHEN links.created_by_event_id IS NULL THEN EXCLUDED.created_by_event_id
    WHEN EXCLUDED.created_by_event_id IS NULL THEN links.created_by_event_id
    ELSE LEAST(links.created_by_event_id,EXCLUDED.created_by_event_id)
  END,
  created_at=LEAST(links.created_at,EXCLUDED.created_at),
  legacy_created_by_event=CASE
    WHEN COALESCE(links.created_by_event_id,EXCLUDED.created_by_event_id) IS NOT NULL THEN NULL
    ELSE COALESCE(links.legacy_created_by_event,EXCLUDED.legacy_created_by_event)
  END;

WITH repository_counts AS (
  SELECT workspace_id, count(*) AS count,
         min(COALESCE(NULLIF(github_slug,''),name)) AS sole_repository
  FROM repos GROUP BY workspace_id
), candidates AS (
  SELECT l.*,
         COALESCE(NULLIF(e.payload_json->>'repository',''),
           NULLIF(COALESCE(NULLIF(task_repo.github_slug,''),task_repo.name),''),
           CASE WHEN repository_counts.count=1 THEN repository_counts.sole_repository END) AS repository,
         COALESCE(repository_counts.count,0) AS repository_count
  FROM links l
  LEFT JOIN events e ON e.workspace_id=l.workspace_id AND e.id=l.created_by_event_id
    AND e.kind='pull_request.opened' AND e.task_id=l.src_id
  LEFT JOIN tasks t ON t.workspace_id=l.workspace_id AND t.id=l.src_id AND l.src_type='task'
  LEFT JOIN repos task_repo ON task_repo.workspace_id=t.workspace_id AND task_repo.name=t.repo_name
  LEFT JOIN repository_counts ON repository_counts.workspace_id=l.workspace_id
  WHERE l.kind='submitted_as' AND l.dst_type='pull_request' AND l.dst_id !~ '#'
)
INSERT INTO lineage_repair_exclusions
  (workspace_id,src_type,src_id,dst_type,dst_id,kind,reason,created_by_event_id)
SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,
       CASE
         WHEN dst_id !~ '^[1-9][0-9]*$' THEN 'invalid legacy pull request number'
         WHEN repository_count > 1 THEN 'ambiguous pull request repository'
         ELSE 'missing pull request repository'
       END,
       created_by_event_id
FROM candidates
WHERE dst_id !~ '^[1-9][0-9]*$' OR repository IS NULL
ON CONFLICT (workspace_id,src_type,src_id,dst_type,dst_id,kind) DO UPDATE SET
  reason=EXCLUDED.reason, created_by_event_id=EXCLUDED.created_by_event_id;

WITH repository_counts AS (
  SELECT workspace_id, count(*) AS count,
         min(COALESCE(NULLIF(github_slug,''),name)) AS sole_repository
  FROM repos GROUP BY workspace_id
), eligible AS (
  SELECT l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind
  FROM links l
  LEFT JOIN events e ON e.workspace_id=l.workspace_id AND e.id=l.created_by_event_id
    AND e.kind='pull_request.opened' AND e.task_id=l.src_id
  LEFT JOIN tasks t ON t.workspace_id=l.workspace_id AND t.id=l.src_id AND l.src_type='task'
  LEFT JOIN repos task_repo ON task_repo.workspace_id=t.workspace_id AND task_repo.name=t.repo_name
  LEFT JOIN repository_counts ON repository_counts.workspace_id=l.workspace_id
  WHERE l.kind='submitted_as' AND l.dst_type='pull_request' AND l.dst_id ~ '^[1-9][0-9]*$'
    AND COALESCE(NULLIF(e.payload_json->>'repository',''),
      NULLIF(COALESCE(NULLIF(task_repo.github_slug,''),task_repo.name),''),
      CASE WHEN repository_counts.count=1 THEN repository_counts.sole_repository END) IS NOT NULL
)
DELETE FROM links l USING eligible e
WHERE l.workspace_id=e.workspace_id AND l.src_type=e.src_type AND l.src_id=e.src_id
  AND l.dst_type=e.dst_type AND l.dst_id=e.dst_id AND l.kind=e.kind;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM links WHERE kind IN ('executes_as','delivered_by','produces','head','implemented_by')
             OR src_type='commit' OR dst_type='commit') THEN
    RAISE EXCEPTION 'migration 058 found lineage vocabulary left unrepaired by migration 057';
  END IF;
END $$;
