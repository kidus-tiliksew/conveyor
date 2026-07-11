CREATE TABLE workspace_credentials (
    workspace_id  text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    credential_id text NOT NULL REFERENCES credentials(id) ON DELETE CASCADE,
    PRIMARY KEY (workspace_id, credential_id)
);

CREATE TABLE workspace_vendor_policies (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    vendor       text NOT NULL,
    harness      text NOT NULL,
    auth_mode    text NOT NULL,
    PRIMARY KEY (workspace_id, vendor, harness, auth_mode),
    FOREIGN KEY (vendor, harness, auth_mode)
        REFERENCES vendor_policies(vendor, harness, auth_mode) ON DELETE CASCADE
);

-- Existing Phase 2 databases did not record which workspace config supplied
-- platform capacity. Associate existing rows conservatively with every known
-- workspace; each workspace's next bootstrap replaces its own associations.
INSERT INTO workspace_credentials (workspace_id, credential_id)
SELECT w.id, c.id FROM workspaces w CROSS JOIN credentials c
ON CONFLICT DO NOTHING;

INSERT INTO workspace_vendor_policies (workspace_id, vendor, harness, auth_mode)
SELECT w.id, p.vendor, p.harness, p.auth_mode
FROM workspaces w CROSS JOIN vendor_policies p
ON CONFLICT DO NOTHING;
