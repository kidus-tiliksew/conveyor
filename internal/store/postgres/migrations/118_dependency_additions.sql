-- Migration 118: audited post-creation task dependency additions (REQ-4/AC-4.6).
CREATE TABLE task_dependency_additions (
    workspace_id       text NOT NULL REFERENCES workspaces(id),
    request_id         text NOT NULL,
    task_id            text NOT NULL,
    depends_on_task_id text NOT NULL,
    reason             text NOT NULL,
    actor_id           text NOT NULL,
    actor_role         text NOT NULL,
    added              boolean NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, request_id),
    FOREIGN KEY (workspace_id, task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, depends_on_task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);
