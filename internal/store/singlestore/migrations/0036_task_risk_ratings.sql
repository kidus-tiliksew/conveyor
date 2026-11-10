-- req-risk-based-review AC-5.5, AC-5.6, and REQ-6, component-risk-review
-- (State and failure model), and feature-risk-based-review RR-3 and RR-8. A
-- task's risk ratings are append-only and unique per task and rating key. A
-- path rating is recorded once per head, a reviewer rating once per review
-- work order, a plan rating once per plan version, and a person raise once
-- per raised tier. seq orders a task's ratings in the order they were
-- recorded. SingleStore has no CHECK, so the Go writers enforce the source and
-- tier value sets, and every write runs under the task operation lock.
CREATE ROWSTORE TABLE IF NOT EXISTS task_risk_ratings (
 workspace_id VARCHAR(191) NOT NULL,
 task_id VARCHAR(191) NOT NULL,
 rating_key VARCHAR(191) NOT NULL,
 seq INT NOT NULL,
 source VARCHAR(32) NOT NULL,
 tier VARCHAR(16) NOT NULL,
 payload_json JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, task_id, rating_key),
 UNIQUE KEY task_risk_ratings_seq (workspace_id, task_id, seq),
 SHARD KEY (workspace_id, task_id)
) COLLATE utf8mb4_bin;
