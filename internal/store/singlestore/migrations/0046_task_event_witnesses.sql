-- component-persistence invariant 42: a peer's task-event mark advances only
-- once every task event it counts is visible. Each task-event commit records
-- the identity of every event its task_event_positions advance counts.
-- Rowstore, sharded and keyed by workspace like task_event_positions, so a
-- position and its witnesses commit on one partition. No baseline row is
-- seeded: a position without a witness is not accepted until a later commit
-- writes one.
CREATE ROWSTORE TABLE IF NOT EXISTS task_event_witnesses (
    workspace_id VARCHAR(255) NOT NULL,
    position BIGINT NOT NULL,
    event_shard_key VARCHAR(255) NOT NULL,
    event_id BIGINT NOT NULL,
    PRIMARY KEY (workspace_id, position, event_shard_key, event_id),
    SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
