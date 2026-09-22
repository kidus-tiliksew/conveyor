-- VK-4. req-verification-kits REQ-7/AC-7.3. Immutable grants and append-only revocations.
CREATE ROWSTORE TABLE IF NOT EXISTS verification_permission_grants (
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
CREATE ROWSTORE TABLE IF NOT EXISTS verification_permission_revocations (
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
