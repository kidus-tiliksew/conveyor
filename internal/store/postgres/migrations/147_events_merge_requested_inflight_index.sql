-- component-monitor-drift; component-persistence (DEC-38).
-- Keep the in-flight merge monitor lookup on the repository/time range.
CREATE INDEX events_merge_requested_inflight_idx
    ON events (workspace_id, (payload_json->>'repository'), at, id)
    WHERE kind = 'merge.requested';
