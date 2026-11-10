-- req-risk-based-review AC-8.3 to AC-8.8, component-risk-review invariants 9
-- and 10, and feature-risk-based-review RR-7. One row per human approval or author
-- waiver of an enforcing task's head. A row binds to the head and the review
-- round at which it was recorded and is unique per task, head, round, kind,
-- and person, so a replayed or concurrent request by one person records one
-- row. Rows are append-only audit history: a new head reaches the merge gate
-- through a new review round, and approvals of an earlier round never count.
CREATE TABLE IF NOT EXISTS task_risk_approvals (
 workspace_id text NOT NULL,
 task_id text NOT NULL,
 head_sha text NOT NULL CHECK (head_sha <> ''),
 review_round integer NOT NULL CHECK (review_round > 0),
 kind text NOT NULL CHECK (kind IN ('approval', 'author_waiver')),
 user_id text NOT NULL CHECK (user_id <> ''),
 seq bigserial NOT NULL,
 payload_json jsonb NOT NULL,
 created_at timestamptz NOT NULL,
 PRIMARY KEY (workspace_id, task_id, head_sha, review_round, kind, user_id),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);
