-- Documentation-closure policy pins and per-head gate evidence (component-task-lifecycle, component-persistence).
CREATE ROWSTORE TABLE IF NOT EXISTS task_documentation_policies (
 workspace_id VARCHAR(255) NOT NULL,
 task_id VARCHAR(255) NOT NULL,
 payload_json JSON NOT NULL,
 PRIMARY KEY (workspace_id, task_id),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS documentation_gate_evidence (
 workspace_id VARCHAR(255) NOT NULL,
 task_id VARCHAR(255) NOT NULL,
 head_sha VARCHAR(255) NOT NULL,
 payload_json JSON NOT NULL,
 PRIMARY KEY (workspace_id, task_id, head_sha),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
