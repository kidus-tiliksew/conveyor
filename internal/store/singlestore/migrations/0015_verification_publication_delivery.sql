-- feature-verification-kit-execution VK-9, DEC-43. Shared locked guards enforce invariants.
CREATE ROWSTORE TABLE IF NOT EXISTS verification_publication_deliveries (
 workspace_id VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
 source_publication_id VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
 pr_key VARCHAR(64) COLLATE utf8mb4_bin NOT NULL,
 generation BIGINT NOT NULL,
 state VARCHAR(32) COLLATE utf8mb4_bin NOT NULL,
 body JSON NOT NULL,
 next_attempt_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY verification_delivery_generation (workspace_id,pr_key,generation),
 UNIQUE KEY verification_delivery_source_pr (workspace_id,source_publication_id,pr_key),
 KEY verification_delivery_source (workspace_id,task_id,context_id,source_publication_id),
 KEY verification_delivery_pending (workspace_id,state,next_attempt_at,pr_key,generation),
 SHARD KEY (workspace_id)
);
