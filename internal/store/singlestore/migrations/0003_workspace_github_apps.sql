-- DEC-41: encrypted app identity. Immutable, restart-safe migration.
CREATE ROWSTORE TABLE IF NOT EXISTS workspace_github_apps (
 workspace_id VARCHAR(255) NOT NULL,
 app_id BIGINT NOT NULL,
 app_slug TEXT NOT NULL,
 client_id TEXT NOT NULL,
 private_key_nonce VARBINARY(12) NOT NULL,
 private_key_ciphertext LONGBLOB NOT NULL,
 installation_id BIGINT,
 installation_account TEXT,
 connected_by TEXT NOT NULL,
 connected_at DATETIME(6) NOT NULL,
 installation_recorded_at DATETIME(6),
 PRIMARY KEY (workspace_id),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
