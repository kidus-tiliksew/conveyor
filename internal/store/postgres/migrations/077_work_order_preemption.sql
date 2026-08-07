CREATE TABLE work_order_preemptions (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    request_id text NOT NULL,
    work_order_id text NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    request_json jsonb NOT NULL,
    result_json jsonb NOT NULL,
    revoked_attempt_id text NOT NULL,
    revoked_session_id text NOT NULL,
    revoked_worker_id text NOT NULL,
    actor_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id)
);

CREATE INDEX work_order_preemptions_claim_idx
    ON work_order_preemptions (workspace_id, work_order_id, revoked_worker_id, revoked_session_id);

ALTER TABLE work_orders DROP CONSTRAINT work_orders_last_attempt_outcome_check;
ALTER TABLE work_orders ADD CONSTRAINT work_orders_last_attempt_outcome_check CHECK (
    last_attempt_outcome IN ('', 'child_failure', 'stalled', 'released', 'cancelled', 'expired', 'preempted')
);
