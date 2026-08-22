-- Display-name changes are caller-owned, append-only identity audit events.
ALTER TABLE deployment_events DROP CONSTRAINT deployment_events_kind_check;
ALTER TABLE deployment_events ADD CONSTRAINT deployment_events_kind_check CHECK (kind IN (
    'identity.legacy_token_rotated', 'identity.legacy_bindings_healed',
    'identity.personal_token_issued', 'identity.personal_token_revoked',
    'identity.signin_link_issued', 'identity.signin_link_redeemed',
    'identity.invitation_delivery_sent', 'identity.invitation_delivery_failed',
    'identity.invitation_delivery_fallback',
    'identity.dashboard_session_created', 'identity.dashboard_session_revoked',
    'identity.password_set', 'identity.password_changed',
    'identity.forge_token_stored', 'identity.forge_token_replaced',
    'identity.forge_token_deleted', 'identity.display_name_changed'
));
