-- Migration 045: event-identity link provenance and serialized dependency
-- edge validation (spec §§4.1, 6.3, 16; §21.46 change 6).

ALTER TABLE links
    RENAME COLUMN created_by_event TO legacy_created_by_event;
ALTER TABLE links
    ALTER COLUMN legacy_created_by_event DROP NOT NULL,
    ADD COLUMN created_by_event_id bigint;

-- Backfill only link/event pairs whose existing projection context identifies
-- exactly one event. Ambiguous or missing historical events remain explicit
-- legacy provenance instead of being associated with a guessed identity.
WITH provenance_candidates AS (
    SELECT
        link.workspace_id,
        link.src_type,
        link.src_id,
        link.dst_type,
        link.dst_id,
        link.kind,
        event.id
    FROM links link
    LEFT JOIN tasks source_task
      ON source_task.workspace_id = link.workspace_id
     AND source_task.id = link.src_id
    JOIN events event
      ON event.workspace_id = link.workspace_id
     AND (
        (
            link.legacy_created_by_event = 'task.created'
            AND link.src_type = 'task'
            AND event.task_id = link.src_id
            AND event.kind = 'task.created'
        )
        OR
        (
            link.legacy_created_by_event = 'blueprint.materialized'
            AND event.kind = 'blueprint.materialized'
            AND (
                (
                    link.src_type = 'blueprint_version'
                    AND event.task_id = split_part(link.src_id, ':v', 1)
                    AND event.payload_json ->> 'version' = split_part(link.src_id, ':v', 2)
                )
                OR
                (
                    link.src_type = 'task'
                    AND event.task_id = source_task.parent_task_id
                    AND event.payload_json ->> 'version' = source_task.origin_spec_version::text
                )
            )
        )
     )
),
unambiguous_provenance AS (
    SELECT
        workspace_id,
        src_type,
        src_id,
        dst_type,
        dst_id,
        kind,
        min(id) AS event_id
    FROM provenance_candidates
    GROUP BY workspace_id, src_type, src_id, dst_type, dst_id, kind
    HAVING count(*) = 1
)
UPDATE links link
SET created_by_event_id = provenance.event_id,
    legacy_created_by_event = NULL
FROM unambiguous_provenance provenance
WHERE link.workspace_id = provenance.workspace_id
  AND link.src_type = provenance.src_type
  AND link.src_id = provenance.src_id
  AND link.dst_type = provenance.dst_type
  AND link.dst_id = provenance.dst_id
  AND link.kind = provenance.kind;

CREATE UNIQUE INDEX events_workspace_id_id_idx
    ON events (workspace_id, id);
ALTER TABLE links
    ADD CONSTRAINT links_created_by_event_fk
        FOREIGN KEY (workspace_id, created_by_event_id)
        REFERENCES events(workspace_id, id),
    ADD CONSTRAINT links_provenance_shape_check
        CHECK (
            (created_by_event_id IS NOT NULL AND legacy_created_by_event IS NULL)
            OR
            (created_by_event_id IS NULL AND legacy_created_by_event IS NOT NULL)
        );
CREATE INDEX links_event_provenance_idx
    ON links (workspace_id, created_by_event_id)
    WHERE created_by_event_id IS NOT NULL;

-- The application acquires the same workspace transaction lock before its
-- direct-create and blueprint-materialization edge writes. Reacquiring it in
-- the trigger also keeps the database invariant true for administrative or
-- future write paths: reciprocal concurrent inserts serialize before the
-- recursive committed-row check.
CREATE OR REPLACE FUNCTION conveyor_reject_dependency_cycle()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(
        hashtext('conveyor:dependency-edges:' || NEW.workspace_id)
    );
    IF EXISTS (
        WITH RECURSIVE ancestors(id) AS (
            SELECT NEW.depends_on_task_id
            UNION
            SELECT dependency.depends_on_task_id
            FROM task_dependencies dependency
            JOIN ancestors ON dependency.task_id = ancestors.id
            WHERE dependency.workspace_id = NEW.workspace_id
        )
        SELECT 1 FROM ancestors WHERE id = NEW.task_id
    ) THEN
        RAISE EXCEPTION 'dependency cycle: task % cannot depend on %',
            NEW.task_id, NEW.depends_on_task_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION conveyor_reject_dependency_cycle() IS
    'Serializes dependency-edge checks per workspace before rejecting cycles.';
