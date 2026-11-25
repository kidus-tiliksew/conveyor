-- component-production-feedback, feature-production-feedback-live FB-2,
-- req-production-feedback REQ-1 and REQ-2, req-environments AC-2.4,
-- DEC-149(1), and DEC-139(2). The three tables share the workspace as shard
-- key with the environment tables, so every row of one signal is on one
-- partition, beside the environments row that the workspace environment claim
-- names as its witness (component-persistence invariant 8). The Go writers
-- hold that claim and keep the length and reference rules. They update rows
-- in place, so the tables are rowstore. An empty reported value was not
-- stated. No dispatch, claim, gate, review, merge, next-action, lineage, or
-- work-order query reads these tables (component-production-feedback
-- invariant 1). Restart-safe: every statement is IF NOT EXISTS. No backfill.
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_signals (
 workspace_id VARCHAR(255) NOT NULL,
 environment_id VARCHAR(64) NOT NULL,
 last_signal_at DATETIME(6) NOT NULL,
 next_signal_seconds INT NOT NULL,
 source VARCHAR(32) NOT NULL,
 source_label VARCHAR(255) NOT NULL,
 actor_id VARCHAR(255) NOT NULL,
 PRIMARY KEY (workspace_id, environment_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- One row for each deploy. A version that comes back after it stopped is a
-- new deploy (component-production-feedback invariants 5 and 6).
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_deploys (
 workspace_id VARCHAR(255) NOT NULL,
 id VARCHAR(64) NOT NULL,
 environment_id VARCHAR(64) NOT NULL,
 service VARCHAR(255) NOT NULL,
 reported_repository LONGTEXT NOT NULL,
 reported_commit VARCHAR(64) NOT NULL,
 reported_version LONGTEXT NOT NULL,
 resolved TINYINT NOT NULL,
 deployed_at DATETIME(6) NOT NULL,
 source VARCHAR(32) NOT NULL,
 source_label VARCHAR(255) NOT NULL,
 actor_id VARCHAR(255) NOT NULL,
 stopped_at DATETIME(6) NULL,
 stop_source VARCHAR(32) NULL,
 stop_source_label VARCHAR(255) NULL,
 stop_actor_id VARCHAR(255) NULL,
 PRIMARY KEY (workspace_id, id),
 KEY feedback_deploys_environment_idx (workspace_id, environment_id, deployed_at),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- What each service runs. The digests are the SHA-256 of the exact service
-- name and of the exact reported repository, commit, and version, so no
-- collation decides which versions are equal.
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_running_versions (
 workspace_id VARCHAR(255) NOT NULL,
 environment_id VARCHAR(64) NOT NULL,
 service_digest VARCHAR(64) NOT NULL,
 version_digest VARCHAR(64) NOT NULL,
 service VARCHAR(255) NOT NULL,
 reported_repository LONGTEXT NOT NULL,
 reported_commit VARCHAR(64) NOT NULL,
 reported_version LONGTEXT NOT NULL,
 resolved TINYINT NOT NULL,
 deploy_id VARCHAR(64) NOT NULL,
 deployed_at DATETIME(6) NOT NULL,
 reported_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, environment_id, service_digest, version_digest),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
