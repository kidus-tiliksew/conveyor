ALTER TABLE credentials
    ADD COLUMN vendor text NOT NULL DEFAULT '',
    ADD COLUMN leased_by text NOT NULL DEFAULT '',
    ADD COLUMN lease_until timestamptz;

CREATE INDEX credentials_lease_idx
    ON credentials (harness, lease_until, cooldown_until);

ALTER TABLE jobs
    ADD COLUMN credential_id text NOT NULL DEFAULT '',
    ADD COLUMN auth_mode text NOT NULL DEFAULT '';
