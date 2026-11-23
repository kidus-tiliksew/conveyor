-- req-mcp-oauth REQ-2 to REQ-4, req-security-boundaries AC-2.1, DEC-146,
-- component-identity-membership invariants 51 to 54, and
-- component-persistence invariants 21, 22, and 41. The four deployment OAuth
-- tables sit outside every workspace. A credential is stored only as the
-- 32-byte SHA-256 of its wire value. A grant names its one current refresh
-- token, so rotation, replay detection, and revocation decide on the grant
-- row. A rotated refresh token stays as a replay marker. No backfill.
CREATE TABLE IF NOT EXISTS oauth_clients (
 id text PRIMARY KEY,
 name text NOT NULL,
 redirect_uris jsonb NOT NULL CHECK (jsonb_typeof(redirect_uris) = 'array'),
 grant_types jsonb NOT NULL CHECK (jsonb_typeof(grant_types) = 'array'),
 created_at timestamptz NOT NULL,
 unused_expires_at timestamptz NOT NULL,
 first_used_at timestamptz
);
CREATE INDEX IF NOT EXISTS oauth_clients_unused_idx ON oauth_clients (unused_expires_at) WHERE first_used_at IS NULL;

CREATE TABLE IF NOT EXISTS oauth_authorization_codes (
 id text PRIMARY KEY,
 code_hash bytea NOT NULL UNIQUE CHECK (octet_length(code_hash) = 32),
 user_id text NOT NULL REFERENCES users(id),
 client_id text NOT NULL,
 client_name text NOT NULL,
 redirect_uri text NOT NULL,
 resource text NOT NULL,
 code_challenge text NOT NULL,
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz
);

CREATE TABLE IF NOT EXISTS oauth_grants (
 id text PRIMARY KEY,
 user_id text NOT NULL REFERENCES users(id),
 client_id text NOT NULL,
 client_name text NOT NULL,
 resource text NOT NULL,
 current_refresh_token_id text NOT NULL,
 created_at timestamptz NOT NULL,
 last_used_at timestamptz,
 revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS oauth_grants_user_idx ON oauth_grants (user_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS oauth_grants_client_idx ON oauth_grants (client_id);

CREATE TABLE IF NOT EXISTS oauth_tokens (
 id text PRIMARY KEY,
 grant_id text NOT NULL REFERENCES oauth_grants(id),
 kind text NOT NULL CHECK (kind IN ('oat', 'ort')),
 token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 rotated_at timestamptz
);
CREATE INDEX IF NOT EXISTS oauth_tokens_grant_idx ON oauth_tokens (grant_id);

-- The OAuth audit kinds join every kind migration 125 admits. NOT VALID keeps
-- the retired kinds of historical rows, as migration 125 did.
ALTER TABLE deployment_events DROP CONSTRAINT deployment_events_kind_check;
ALTER TABLE deployment_events ADD CONSTRAINT deployment_events_kind_check CHECK (kind IN (
    'identity.legacy_token_rotated', 'identity.legacy_bindings_healed',
    'identity.personal_token_issued', 'identity.personal_token_revoked',
    'identity.signin_link_issued', 'identity.signin_link_redeemed',
    'identity.invitation_delivery_sent', 'identity.invitation_delivery_failed',
    'identity.invitation_delivery_fallback',
    'identity.dashboard_session_created', 'identity.dashboard_session_revoked',
    'identity.password_set', 'identity.password_changed', 'identity.display_name_changed',
    'identity.oauth_grant_created', 'identity.oauth_grant_revoked',
    'identity.oauth_refresh_reuse_detected'
)) NOT VALID;
