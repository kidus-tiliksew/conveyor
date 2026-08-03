-- Repair the migration-058 repos.name fallback without guessing a durable
-- pull-request repository identity. Event payload identity wins; otherwise a
-- task repository contributes only a non-empty GitHub slug (spec §16).
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
        'migration.feature_node_dropped', 'migration.requirement_reference_repaired',
        'lineage.vocabulary_repaired', 'lineage.historical_fabrication_recorded',
        'lineage.pull_request_identity_repaired', 'lineage.rebuilt'
    ) AND job_id IS NULL)
);

CREATE TEMPORARY TABLE migration_060_candidates ON COMMIT DROP AS
SELECT l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,
       l.legacy_created_by_event,l.created_at,l.created_by_event_id,
       e.payload_json->>'number' AS pull_request_number,
       COALESCE(NULLIF(e.payload_json->>'repository',''),NULLIF(r.github_slug,'')) AS repository,
       NULLIF(r.name,'') AS repository_name,
       COALESCE(NULLIF(e.payload_json->>'repository',''),'')='' AND
         COALESCE(NULLIF(r.github_slug,''),'')='' AND
         l.dst_id=r.name || '#' || (e.payload_json->>'number') AS name_fallback
FROM links l
JOIN events e ON e.workspace_id=l.workspace_id AND e.id=l.created_by_event_id
  AND e.kind='pull_request.opened' AND e.task_id=l.src_id
JOIN tasks t ON t.workspace_id=l.workspace_id AND t.id=l.src_id
LEFT JOIN repos r ON r.workspace_id=t.workspace_id AND r.name=t.repo_name
WHERE l.kind='submitted_as' AND l.src_type='task' AND l.dst_type='pull_request'
  AND COALESCE(e.payload_json->>'number','') ~ '^[1-9][0-9]*$'
  AND (l.dst_id !~ '#' OR (
       COALESCE(NULLIF(e.payload_json->>'repository',''),'')='' AND
       COALESCE(NULLIF(r.github_slug,''),'')='' AND
       l.dst_id=r.name || '#' || (e.payload_json->>'number')));

CREATE TEMPORARY TABLE migration_060_actions (
    workspace_id text NOT NULL,
    action text NOT NULL,
    reason text NOT NULL
) ON COMMIT DROP;

INSERT INTO migration_060_actions
SELECT workspace_id,'canonicalized','event repository or task github_slug supplied canonical identity'
FROM migration_060_candidates
WHERE repository IS NOT NULL AND dst_id<>repository || '#' || pull_request_number;

INSERT INTO links
  (workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at,created_by_event_id)
SELECT workspace_id,src_type,src_id,dst_type,repository || '#' || pull_request_number,kind,
       legacy_created_by_event,created_at,created_by_event_id
FROM migration_060_candidates WHERE repository IS NOT NULL
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

-- A row minted from repos.name by 058 is returned to its non-canonical bare
-- identity and explicitly excluded. This preserves provenance without leaving
-- an identity that a future slug-backed event would split.
INSERT INTO migration_060_actions
SELECT workspace_id,'reverted_name_fallback','empty github_slug and missing event repository'
FROM migration_060_candidates WHERE repository IS NULL AND name_fallback;

INSERT INTO links
  (workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at,created_by_event_id)
SELECT workspace_id,src_type,src_id,dst_type,pull_request_number,kind,
       legacy_created_by_event,created_at,created_by_event_id
FROM migration_060_candidates WHERE repository IS NULL AND name_fallback
ON CONFLICT (workspace_id,src_type,src_id,dst_type,dst_id,kind) DO UPDATE SET
  created_by_event_id=CASE
    WHEN links.created_by_event_id IS NULL THEN EXCLUDED.created_by_event_id
    WHEN EXCLUDED.created_by_event_id IS NULL THEN links.created_by_event_id
    ELSE LEAST(links.created_by_event_id,EXCLUDED.created_by_event_id)
  END,
  created_at=LEAST(links.created_at,EXCLUDED.created_at);

INSERT INTO migration_060_actions
SELECT candidate.workspace_id,'excluded','empty github_slug and missing event repository'
FROM migration_060_candidates candidate
LEFT JOIN lineage_repair_exclusions exclusion
  ON exclusion.workspace_id=candidate.workspace_id
 AND exclusion.src_type=candidate.src_type AND exclusion.src_id=candidate.src_id
 AND exclusion.dst_type=candidate.dst_type AND exclusion.dst_id=candidate.pull_request_number
 AND exclusion.kind=candidate.kind
WHERE candidate.repository IS NULL
  AND (exclusion.workspace_id IS NULL OR exclusion.reason<>'empty github_slug and missing event repository');

INSERT INTO lineage_repair_exclusions
  (workspace_id,src_type,src_id,dst_type,dst_id,kind,reason,created_by_event_id)
SELECT workspace_id,src_type,src_id,dst_type,pull_request_number,kind,
       'empty github_slug and missing event repository',created_by_event_id
FROM migration_060_candidates WHERE repository IS NULL
ON CONFLICT (workspace_id,src_type,src_id,dst_type,dst_id,kind) DO UPDATE SET
  reason=EXCLUDED.reason, created_by_event_id=EXCLUDED.created_by_event_id;

DELETE FROM links link USING migration_060_candidates candidate
WHERE link.workspace_id=candidate.workspace_id AND link.src_type=candidate.src_type
  AND link.src_id=candidate.src_id AND link.dst_type=candidate.dst_type
  AND link.dst_id=candidate.dst_id AND link.kind=candidate.kind
  AND candidate.dst_id<>COALESCE(candidate.repository || '#' || candidate.pull_request_number,
                                  candidate.pull_request_number);
