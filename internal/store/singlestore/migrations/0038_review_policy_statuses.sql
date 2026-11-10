-- req-risk-based-review AC-10.1 and AC-10.2, component-git-delivery
-- invariant 37, and feature-risk-based-review RR-9. One row per head of an
-- enforcing task records the conveyor/review-policy publication. SingleStore
-- has no CHECK, so the Go writers validate the state, the desired state, and
-- the revision. Every write runs under the task operation lock and records
-- the row's revision on a claim row, so a reader waits until the latest write
-- is visible across partitions.
CREATE ROWSTORE TABLE IF NOT EXISTS review_policy_statuses (
 workspace_id VARCHAR(191) NOT NULL,
 task_id VARCHAR(191) NOT NULL,
 head_sha VARCHAR(191) NOT NULL,
 state VARCHAR(32) NOT NULL,
 desired VARCHAR(32) NOT NULL,
 revision INT NOT NULL,
 payload_json JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, task_id, head_sha),
 SHARD KEY (workspace_id, task_id)
) COLLATE utf8mb4_bin;
