-- Migration 051: proposed and operator-confirmed blueprint/requirement serves
-- relationships (spec §4.2 item 1; §21.46 change 7).

CREATE TABLE requirement_serves_links (
    workspace_id      text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    blueprint_task_id text NOT NULL,
    requirement_id    text NOT NULL,
    state             text NOT NULL CHECK (state IN ('proposed','confirmed','dismissed')),
    source            text NOT NULL CHECK (source IN ('planning','triage','operator')),
    created_by_event_id bigint NOT NULL,
    decision_event_id   bigint,
    proposed_by       text NOT NULL,
    decided_by        text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, blueprint_task_id, requirement_id),
    FOREIGN KEY (workspace_id, blueprint_task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, requirement_id)
        REFERENCES requirements(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, created_by_event_id)
        REFERENCES events(workspace_id, id),
    FOREIGN KEY (workspace_id, decision_event_id)
        REFERENCES events(workspace_id, id),
    CHECK (
        (state = 'proposed' AND decision_event_id IS NULL AND decided_by = '')
        OR
        (state IN ('confirmed','dismissed') AND decision_event_id IS NOT NULL AND decided_by <> '')
    )
);

-- Existing Phase 6.2 machinery suggestions already have exact event
-- provenance. Preserve them as proposals during upgrade; they must no longer
-- render as confirmed merely because a planning session named a requirement.
INSERT INTO requirement_serves_links (
    workspace_id, blueprint_task_id, requirement_id, state, source,
    created_by_event_id, proposed_by, created_at, updated_at
)
SELECT
    event.workspace_id,
    event.task_id,
    event.payload_json ->> 'requirement_id',
    'proposed',
    CASE WHEN task.source LIKE 'planning:%' THEN 'planning' ELSE 'triage' END,
    event.id,
    COALESCE(NULLIF(event.actor_id, ''), 'conveyor'),
    event.at,
    event.at
FROM events event
JOIN tasks task
  ON task.workspace_id = event.workspace_id
 AND task.id = event.task_id
JOIN requirements requirement
  ON requirement.workspace_id = event.workspace_id
 AND requirement.id = event.payload_json ->> 'requirement_id'
WHERE event.kind = 'task.requirement_suggested'
  AND event.task_id IS NOT NULL
ORDER BY event.id
ON CONFLICT (workspace_id, blueprint_task_id, requirement_id) DO NOTHING;

CREATE INDEX requirement_serves_links_requirement_idx
    ON requirement_serves_links (workspace_id, requirement_id, state, blueprint_task_id);
