-- Document event pages retain workspace and document membership without scanning
-- workspace history (req-accounts-and-membership AC-4.4; component-persistence).
-- EXPLAIN on 370k events showed a parallel sequential scan for this branch.
CREATE INDEX events_system_design_document_idx
    ON events (workspace_id, (payload_json->>'document_id'), at, id)
    WHERE kind LIKE 'system_design.%';
