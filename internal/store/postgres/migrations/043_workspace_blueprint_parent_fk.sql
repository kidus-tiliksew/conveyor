-- Blueprint parents are workspace-scoped task references (spec §4.1).
-- This follows the blueprint origin-identity migration introduced on main.
ALTER TABLE tasks
    DROP CONSTRAINT tasks_parent_task_fk,
    ADD CONSTRAINT tasks_parent_task_fk
        FOREIGN KEY (workspace_id, parent_task_id)
        REFERENCES tasks(workspace_id, id);
