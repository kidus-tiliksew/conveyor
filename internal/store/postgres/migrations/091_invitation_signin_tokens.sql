CREATE TABLE invitation_signin_tokens (
    id          text PRIMARY KEY,
    email       text NOT NULL,
    user_id     text REFERENCES users(id),
    token_hash  bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    expires_at  timestamptz NOT NULL,
    redeemed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX invitation_signin_tokens_email_idx
    ON invitation_signin_tokens (email, created_at DESC);

ALTER TABLE deployment_events DROP CONSTRAINT deployment_events_kind_check;
ALTER TABLE deployment_events ADD CONSTRAINT deployment_events_kind_check CHECK (kind IN (
    'identity.legacy_token_rotated', 'identity.legacy_bindings_healed',
    'identity.personal_token_issued', 'identity.personal_token_revoked',
    'identity.signin_link_issued', 'identity.signin_link_redeemed',
    'identity.invitation_delivery_sent', 'identity.invitation_delivery_failed',
    'identity.invitation_delivery_fallback',
    'identity.dashboard_session_created', 'identity.dashboard_session_revoked'
));
