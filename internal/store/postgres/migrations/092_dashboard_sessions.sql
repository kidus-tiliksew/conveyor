CREATE TABLE dashboard_sessions (
    id           text PRIMARY KEY,
    user_id      text NOT NULL REFERENCES users(id),
    session_hash bytea NOT NULL UNIQUE CHECK (octet_length(session_hash) = 32),
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX dashboard_sessions_user_idx
    ON dashboard_sessions (user_id, created_at DESC);
