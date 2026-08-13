ALTER TABLE deployment_events
    DROP CONSTRAINT deployment_events_kind_check;

ALTER TABLE deployment_events
    ADD CONSTRAINT deployment_events_kind_check CHECK (kind IN (
        'identity.legacy_token_rotated',
        'identity.legacy_bindings_healed',
        'identity.personal_token_issued',
        'identity.personal_token_revoked'
    ));
