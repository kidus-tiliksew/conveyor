ALTER TABLE work_orders ADD COLUMN last_attempt_outcome text NOT NULL DEFAULT '';
ALTER TABLE work_orders ADD COLUMN last_failure_message text NOT NULL DEFAULT '';
ALTER TABLE work_orders ADD COLUMN last_failure_exit_status integer;
ALTER TABLE work_orders ADD COLUMN last_failure_at timestamptz;
ALTER TABLE work_orders ADD COLUMN automatic_retry_count integer NOT NULL DEFAULT 0;
ALTER TABLE work_orders ADD COLUMN next_retry_at timestamptz;
ALTER TABLE work_orders ADD COLUMN retry_suppressed boolean NOT NULL DEFAULT false;

ALTER TABLE work_orders ADD CONSTRAINT work_orders_last_attempt_outcome_check CHECK (
    last_attempt_outcome IN ('', 'child_failure', 'released', 'cancelled', 'expired')
);
ALTER TABLE work_orders ADD CONSTRAINT work_orders_automatic_retry_count_check CHECK (automatic_retry_count >= 0);

CREATE TABLE work_order_recoveries (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    work_order_id text NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    request_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, work_order_id, request_id)
);
