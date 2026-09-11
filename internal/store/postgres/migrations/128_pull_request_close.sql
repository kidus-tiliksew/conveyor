-- component-git-delivery; req-task-lifecycle-and-queue AC-7.4.
CREATE TABLE pull_request_closes (
 workspace_id text NOT NULL,
 task_id text NOT NULL,
 payload_json jsonb NOT NULL,
 PRIMARY KEY (workspace_id, task_id),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id)
);
