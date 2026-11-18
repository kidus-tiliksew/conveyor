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
 event_document_key AS (SHA2(JSON_EXTRACT_STRING(payload_json,'document_id'),256)) PERSISTED VARCHAR(64),
 event_requirement_key AS (SHA2(JSON_EXTRACT_STRING(payload_json,'requirement_id'),256)) PERSISTED VARCHAR(64),
 event_session_key AS (SHA2(JSON_EXTRACT_STRING(payload_json,'session_id'),256)) PERSISTED VARCHAR(64),
 SHARD KEY (event_shard_key),
 SORT KEY (workspace_id, task_id, at, id),
 UNIQUE KEY events_task_event_idx (event_shard_key, workspace_id, id) USING HASH,
 KEY events_workspace_id_idx (workspace_id, id) USING HASH,
 KEY events_task_idx (workspace_id, task_id) USING HASH,
 KEY events_document_key_idx (workspace_id, event_document_key) USING HASH,
 KEY events_requirement_key_idx (workspace_id, event_requirement_key) USING HASH,
 KEY events_session_key_idx (workspace_id, event_session_key) USING HASH
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
