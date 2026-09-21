-- VK-6. Additive retry-safe DDL. SingleStore enforces lifecycle and
-- immutable-row predicates under the shared task/workspace locks in Go.
-- Artifact roles are validated by core.ArtifactRole.Valid, not a SQL enum.
CREATE ROWSTORE TABLE IF NOT EXISTS verification_contexts (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
CREATE ROWSTORE TABLE IF NOT EXISTS verification_selections (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
CREATE ROWSTORE TABLE IF NOT EXISTS verification_obligations (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
CREATE ROWSTORE TABLE IF NOT EXISTS verification_attempts (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
CREATE ROWSTORE TABLE IF NOT EXISTS verification_operations (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
CREATE ROWSTORE TABLE IF NOT EXISTS verification_evidence (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
CREATE ROWSTORE TABLE IF NOT EXISTS verification_evidence_links (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
CREATE ROWSTORE TABLE IF NOT EXISTS verification_publications (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
CREATE ROWSTORE TABLE IF NOT EXISTS verification_upload_chunks (
 workspace_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 task_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 context_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 run_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 state VARCHAR(32) NOT NULL DEFAULT '',
 body JSON NOT NULL,
 expires_at DATETIME(6),
 PRIMARY KEY (workspace_id,id),
 UNIQUE KEY (workspace_id,key_hash),
 KEY task_context (workspace_id,task_id,context_id),
 SHARD KEY (workspace_id)
);
