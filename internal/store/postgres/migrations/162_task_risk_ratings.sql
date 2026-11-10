-- req-risk-based-review AC-5.5, AC-5.6, REQ-6; component-risk-review, State
-- and failure model; feature-risk-based-review RR-3, RR-8. A task's risk
-- ratings are append-only and unique per task and rating key: a path rating
-- once per head, a reviewer rating once per review work order, a plan rating
-- once per plan version, and a person raise once per raised tier. seq orders a
-- task's ratings in the order they were recorded.
CREATE TABLE IF NOT EXISTS task_risk_ratings (
 workspace_id text NOT NULL,
 task_id text NOT NULL,
 rating_key text NOT NULL,
 seq integer NOT NULL CHECK (seq > 0),
 source text NOT NULL CHECK (source IN ('plan', 'path_rule', 'reviewer', 'person')),
 tier text NOT NULL CHECK (tier IN ('low', 'medium', 'high')),
 payload_json jsonb NOT NULL,
 created_at timestamptz NOT NULL,
 PRIMARY KEY (workspace_id, task_id, rating_key),
 UNIQUE (workspace_id, task_id, seq),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);
