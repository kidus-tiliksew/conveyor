CREATE INDEX repository_drift_unresolved_task
    ON repository_drift (workspace_id, task_id)
    WHERE resolved_at IS NULL;
