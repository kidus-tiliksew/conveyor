ALTER TABLE users
    ADD COLUMN password_hash text
        CHECK (password_hash IS NULL OR password_hash LIKE '$argon2id$%');

ALTER TABLE dashboard_sessions
    ADD COLUMN established_by_link boolean NOT NULL DEFAULT false;

