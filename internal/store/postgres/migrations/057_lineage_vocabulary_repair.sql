-- Phase 6.3 lineage vocabulary repair. Events are immutable; this migration
-- changes only their rebuildable projection (spec §4.2 item 4, §16).
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
        'lineage.rebuilt'
    ) AND job_id IS NULL)
);

CREATE TABLE lineage_repair_exclusions (
    workspace_id text NOT NULL,
    src_type text NOT NULL,
    src_id text NOT NULL,
    dst_type text NOT NULL,
    dst_id text NOT NULL,
    kind text NOT NULL,
    reason text NOT NULL,
    created_by_event_id bigint,
    PRIMARY KEY (workspace_id,src_type,src_id,dst_type,dst_id,kind)
);

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at,created_by_event_id)
SELECT workspace_id,src_type,src_id,dst_type,dst_id,
       CASE
         WHEN kind='executes_as' THEN 'dispatches'
         WHEN kind='delivered_by' THEN 'submitted_as'
         WHEN kind='produces' AND dst_type='requirement' THEN 'produced_requirement'
         WHEN kind='produces' AND dst_type='blueprint' THEN 'produced_blueprint'
         WHEN kind='produces' AND dst_type='verdict' THEN 'produced_verdict'
       END,
       legacy_created_by_event,created_at,created_by_event_id
FROM links
WHERE kind IN ('executes_as','delivered_by')
   OR (kind='produces' AND dst_type IN ('requirement','blueprint','verdict'))
ON CONFLICT DO NOTHING;
DELETE FROM links WHERE kind IN ('executes_as','delivered_by')
   OR (kind='produces' AND dst_type IN ('requirement','blueprint','verdict'));

-- Reconstruct commit ranges only from the asserting event's complete identity.
INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT e.workspace_id,'task',e.task_id,'commit_range',
       (e.payload_json->>'repository') || '@' || (e.payload_json->>'base_sha') || '..' || (e.payload_json->>'head_sha'),
       'submitted_range',e.id,e.at
FROM events e
WHERE e.kind='pull_request.opened' AND e.task_id IS NOT NULL
  AND COALESCE(e.payload_json->>'repository','')<>''
  AND COALESCE(e.payload_json->>'base_sha','')<>''
  AND COALESCE(e.payload_json->>'head_sha','')<>''
ON CONFLICT (workspace_id,src_type,src_id,dst_type,dst_id,kind) DO UPDATE SET
  created_by_event_id=LEAST(COALESCE(links.created_by_event_id,EXCLUDED.created_by_event_id),EXCLUDED.created_by_event_id),
  created_at=LEAST(links.created_at,EXCLUDED.created_at), legacy_created_by_event=NULL;

INSERT INTO lineage_repair_exclusions
SELECT l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,
       CASE WHEN l.kind='head' THEN 'ambiguous relationship' ELSE 'insufficient event payload for canonical commit range' END,
       l.created_by_event_id
FROM links l
WHERE l.kind IN ('head','implemented_by') OR l.src_type='commit' OR l.dst_type='commit'
ON CONFLICT DO NOTHING;

DELETE FROM links WHERE kind IN ('head','implemented_by') OR src_type='commit' OR dst_type='commit';

-- Unknown legacy produces rows cannot be interpreted without guessing.
INSERT INTO lineage_repair_exclusions
SELECT l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,
       'unsupported legacy produces destination',l.created_by_event_id
FROM links l WHERE l.kind='produces'
ON CONFLICT DO NOTHING;
DELETE FROM links WHERE kind='produces';

-- Guarded parsing contract for historical numeric JSON fields: future repair
-- queries must test the textual form before casting.
DO $$ BEGIN
  PERFORM 1 FROM events
   WHERE kind IN ('task.created','requirement.version_confirmed','spec.version_created','pull_request.opened')
     AND COALESCE(payload_json->>'version', payload_json->>'origin_spec_version', payload_json->>'number', '') ~ '^[0-9]+$'
   LIMIT 1;
END $$;
