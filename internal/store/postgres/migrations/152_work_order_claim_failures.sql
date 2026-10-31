-- component-work-orders (Claim admission) and component-persistence (Schema
-- domains). One bounded record per work order of its consecutive claims that
-- failed with store.ErrBackendOperation. A successful claim deletes the row.
-- No driver text is stored. No backfill.
CREATE TABLE IF NOT EXISTS work_order_claim_failures (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    work_order_id text NOT NULL,
    consecutive_failures integer NOT NULL CHECK (consecutive_failures >= 1),
    first_failed_at timestamptz NOT NULL,
    last_failed_at timestamptz NOT NULL,
    error_class text NOT NULL CHECK (error_class <> ''),
    PRIMARY KEY (workspace_id, work_order_id),
    FOREIGN KEY (workspace_id, work_order_id)
        REFERENCES work_orders(workspace_id, id) ON DELETE CASCADE
);
