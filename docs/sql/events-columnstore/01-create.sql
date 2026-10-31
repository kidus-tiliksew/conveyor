-- Operator-run only. Select the intended database before sourcing this file.
-- No IF NOT EXISTS: an existing target requires inspection, not silent reuse.
-- This file never renames or drops the legacy events table.
CREATE TABLE events_columnstore (
 id BIGINT NOT NULL AUTO_INCREMENT,
 event_shard_key VARCHAR(255) NOT NULL,
 task_id VARCHAR(255),
 job_id LONGTEXT,
 kind LONGTEXT NOT NULL,
 actor_id LONGTEXT NOT NULL,
 actor_role LONGTEXT NOT NULL,
 payload_json JSON NOT NULL,
 at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 workspace_id VARCHAR(255) NOT NULL,
 SHARD KEY (event_shard_key),
 SORT KEY (workspace_id, task_id, at, id),
 UNIQUE KEY events_task_event_idx (event_shard_key, workspace_id, id) USING HASH,
 KEY events_workspace_id_idx (workspace_id, id) USING HASH,
 KEY events_task_idx (workspace_id, task_id) USING HASH
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
