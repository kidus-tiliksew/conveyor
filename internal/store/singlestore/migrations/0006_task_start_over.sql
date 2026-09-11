-- req-task-lifecycle-and-queue REQ-7. The Go migration adds task columns
-- with schema checks under the startup lock because DDL commits implicitly.
CREATE ROWSTORE TABLE IF NOT EXISTS task_start_overs (
 workspace_id VARCHAR(255) NOT NULL,
 task_id VARCHAR(255) NOT NULL,
 request_id VARCHAR(200) NOT NULL,
 successor_id VARCHAR(255) NOT NULL,
 reason TEXT NOT NULL,
 note TEXT NOT NULL,
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id,task_id,request_id),
 UNIQUE KEY task_start_over_task (workspace_id,task_id),
 UNIQUE KEY task_start_over_successor (workspace_id,successor_id),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
