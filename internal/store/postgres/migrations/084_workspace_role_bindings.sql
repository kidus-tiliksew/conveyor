CREATE TABLE workspace_role_bindings (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         text NOT NULL CHECK (role IN ('user', 'operator')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);

CREATE INDEX workspace_role_bindings_user_idx
    ON workspace_role_bindings (user_id, workspace_id);

-- The first provisioned account owns the existing singleton deployment. This
-- is the zero-configuration legacy-token compatibility path.
INSERT INTO workspace_role_bindings (workspace_id, user_id, role)
SELECT w.id, u.id, 'operator'
FROM workspaces w
CROSS JOIN LATERAL (SELECT id FROM users ORDER BY created_at, id LIMIT 1) u
ON CONFLICT (workspace_id, user_id) DO NOTHING;
