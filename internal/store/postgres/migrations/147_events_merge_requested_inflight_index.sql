-- component-monitor-drift; component-persistence (DEC-38).
-- Keep the in-flight merge monitor lookup on the repository/time range.
-- CREATE INDEX takes a SHARE lock on events for this migration transaction.
CREATE INDEX IF NOT EXISTS events_merge_requested_inflight_idx
    ON events (workspace_id, (payload_json->>'repository'), at, id)
    WHERE kind = 'merge.requested';
