ALTER TABLE tasks
    ADD COLUMN assignee_user_id text REFERENCES users(id) ON DELETE SET NULL;

CREATE INDEX tasks_workspace_assignee_idx
    ON tasks (workspace_id, assignee_user_id)
    WHERE assignee_user_id IS NOT NULL;
