-- Connect blueprint identities to every immutable version so bounded context
-- traversal can cross requirement -> blueprint -> version -> child without
-- parsing opaque node IDs (spec §4.2 item 4, §16).
INSERT INTO links (
    workspace_id, src_type, src_id, dst_type, dst_id, kind,
    created_by_event_id, created_at
)
SELECT
    event.workspace_id,
    'blueprint',
    event.task_id,
    'blueprint_version',
    event.task_id || ':v' || (event.payload_json ->> 'version'),
    'versions',
    event.id,
    event.at
FROM events event
WHERE event.kind = 'spec.version_created'
  AND event.task_id IS NOT NULL
  AND COALESCE((event.payload_json ->> 'version')::integer, 0) > 0
ON CONFLICT (workspace_id, src_type, src_id, dst_type, dst_id, kind)
DO UPDATE SET
    created_by_event_id = LEAST(
        COALESCE(links.created_by_event_id, EXCLUDED.created_by_event_id),
        EXCLUDED.created_by_event_id
    ),
    created_at = LEAST(links.created_at, EXCLUDED.created_at),
    legacy_created_by_event = NULL;
