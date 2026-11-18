-- component-persistence invariants 35 and 42. SingleStore migration 0046
-- records, beside task_event_positions, the identity of every task event a
-- position counts, so a SingleStore peer advances its mark only once those
-- events are visible. PostgreSQL creates the same table and never reads or
-- writes it, as with task_event_positions (migration 170): its peers learn
-- of commits through pg_notify.
CREATE TABLE IF NOT EXISTS task_event_witnesses (
    workspace_id TEXT NOT NULL,
    position BIGINT NOT NULL,
    event_shard_key TEXT NOT NULL,
    event_id BIGINT NOT NULL,
    PRIMARY KEY (workspace_id, position, event_shard_key, event_id)
);
