ALTER TABLE tasks ADD COLUMN mode text;
ALTER TABLE tasks ADD COLUMN spec_approval boolean;
ALTER TABLE tasks ADD COLUMN merge_approval boolean;
ALTER TABLE tasks ADD COLUMN policy_version integer NOT NULL DEFAULT 0;

UPDATE tasks SET
    mode = CASE WHEN escalation_level = 'L3' THEN 'manual' ELSE 'auto' END,
    spec_approval = CASE WHEN escalation_level IN ('L0', 'L1') THEN false ELSE true END,
    merge_approval = CASE WHEN escalation_level = 'L0' THEN false ELSE true END;

ALTER TABLE tasks ALTER COLUMN mode SET NOT NULL;
ALTER TABLE tasks ALTER COLUMN spec_approval SET NOT NULL;
ALTER TABLE tasks ALTER COLUMN merge_approval SET NOT NULL;
ALTER TABLE tasks ADD CONSTRAINT tasks_mode_check CHECK (mode IN ('auto', 'manual'));

ALTER TABLE work_orders ADD COLUMN worker_id text NOT NULL DEFAULT '';

CREATE TABLE worker_pairings (
    token_hash text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workers (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name text NOT NULL,
    credential_hash text NOT NULL UNIQUE,
    lease_expires_at timestamptz,
    last_seen_at timestamptz,
    revoked_at timestamptz,
    probe_results jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE INDEX workers_workspace_idx ON workers (workspace_id, created_at);

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
    task_id IS NOT NULL
    OR (kind IN (
        'config.updated', 'workspace.created', 'worker.pairing_issued',
        'worker.enrolled', 'worker.revoked', 'worker.heartbeat'
    ) AND job_id IS NULL)
);
