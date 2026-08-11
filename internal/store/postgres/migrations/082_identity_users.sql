ALTER TABLE users
    ADD COLUMN email text,
    ADD COLUMN display_name text NOT NULL DEFAULT '',
    ADD COLUMN status text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'deactivated'));

-- The retired Phase 2 identity rows had no dependants after migration 009.
-- Preserve any such row as an account instead of discarding it on upgrade.
UPDATE users
SET email = md5(id) || '@legacy.invalid',
    display_name = CASE
        WHEN btrim(identity_provider_ref) = '' THEN id
        ELSE identity_provider_ref
    END;

ALTER TABLE users
    ALTER COLUMN email SET NOT NULL,
    ADD CONSTRAINT users_email_normalized CHECK (email = lower(btrim(email)) AND email <> '');

CREATE UNIQUE INDEX users_email_unique ON users (email);

ALTER TABLE users
    DROP COLUMN identity_provider_ref,
    DROP COLUMN role;
