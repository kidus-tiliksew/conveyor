-- Blueprint parents are workspace-scoped task references (spec §4.1).
ALTER TABLE tasks
    DROP CONSTRAINT tasks_parent_task_fk,
    ADD CONSTRAINT tasks_parent_task_fk
        FOREIGN KEY (workspace_id, parent_task_id)
        REFERENCES tasks(workspace_id, id);
