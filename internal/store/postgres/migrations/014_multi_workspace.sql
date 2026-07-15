CREATE UNIQUE INDEX workspaces_name_lower_unique ON workspaces (lower(btrim(name)));

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
    task_id IS NOT NULL
    OR (kind IN ('config.updated', 'workspace.created') AND job_id IS NULL)
);
