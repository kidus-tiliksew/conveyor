-- feature-requirement-insights RI-5, DEC-87 (2), req-requirement-insights
-- AC-2.4, AC-2.6, and AC-2.7. At most one generated headline per requirement,
-- keyed and sharded with the workspace. Restart-safe: the single statement is
-- IF NOT EXISTS. No backfill.
CREATE ROWSTORE TABLE IF NOT EXISTS requirement_insight_headlines (
 workspace_id VARCHAR(255) NOT NULL,
 requirement_id VARCHAR(255) NOT NULL,
 fingerprint LONGTEXT NOT NULL,
 headline LONGTEXT NOT NULL,
 model LONGTEXT NOT NULL,
 tokens_in BIGINT NOT NULL,
 tokens_out BIGINT NOT NULL,
 generated_by LONGTEXT NOT NULL,
 generated_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, requirement_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
