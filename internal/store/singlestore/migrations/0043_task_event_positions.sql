CREATE TABLE IF NOT EXISTS task_event_positions (
    workspace_id VARCHAR(255) NOT NULL PRIMARY KEY,
    position BIGINT NOT NULL
);
