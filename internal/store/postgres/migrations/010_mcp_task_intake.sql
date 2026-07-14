-- MCP intake retries must resolve to the original durable task across daemon
-- restarts and concurrent clients (spec §21.5).
ALTER TABLE tasks ADD COLUMN intake_key text;

CREATE UNIQUE INDEX tasks_workspace_intake_key_idx
    ON tasks (workspace_id, intake_key)
    WHERE intake_key IS NOT NULL;
