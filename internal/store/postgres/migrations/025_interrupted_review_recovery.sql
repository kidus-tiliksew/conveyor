-- Same-round recovery reauthorizes only incomplete interrupted seats. The
-- original result is retained so exact request retries remain stable even
-- after a recovered seat is claimed or completed.
CREATE TABLE interrupted_review_recoveries (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    request_id text NOT NULL,
    task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    review_round integer NOT NULL CHECK (review_round > 0),
    actor_id text NOT NULL,
    result_json jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id)
);

CREATE INDEX interrupted_review_recoveries_task_round_idx
    ON interrupted_review_recoveries (workspace_id, task_id, review_round);
