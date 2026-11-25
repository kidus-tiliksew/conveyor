-- component-production-feedback, feature-production-feedback-live FB-3,
-- req-production-feedback REQ-1 (AC-1.4) and REQ-3, req-environments
-- AC-1.2, and DEC-149(1) and (4). Every table shares the workspace as shard
-- key with the feedback tables of migration 0051. The Go writers hold the
-- workspace feedback claim, keep the value rules that SingleStore has no
-- CHECK constraint for, and stamp feedback_status_state as the claim's
-- witness (component-persistence invariant 8). Rows are updated in place, so
-- every table is rowstore. No dispatch, claim, gate, review, merge,
-- next-action, lineage, or work-order query reads these tables
-- (component-production-feedback invariant 1). Restart-safe: every statement
-- is IF NOT EXISTS. No backfill, and no write to the task ledger.

-- The stored answer for each version, keyed by the FB-2 version identity. A
-- resolved row holds the repository and the full commit ID and is never
-- changed; an unresolved row keeps its reason and the earliest time a later
-- signal can try it again (invariants 7 and 8).
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_resolutions (
 workspace_id VARCHAR(255) NOT NULL,
 environment_id VARCHAR(64) NOT NULL,
 service_digest VARCHAR(64) NOT NULL,
 version_digest VARCHAR(64) NOT NULL,
 service VARCHAR(255) NOT NULL,
 reported_repository LONGTEXT NOT NULL,
 reported_commit VARCHAR(64) NOT NULL,
 reported_version LONGTEXT NOT NULL,
 resolved_repository LONGTEXT NOT NULL,
 resolved_commit VARCHAR(64) NOT NULL,
 reason VARCHAR(64) NOT NULL,
 attempted_at DATETIME(6) NOT NULL,
 next_attempt_at DATETIME(6) NULL,
 PRIMARY KEY (workspace_id, environment_id, service_digest, version_digest),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- GitHub's answer for each pair of commits compared. A stored answer is
-- never asked again (invariant 14).
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_comparisons (
 workspace_id VARCHAR(255) NOT NULL,
 repository VARCHAR(255) NOT NULL,
 base_commit VARCHAR(64) NOT NULL,
 head_commit VARCHAR(64) NOT NULL,
 status VARCHAR(32) NOT NULL,
 compared_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, repository, base_commit, head_commit),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- Admitted comparisons and branch reads: the rows of the last minute count
-- against the workspace's 60-a-minute allowance, and a row in its lease keeps
-- a second request for the same pair out.
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_admissions (
 workspace_id VARCHAR(255) NOT NULL,
 repository VARCHAR(255) NOT NULL,
 base_commit VARCHAR(64) NOT NULL,
 head_ref VARCHAR(255) NOT NULL,
 admitted_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, repository, base_commit, head_ref),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- The newest observed head of each tracked branch.
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_branch_heads (
 workspace_id VARCHAR(255) NOT NULL,
 repository VARCHAR(255) NOT NULL,
 branch VARCHAR(255) NOT NULL,
 head_commit VARCHAR(64) NOT NULL,
 observed_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, repository, branch),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- The witness of the workspace feedback claim: each claimed write stamps the
-- next revision, so the next holder waits until it sees the last one.
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_status_state (
 workspace_id VARCHAR(255) NOT NULL,
 revision BIGINT NOT NULL,
 PRIMARY KEY (workspace_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
