CREATE TABLE deployment_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id       text NOT NULL DEFAULT 'deployment' REFERENCES orgs(id),
    kind         text NOT NULL CHECK (kind IN (
        'identity.legacy_token_rotated',
        'identity.legacy_bindings_healed'
    )),
    actor_id     text NOT NULL,
    actor_role   text NOT NULL,
    payload_json jsonb NOT NULL,
    at           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX deployment_events_timeline_idx
    ON deployment_events (org_id, at, id);

CREATE TRIGGER deployment_events_append_only
BEFORE UPDATE OR DELETE ON deployment_events
FOR EACH ROW EXECUTE FUNCTION conveyor_reject_append_only_mutation();

-- Deployment authorization is an indexed live-binding decision. The existing
-- user index starts with user_id; include role so the EXISTS probe can remain
-- index-only as deployments accumulate workspaces.
CREATE INDEX workspace_role_bindings_user_role_idx
    ON workspace_role_bindings (user_id, role);
