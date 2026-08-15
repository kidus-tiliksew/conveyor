-- Requirement list/detail reads select taskless document events by workspace
-- and payload requirement ID. Without this expression index every requirement
-- performs a parallel scan of the append-only events table.
CREATE INDEX IF NOT EXISTS events_requirement_document_idx
    ON events (workspace_id, ((payload_json ->> 'requirement_id')), at, id)
    WHERE task_id IS NULL;

-- Board activity derives its lifecycle, user-request, and forge markers from a
-- fixed event-kind vocabulary for a bounded task page. Keep that lookup off a
-- scan of each task's complete append-only history.
CREATE INDEX IF NOT EXISTS events_task_kind_marker_idx
    ON events (task_id, kind, at, id)
    WHERE task_id IS NOT NULL;
