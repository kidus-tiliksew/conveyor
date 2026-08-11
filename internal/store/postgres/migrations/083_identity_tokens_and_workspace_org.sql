CREATE TABLE user_tokens (
    id           text PRIMARY KEY,
    user_id      text NOT NULL REFERENCES users(id),
    label        text NOT NULL DEFAULT '',
    token_hash   bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    last_used_at timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX user_tokens_user_idx ON user_tokens (user_id, created_at);

ALTER TABLE workspaces
    ADD COLUMN org_id text REFERENCES orgs(id);

UPDATE workspaces SET org_id = 'deployment' WHERE org_id IS NULL;

ALTER TABLE workspaces
    ALTER COLUMN org_id SET DEFAULT 'deployment',
    ALTER COLUMN org_id SET NOT NULL;
