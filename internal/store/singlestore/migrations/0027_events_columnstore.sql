-- Events are append-only timelines, not mutable workspace projections.
-- event_shard_key is task_id when present, otherwise event/<id> for copied
-- rows or event/<random id> for new rows, so task-less events also spread.
-- Stop all existing Conveyor writers before upgrading. The startup migration
-- lock serializes migrators, not API processes already running the old binary.
-- migrateEventsColumnstore copies in bounded INSERT SELECT batches, preserves
-- IDs, resumes interrupted copies and ALTER TABLE renames, synchronizes every
-- aggregator's AUTO_INCREMENT allocator, then drops the old rowstore.
-- SingleStore cannot atomically rename two tables. Do not serve traffic until
-- this migration completes. Keep enough disk for both complete copies.
CREATE TABLE IF NOT EXISTS events_columnstore_0027 (
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
