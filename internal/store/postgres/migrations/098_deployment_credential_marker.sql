-- The CONVEYOR_API_TOKEN mapping is deployment state, not a property of its
-- display label. Keep the table locked while the legacy label is consulted for
-- the one-time backfill and the replacement uniqueness guard is installed.
LOCK TABLE user_tokens IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE user_tokens
    ADD COLUMN IF NOT EXISTS deployment_credential boolean NOT NULL DEFAULT false;

DO $$
DECLARE
    marked_count integer;
    legacy_count integer;
BEGIN
    SELECT count(*) INTO marked_count
    FROM user_tokens
    WHERE deployment_credential;

    IF marked_count > 1 THEN
        RAISE EXCEPTION 'multiple deployment credentials already marked';
    END IF;

    -- A second execution must preserve the durable marker even when labels
    -- have subsequently changed or collided.
    IF marked_count = 0 THEN
        SELECT count(*) INTO legacy_count
        FROM user_tokens
        WHERE label = 'legacy API token' AND kind = 'user';

        IF legacy_count > 1 THEN
            RAISE EXCEPTION 'multiple legacy API token mappings found during deployment credential backfill';
        END IF;

        UPDATE user_tokens
        SET deployment_credential = true
        WHERE label = 'legacy API token' AND kind = 'user';
    END IF;
END
$$;

DROP INDEX IF EXISTS user_tokens_legacy_mapping_unique;

CREATE UNIQUE INDEX IF NOT EXISTS user_tokens_deployment_credential_unique
    ON user_tokens (deployment_credential)
    WHERE deployment_credential;
