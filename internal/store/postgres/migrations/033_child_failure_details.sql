ALTER TABLE work_orders ADD COLUMN last_failure_detail text NOT NULL DEFAULT '';
ALTER TABLE work_orders ADD COLUMN retry_suppression_reason text NOT NULL DEFAULT '';

CREATE TABLE harness_model_failures (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    harness text NOT NULL,
    model text NOT NULL,
    detail text NOT NULL,
    work_order_id text NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, harness, model)
);
