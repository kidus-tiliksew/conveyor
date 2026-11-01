-- req-quiet-idle-runtime AC-5.1, AC-5.2; component-durable-queue (The job
-- index) and component-persistence (Schema domains). One row per job stream,
-- written by pglog in the transaction of each job append. pglog.EnsureSchema
-- runs the same DDL before numbered migrations, so this file is a no-op on a
-- database it already reached; the runner then calls pglog.FillJobIndex in
-- this migration's transaction, which folds every existing job stream once.
-- No event_log row is deleted or rewritten, and no events row is written.
CREATE TABLE IF NOT EXISTS event_log_job_index (
    workspace_id text NOT NULL,
    stream_id    text NOT NULL,
    kind         text NOT NULL,
    state        text NOT NULL,
    generation   integer NOT NULL,
    version      bigint NOT NULL,
    next_at      timestamptz,
    claimed_at   timestamptz,
    PRIMARY KEY (workspace_id, stream_id)
);
CREATE INDEX IF NOT EXISTS event_log_job_index_unfinished
    ON event_log_job_index (workspace_id, state)
    WHERE state IN ('available', 'scheduled', 'running');
