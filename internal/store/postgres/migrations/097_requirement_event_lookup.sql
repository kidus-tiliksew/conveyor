-- Requirement list/detail reads select taskless document events by workspace
-- and payload requirement ID. Without this expression index every requirement
-- performs a parallel scan of the append-only events table.
CREATE INDEX IF NOT EXISTS events_requirement_document_idx
    ON events (workspace_id, ((payload_json ->> 'requirement_id')), at, id)
    WHERE task_id IS NULL;
