-- Activity summaries and incremental SSE reads order by the append-only event
-- identity, not event time. Keep those reads index-only by task.
CREATE INDEX events_task_id_idx ON events (task_id, id);

-- Resolve the latest job deterministically without sorting all attempts.
CREATE INDEX jobs_task_started_id_idx ON jobs (task_id, started_at DESC, id DESC);
