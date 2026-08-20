-- Migration 105: bounded, observational attempt output (req-260820-221be8).

ALTER TABLE work_orders
    ADD CONSTRAINT work_orders_workspace_id_id_key UNIQUE (workspace_id, id);

CREATE TABLE work_order_activity_snapshots (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    work_order_id text NOT NULL,
    attempt_id text NOT NULL CHECK (attempt_id <> ''),
    content text NOT NULL CHECK (octet_length(content) <= 4096),
    captured_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, work_order_id),
    FOREIGN KEY (workspace_id, work_order_id)
        REFERENCES work_orders(workspace_id, id) ON DELETE CASCADE
);

CREATE TABLE work_order_transcript_captures (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    work_order_id text NOT NULL,
    attempt_id text NOT NULL CHECK (attempt_id <> ''),
    content text NOT NULL CHECK (octet_length(content) <= 4194304),
    termination_reason text NOT NULL CHECK (termination_reason <> ''),
    truncated boolean NOT NULL DEFAULT false,
    captured_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, work_order_id, attempt_id),
    FOREIGN KEY (workspace_id, work_order_id)
        REFERENCES work_orders(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX work_order_transcript_captures_history_idx
    ON work_order_transcript_captures (workspace_id, work_order_id, captured_at, attempt_id);
