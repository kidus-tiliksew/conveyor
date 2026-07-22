CREATE TABLE task_setup_changes (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    request_id text NOT NULL,
    task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    request_json jsonb NOT NULL,
    result_json jsonb NOT NULL,
    actor_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id)
);

CREATE INDEX task_setup_changes_task_idx
    ON task_setup_changes (workspace_id, task_id, created_at);

ALTER TABLE work_orders
    ADD COLUMN review_superseded boolean NOT NULL DEFAULT false,
    ADD COLUMN required_effort text NOT NULL DEFAULT ''
        CHECK (required_effort IN ('', 'low', 'medium', 'high'));

DROP INDEX work_orders_review_seat_idx;
CREATE UNIQUE INDEX work_orders_review_seat_idx
    ON work_orders (workspace_id, task_id, review_round, review_seat)
    WHERE stage = 'review' AND review_round > 0 AND NOT review_superseded;
