-- An operator retry starts a new immutable review round. Request IDs are
-- workspace-wide idempotency keys so reuse for another task or payload fails
-- closed instead of creating ambiguous review history.
CREATE TABLE review_round_retries (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    request_id text NOT NULL,
    task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    reason text NOT NULL,
    prior_round integer NOT NULL CHECK (prior_round > 0),
    new_round integer NOT NULL CHECK (new_round = prior_round + 1),
    pr_head text NOT NULL,
    actor_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id)
);

CREATE UNIQUE INDEX review_round_retries_task_round_idx
    ON review_round_retries (workspace_id, task_id, new_round);
