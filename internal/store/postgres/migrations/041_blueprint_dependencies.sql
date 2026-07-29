-- Phase 6.1 blueprint lineage and dependency ordering (spec §§4.1, 6.3, 16).
ALTER TABLE tasks
    ALTER COLUMN parent_task_id DROP NOT NULL,
    ALTER COLUMN parent_task_id DROP DEFAULT;
UPDATE tasks SET parent_task_id = NULL WHERE parent_task_id = '';
ALTER TABLE tasks
    ADD COLUMN origin_spec_version integer NOT NULL DEFAULT 0,
    ADD COLUMN origin_sub_id text NOT NULL DEFAULT '',
    ADD CONSTRAINT tasks_parent_task_fk
        FOREIGN KEY (parent_task_id) REFERENCES tasks(id);

CREATE INDEX tasks_parent_task_idx
    ON tasks (workspace_id, parent_task_id)
    WHERE parent_task_id IS NOT NULL;
CREATE UNIQUE INDEX tasks_workspace_id_id_idx ON tasks (workspace_id, id);
CREATE UNIQUE INDEX tasks_live_blueprint_origin_idx
    ON tasks (workspace_id, parent_task_id, origin_spec_version, origin_sub_id)
    WHERE parent_task_id IS NOT NULL
      AND origin_sub_id <> ''
      AND state NOT IN ('merged', 'closed');

CREATE TABLE task_dependencies (
    workspace_id      text NOT NULL REFERENCES workspaces(id),
    task_id           text NOT NULL,
    depends_on_task_id text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, task_id, depends_on_task_id),
    FOREIGN KEY (workspace_id, task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, depends_on_task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE,
    CHECK (task_id <> depends_on_task_id)
);

CREATE INDEX task_dependencies_dependency_idx
    ON task_dependencies (workspace_id, depends_on_task_id, task_id);

CREATE FUNCTION conveyor_reject_dependency_cycle()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
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

CREATE TRIGGER task_dependencies_acyclic
BEFORE INSERT OR UPDATE ON task_dependencies
FOR EACH ROW EXECUTE FUNCTION conveyor_reject_dependency_cycle();

CREATE TABLE links (
    workspace_id     text NOT NULL REFERENCES workspaces(id),
    src_type         text NOT NULL,
    src_id           text NOT NULL,
    dst_type         text NOT NULL,
    dst_id           text NOT NULL,
    kind             text NOT NULL,
    created_by_event text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, src_type, src_id, dst_type, dst_id, kind)
);

CREATE INDEX links_destination_idx
    ON links (workspace_id, dst_type, dst_id, kind);
