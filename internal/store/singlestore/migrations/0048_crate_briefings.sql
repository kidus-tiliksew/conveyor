-- feature-crate-insights CI-2, component-crates invariant 29 and Storage,
-- req-crate-insights AC-5.2, AC-5.4, and AC-5.6, and DEC-142(2). At most one
-- generated briefing per crate, keyed and sharded with the workspace. The
-- writer selects the row FOR UPDATE, so the table is rowstore (DEC-139(2)).
-- look_first holds the validated entries as JSON array text. The token
-- counts are stored and never served. Restart-safe: the single statement is
-- IF NOT EXISTS. No backfill.
CREATE ROWSTORE TABLE IF NOT EXISTS crate_briefings (
 workspace_id VARCHAR(255) NOT NULL,
 crate_id VARCHAR(255) NOT NULL,
 fingerprint LONGTEXT NOT NULL,
 headline LONGTEXT NOT NULL,
 account LONGTEXT NOT NULL,
 look_first LONGTEXT NOT NULL,
 model LONGTEXT NOT NULL,
 tokens_in BIGINT NOT NULL,
 tokens_out BIGINT NOT NULL,
 generated_by LONGTEXT NOT NULL,
 generated_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, crate_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
