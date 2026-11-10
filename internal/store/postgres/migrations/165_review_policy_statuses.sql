-- req-risk-based-review AC-10.1 and AC-10.2, component-git-delivery
-- invariant 37, and feature-risk-based-review RR-9. One row per head of an
-- enforcing task records the conveyor/review-policy publication: the state the
-- head's requirements last called for, the state GitHub last accepted, and the
-- queued, retrying, published, or failed lifecycle. Every write runs under the
-- task operation lock and appends its review_policy_status.publication_* event
-- and, for a queue, the head's review_policy_status job in the same
-- transaction.
CREATE TABLE IF NOT EXISTS review_policy_statuses (
 workspace_id text NOT NULL,
 task_id text NOT NULL,
 head_sha text NOT NULL CHECK (head_sha <> ''),
 state text NOT NULL CHECK (state IN ('queued', 'retrying', 'published', 'failed')),
 desired text NOT NULL CHECK (desired IN ('pending', 'success')),
 revision integer NOT NULL CHECK (revision > 0),
 payload_json jsonb NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (workspace_id, task_id, head_sha),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS review_policy_statuses_open_idx
 ON review_policy_statuses (workspace_id, created_at) WHERE state <> 'published';
