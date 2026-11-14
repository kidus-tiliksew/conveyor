-- feature-risk-based-review RR-11 and RR-15, req-risk-based-review AC-5.6 and
-- AC-12.1 to AC-12.3, and component-risk-review invariant 12. SingleStore has
-- no CHECK, so the Go writers validate the 'risk' stage, the 'risk_agent'
-- source, and the subject, and every write runs under the task operation
-- lock.
--
-- One row per risk order names what it rates: exactly one plan version, or
-- one head with its base and the changed paths and matched rules copied at
-- creation. A task has at most one risk order per subject.
CREATE ROWSTORE TABLE IF NOT EXISTS risk_orders (
 workspace_id VARCHAR(191) NOT NULL,
 task_id VARCHAR(191) NOT NULL,
 work_order_id VARCHAR(191) NOT NULL,
 subject_key VARCHAR(191) NOT NULL,
 plan_version INT NOT NULL DEFAULT 0,
 head_sha VARCHAR(191) NOT NULL DEFAULT '',
 base_sha VARCHAR(191) NOT NULL DEFAULT '',
 changed_paths JSON NOT NULL,
 matched_rules JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, task_id, work_order_id),
 UNIQUE KEY risk_orders_subject (workspace_id, task_id, subject_key),
 SHARD KEY (workspace_id, task_id)
) COLLATE utf8mb4_bin;

-- The claim ledger keeps each accepted claim's session ID and client-token
-- hash, because a release, expiry, or cancellation clears them from the work
-- order row. A risk claim is refused for any identity that claimed a plan,
-- implementation, or review order of the task. No event or read exposes the
-- hash.
CREATE ROWSTORE TABLE IF NOT EXISTS work_order_claim_identities (
 workspace_id VARCHAR(191) NOT NULL,
 task_id VARCHAR(191) NOT NULL,
 work_order_id VARCHAR(191) NOT NULL,
 attempt_id VARCHAR(191) NOT NULL,
 stage VARCHAR(32) NOT NULL,
 session_id VARCHAR(255) NOT NULL DEFAULT '',
 client_token_hash VARCHAR(128) NOT NULL DEFAULT '',
 claimed_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, task_id, work_order_id, attempt_id),
 SHARD KEY (workspace_id, task_id)
) COLLATE utf8mb4_bin;

-- Claims before this migration keep the identity their order row still holds.
INSERT IGNORE INTO work_order_claim_identities
 (workspace_id, task_id, work_order_id, attempt_id, stage, session_id, client_token_hash, claimed_at)
SELECT workspace_id, task_id, id, COALESCE(NULLIF(attempt_id, ''), 'migration-0040'), stage, session_id, client_token_hash,
 COALESCE(execution_started_at, updated_at, created_at)
FROM work_orders
WHERE session_id <> '' OR client_token_hash <> '';
