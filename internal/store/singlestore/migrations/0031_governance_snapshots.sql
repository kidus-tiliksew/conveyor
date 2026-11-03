-- Content-addressed governance snapshots. work_orders keep a digest
-- reference and readers join the body. Identical claim-time JSON is stored once
-- (component-persistence, Schema domains). Append-only write-once ~2 MB rows
-- use columnstore like events. Digest uniqueness is a HASH unique key so
-- INSERT IGNORE still dedupes. SingleStore DDL commits implicitly and has no
-- ADD/DROP COLUMN IF EXISTS, so the work_orders digest column, copy, and drop
-- live in Go behind information_schema checks.
CREATE TABLE IF NOT EXISTS governance_snapshots (
  workspace_id VARCHAR(255) NOT NULL,
  digest CHAR(64) NOT NULL,
  content JSON NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  SHARD KEY (workspace_id),
  SORT KEY (workspace_id, digest),
  UNIQUE KEY governance_snapshots_digest_idx (workspace_id, digest) USING HASH
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
