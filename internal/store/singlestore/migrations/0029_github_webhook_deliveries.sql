-- req-quiet-idle-runtime AC-3.2, AC-3.3, AC-3.6 and DEC-99(5),
-- component-identity-membership (The webhook secret) and component-persistence
-- (Schema domains). The workspace GitHub webhook secret is sealed like the App
-- private key, and the store deletes it in the same transaction as its App row.
-- Each accepted delivery is recorded once per workspace, and a repeated delivery
-- ID is refused by the primary key. Go checks non-empty IDs and events.
CREATE ROWSTORE TABLE IF NOT EXISTS workspace_github_webhook_secrets (
 workspace_id VARCHAR(255) NOT NULL,
 secret_nonce VARBINARY(12) NOT NULL,
 secret_ciphertext LONGBLOB NOT NULL,
 set_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS github_webhook_deliveries (
 workspace_id VARCHAR(255) NOT NULL,
 delivery_id VARCHAR(255) NOT NULL,
 event VARCHAR(255) NOT NULL,
 received_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, delivery_id),
 KEY github_webhook_deliveries_received (workspace_id, received_at),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
