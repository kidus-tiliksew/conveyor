ALTER TABLE workspaces
ADD COLUMN config_version bigint NOT NULL DEFAULT 1;

-- Workspace-level configuration mutations share the append-only event table
-- with task transitions. task_id remains populated for every task event; a
-- config.updated event is instead anchored directly to its workspace
-- (spec §3.1, §16, §21.3).
ALTER TABLE events
ADD COLUMN workspace_id text REFERENCES workspaces(id);

UPDATE events e
SET workspace_id = t.workspace_id
FROM tasks t
WHERE t.id = e.task_id;

ALTER TABLE events
ALTER COLUMN workspace_id SET NOT NULL,
ALTER COLUMN task_id DROP NOT NULL;

ALTER TABLE events
ADD CONSTRAINT events_scope_check CHECK (
    task_id IS NOT NULL
    OR (kind = 'config.updated' AND job_id IS NULL)
);

CREATE INDEX events_workspace_timeline_idx ON events (workspace_id, at, id);
