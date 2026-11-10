-- req-risk-based-review AC-8.3 to AC-8.8, component-risk-review invariants 9
-- and 10, and feature-risk-based-review RR-7. One row per human approval or author
-- waiver of an enforcing task's head, unique per task, head, review round,
-- kind, and person, so a replayed or concurrent request by one person records
-- one row. SingleStore has no CHECK, so the Go writers validate the kind and
-- values, and every write runs under the task operation lock.
CREATE ROWSTORE TABLE IF NOT EXISTS task_risk_approvals (
 workspace_id VARCHAR(191) NOT NULL,
 task_id VARCHAR(191) NOT NULL,
 head_sha VARCHAR(191) NOT NULL,
 review_round INT NOT NULL,
 kind VARCHAR(32) NOT NULL,
 user_id VARCHAR(191) NOT NULL,
 payload_json JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, task_id, head_sha, review_round, kind, user_id),
 SHARD KEY (workspace_id, task_id)
) COLLATE utf8mb4_bin;
