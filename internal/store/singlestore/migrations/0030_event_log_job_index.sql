-- req-quiet-idle-runtime AC-5.1 and AC-5.2, component-durable-queue (The job
-- index), and component-persistence (Schema domains). One row per job stream,
-- written by s2log in the transaction of each job append. s2log.EnsureSchema
-- runs the same DDL before numbered migrations. The runner then calls
-- s2log.FillJobIndex, which folds every existing job stream once, and records
-- this version only after the fill succeeds. No event_log row is deleted or
-- rewritten.
CREATE ROWSTORE TABLE IF NOT EXISTS event_log_job_index (
 workspace_id VARCHAR(255) NOT NULL, stream_id VARCHAR(768) NOT NULL,
 kind VARCHAR(255) NOT NULL, state VARCHAR(32) NOT NULL, generation INT NOT NULL, version BIGINT NOT NULL,
 next_at DATETIME(6) NULL, claimed_at DATETIME(6) NULL,
 PRIMARY KEY (workspace_id, stream_id), KEY event_log_job_index_unfinished (workspace_id, state),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
