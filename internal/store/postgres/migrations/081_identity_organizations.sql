CREATE TABLE orgs (
    id         text PRIMARY KEY,
    name       text NOT NULL CHECK (btrim(name) <> ''),
    singleton  boolean NOT NULL DEFAULT true CHECK (singleton),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (singleton)
);

-- A deployment is one database. The fixed identity makes the forward migration
-- idempotent and gives existing workspaces a stable backfill target.
INSERT INTO orgs (id, name) VALUES ('deployment', 'Conveyor');

CREATE FUNCTION conveyor_preserve_deployment_org()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'the deployment organization cannot be removed or re-keyed';
END;
$$;

CREATE TRIGGER orgs_preserve_singleton
BEFORE DELETE OR UPDATE OF id, singleton ON orgs
FOR EACH ROW EXECUTE FUNCTION conveyor_preserve_deployment_org();
