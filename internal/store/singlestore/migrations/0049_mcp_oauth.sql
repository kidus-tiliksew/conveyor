-- req-mcp-oauth REQ-2 to REQ-4, req-security-boundaries AC-2.1, DEC-146,
-- DEC-139(2), component-identity-membership invariants 51 to 54, and
-- component-persistence invariants 8, 21, 36, 41, and 44. The four deployment
-- OAuth tables sit outside every workspace. Each is rowstore: writers lock
-- client, code, and grant rows with FOR UPDATE, and the token and code tables
-- are read by digest. A credential is stored only as the 32-byte SHA-256 of
-- its wire value. The store checks the length before every write. A grant and
-- its tokens share the grant ID as shard key, so a rotation or revocation
-- writes one partition, and every rotation and revocation decides on the
-- locked grant row, which names the one current refresh token. Restart-safe:
-- every statement is IF NOT EXISTS. A grant keeps the full verified client ID,
-- which a metadata-document URL can make longer than 255 characters, so the
-- column is LONGTEXT and carries no index. No backfill.
CREATE ROWSTORE TABLE IF NOT EXISTS oauth_clients (
 id VARCHAR(255) NOT NULL,
 name LONGTEXT NOT NULL,
 redirect_uris LONGTEXT NOT NULL,
 grant_types LONGTEXT NOT NULL,
 created_at DATETIME(6) NOT NULL,
 unused_expires_at DATETIME(6) NOT NULL,
 first_used_at DATETIME(6),
 PRIMARY KEY (id),
 SHARD KEY (id)
) COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS oauth_authorization_codes (
 id VARCHAR(255) NOT NULL,
 code_hash VARBINARY(32) NOT NULL,
 user_id VARCHAR(255) NOT NULL,
 client_id LONGTEXT NOT NULL,
 client_name LONGTEXT NOT NULL,
 redirect_uri LONGTEXT NOT NULL,
 resource LONGTEXT NOT NULL,
 code_challenge LONGTEXT NOT NULL,
 created_at DATETIME(6) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 consumed_at DATETIME(6),
 PRIMARY KEY (id),
 KEY oauth_authorization_codes_hash_idx (code_hash),
 SHARD KEY (id)
) COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS oauth_grants (
 id VARCHAR(255) NOT NULL,
 user_id VARCHAR(255) NOT NULL,
 client_id LONGTEXT NOT NULL,
 client_name LONGTEXT NOT NULL,
 resource LONGTEXT NOT NULL,
 current_refresh_token_id VARCHAR(255) NOT NULL,
 created_at DATETIME(6) NOT NULL,
 last_used_at DATETIME(6),
 revoked_at DATETIME(6),
 PRIMARY KEY (id),
 KEY oauth_grants_user_idx (user_id, created_at),
 SHARD KEY (id)
) COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS oauth_tokens (
 grant_id VARCHAR(255) NOT NULL,
 id VARCHAR(255) NOT NULL,
 kind VARCHAR(8) NOT NULL,
 token_hash VARBINARY(32) NOT NULL,
 created_at DATETIME(6) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 rotated_at DATETIME(6),
 PRIMARY KEY (grant_id, id),
 KEY oauth_tokens_hash_idx (token_hash),
 SHARD KEY (grant_id)
) COLLATE utf8mb4_bin;
