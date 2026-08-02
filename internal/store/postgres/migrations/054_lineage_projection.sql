-- Phase 6.3 event-derived lineage projection (spec §4.2 item 4, §16).
-- Existing minimal Phase 6.1 rows remain valid while the new projector fills
-- the generalized graph. Every current dependency gains an explicit source
-- event so a subsequent rebuild is lossless.

INSERT INTO events (
    workspace_id, task_id, kind, actor_id, actor_role, payload_json, at
)
SELECT
    dependency.workspace_id,
    dependency.task_id,
    'task.dependency_added',
    'system',
    'system',
    jsonb_build_object(
        'task_id', dependency.task_id,
        'depends_on_task_id', dependency.depends_on_task_id,
        'backfilled', true
    ),
    dependency.created_at
FROM task_dependencies dependency
WHERE NOT EXISTS (
    SELECT 1
    FROM events event
    WHERE event.workspace_id = dependency.workspace_id
      AND event.task_id = dependency.task_id
      AND event.kind = 'task.dependency_added'
      AND event.payload_json ->> 'depends_on_task_id' = dependency.depends_on_task_id
);

-- Child task.created events already contain their immutable blueprint origin.
INSERT INTO links (
    workspace_id, src_type, src_id, dst_type, dst_id, kind,
    created_by_event_id, created_at
)
SELECT
    event.workspace_id,
    'blueprint_version',
    (event.payload_json ->> 'parent_task_id') || ':v' ||
        (event.payload_json ->> 'origin_spec_version'),
    'task',
    event.task_id,
    'materializes',
    event.id,
    event.at
FROM events event
WHERE event.kind = 'task.created'
  AND event.task_id IS NOT NULL
  AND COALESCE(event.payload_json ->> 'parent_task_id', '') <> ''
  AND COALESCE((event.payload_json ->> 'origin_spec_version')::integer, 0) > 0
ON CONFLICT (workspace_id, src_type, src_id, dst_type, dst_id, kind)
DO UPDATE SET
    created_by_event_id = LEAST(
        COALESCE(links.created_by_event_id, EXCLUDED.created_by_event_id),
        EXCLUDED.created_by_event_id
    ),
    created_at = LEAST(links.created_at, EXCLUDED.created_at),
    legacy_created_by_event = NULL;

INSERT INTO links (
    workspace_id, src_type, src_id, dst_type, dst_id, kind,
    created_by_event_id, created_at
)
SELECT
    event.workspace_id,
    'task',
    event.task_id,
    'task',
    event.payload_json ->> 'depends_on_task_id',
    'depends_on',
    event.id,
    event.at
FROM events event
WHERE event.kind = 'task.dependency_added'
  AND event.task_id IS NOT NULL
  AND COALESCE(event.payload_json ->> 'depends_on_task_id', '') <> ''
ON CONFLICT (workspace_id, src_type, src_id, dst_type, dst_id, kind)
DO UPDATE SET
    created_by_event_id = LEAST(
        COALESCE(links.created_by_event_id, EXCLUDED.created_by_event_id),
        EXCLUDED.created_by_event_id
    ),
    created_at = LEAST(links.created_at, EXCLUDED.created_at),
    legacy_created_by_event = NULL;

-- Generalize the projection across the already-recorded Phase 6.2 and
-- delivery events. These statements are intentionally event-only: mutable
-- read-model tables are not allowed to invent graph history.
INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'task',event.task_id,'work_order',
       COALESCE(NULLIF(event.payload_json ->> 'id',''), event.payload_json ->> 'work_order_id'),
       'executes_as',event.id,event.at
FROM events event
WHERE event.kind='work_order.created' AND event.task_id IS NOT NULL
  AND COALESCE(NULLIF(event.payload_json ->> 'id',''), event.payload_json ->> 'work_order_id','') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'planning_session',event.payload_json ->> 'session_id',
       'requirement',event.payload_json ->> 'produced_requirement_id','produces',event.id,event.at
FROM events event
WHERE event.kind='planning_session.finalized'
  AND COALESCE(event.payload_json ->> 'session_id','') <> ''
  AND COALESCE(event.payload_json ->> 'produced_requirement_id','') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'planning_session',event.payload_json ->> 'session_id',
       'blueprint',event.payload_json ->> 'produced_task_id','produces',event.id,event.at
FROM events event
WHERE event.kind='planning_session.finalized'
  AND COALESCE(event.payload_json ->> 'session_id','') <> ''
  AND COALESCE(event.payload_json ->> 'produced_task_id','') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'requirement',event.payload_json ->> 'requirement_id',
       'blueprint',event.task_id,'serves',event.id,event.at
FROM events event
WHERE event.kind='requirement.serves_confirmed' AND event.task_id IS NOT NULL
  AND COALESCE(event.payload_json ->> 'requirement_id','') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'requirement_version',
       (event.payload_json ->> 'requirement_id') || ':v' || (event.payload_json ->> 'version'),
       'requirement_version',
       (event.payload_json ->> 'requirement_id') || ':v' || (((event.payload_json ->> 'version')::integer)-1)::text,
       'supersedes',event.id,event.at
FROM events event
WHERE event.kind='requirement.version_confirmed'
  AND COALESCE((event.payload_json ->> 'version')::integer,0) > 1
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'blueprint_version',event.task_id || ':v' || (event.payload_json ->> 'version'),
       'blueprint_version',event.task_id || ':v' || (((event.payload_json ->> 'version')::integer)-1)::text,
       'supersedes',event.id,event.at
FROM events event
WHERE event.kind='spec.version_created' AND event.task_id IS NOT NULL
  AND COALESCE((event.payload_json ->> 'version')::integer,0) > 1
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'task',event.task_id,'pull_request',
       concat_ws('#',NULLIF(event.payload_json ->> 'repository',''),event.payload_json ->> 'number'),
       'delivered_by',event.id,event.at
FROM events event
WHERE event.kind='pull_request.opened' AND event.task_id IS NOT NULL
  AND COALESCE(event.payload_json ->> 'number','') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'pull_request',
       concat_ws('#',NULLIF(event.payload_json ->> 'repository',''),event.payload_json ->> 'number'),
       'commit',event.payload_json ->> 'head_sha','head',event.id,event.at
FROM events event
WHERE event.kind='pull_request.opened'
  AND COALESCE(event.payload_json ->> 'number','') <> ''
  AND COALESCE(event.payload_json ->> 'head_sha','') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'task',event.task_id,'commit',event.payload_json ->> 'head_sha',
       'implemented_by',event.id,event.at
FROM events event
WHERE event.kind='pull_request.opened' AND event.task_id IS NOT NULL
  AND COALESCE(event.payload_json ->> 'head_sha','') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'work_order',event.payload_json ->> 'review_work_order_id',
       'verdict','review:' || (event.payload_json ->> 'review_work_order_id'),
       'produces',event.id,event.at
FROM events event
WHERE event.kind='review.completed'
  AND COALESCE(event.payload_json ->> 'review_work_order_id','') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'evidence',evidence.id,'verdict',
       'review:' || (event.payload_json ->> 'review_work_order_id'),
       'supports',event.id,event.at
FROM events event
CROSS JOIN LATERAL jsonb_array_elements_text(
    COALESCE(event.payload_json -> 'evidence_ids','[]'::jsonb)
) evidence(id)
WHERE event.kind='review.completed'
  AND COALESCE(event.payload_json ->> 'review_work_order_id','') <> ''
ON CONFLICT DO NOTHING;

ALTER TABLE links
    ADD CONSTRAINT links_nonempty_endpoints_check CHECK (
        btrim(src_type) <> '' AND btrim(src_id) <> '' AND
        btrim(dst_type) <> '' AND btrim(dst_id) <> '' AND btrim(kind) <> ''
    ),
    ADD CONSTRAINT links_no_self_edge_check CHECK (
        src_type <> dst_type OR src_id <> dst_id
    );
