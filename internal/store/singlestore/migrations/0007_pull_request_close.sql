-- component-git-delivery: req-task-lifecycle-and-queue AC-7.4.
CREATE ROWSTORE TABLE IF NOT EXISTS pull_request_closes (
 workspace_id VARCHAR(255) NOT NULL,
 task_id VARCHAR(255) NOT NULL,
 payload_json JSON NOT NULL,
 PRIMARY KEY (workspace_id, task_id),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
