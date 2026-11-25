-- component-environments, feature-production-feedback-live FB-1,
-- req-environments REQ-1 and REQ-2, req-security-boundaries AC-2.8,
-- DEC-148(6), and DEC-139(2). An environment is a workspace record for one
-- place the software runs. The three tables share the workspace as shard key,
-- so every row of one mutation is on one partition. The Go writers hold the
-- workspace environment claim and keep the uniqueness, length, and limit
-- rules. revision is the claim witness: a writer waits until the row that the
-- claim names shows the revision the claim carries (component-persistence
-- invariant 8). The writers update rows in place, so the tables are rowstore.
-- No dispatch, claim, gate, review, merge, next-action, lineage, or
-- work-order query reads these tables (component-environments invariant 1).
-- Restart-safe: every statement is IF NOT EXISTS. No backfill.
CREATE ROWSTORE TABLE IF NOT EXISTS environments (
 workspace_id VARCHAR(255) NOT NULL,
 id VARCHAR(64) NOT NULL,
 name VARCHAR(255) NOT NULL,
 name_fold VARCHAR(255) NOT NULL,
 tracked_branches LONGTEXT NOT NULL,
 github_environments LONGTEXT NOT NULL,
 version_rule_kind VARCHAR(32) NULL,
 version_rule_text LONGTEXT NULL,
 timing_window_minutes INT NOT NULL,
 archived TINYINT NOT NULL,
 created_by VARCHAR(255) NOT NULL,
 created_at DATETIME(6) NOT NULL,
 revision BIGINT NOT NULL,
 PRIMARY KEY (workspace_id, id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- A key is stored only as the 32-byte SHA-256 of its value. The store checks
-- the length before every write (component-environments invariant 11).
CREATE ROWSTORE TABLE IF NOT EXISTS environment_keys (
 workspace_id VARCHAR(255) NOT NULL,
 id VARCHAR(64) NOT NULL,
 environment_id VARCHAR(64) NOT NULL,
 label VARCHAR(255) NOT NULL,
 key_hash VARBINARY(32) NOT NULL,
 created_by VARCHAR(255) NOT NULL,
 created_at DATETIME(6) NOT NULL,
 last_used_at DATETIME(6) NULL,
 revoked_at DATETIME(6) NULL,
 PRIMARY KEY (workspace_id, id),
 KEY environment_keys_hash_idx (key_hash),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- One row for each suggested or ignored GitHub environment name, keyed by the
-- SHA-256 of the name fold. A row ignored before any report has no repository
-- and no report time (component-environments invariant 16).
CREATE ROWSTORE TABLE IF NOT EXISTS environment_suggestions (
 workspace_id VARCHAR(255) NOT NULL,
 name_digest VARCHAR(64) NOT NULL,
 github_environment LONGTEXT NOT NULL,
 repository LONGTEXT NULL,
 last_reported_at DATETIME(6) NULL,
 ignored TINYINT NOT NULL,
 revision BIGINT NOT NULL,
 PRIMARY KEY (workspace_id, name_digest),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
