-- component-crates; feature-crates CR-1; req-crates REQ-1, REQ-2. A crate is a
-- workspace record that groups tasks for tracking and reporting. It holds a
-- name, an optional target date, a declared requirement list, and a state of
-- open or closed. A task belongs to at most one crate through the nullable
-- tasks.crate_id column. Existing tasks keep a NULL crate. No dispatch,
-- claim, gate, review, merge, next-action, or lineage query reads either
-- table or the column (component-crates invariant 1; DEC-141).
CREATE TABLE IF NOT EXISTS crates (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    id text NOT NULL,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    target_date date NULL,
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    closed_at timestamptz NULL,
    PRIMARY KEY (workspace_id, id)
);

-- The declared list. An edit replaces it whole, and it changes no task's
-- context attachments (component-crates invariant 5; req-crates AC-3.3).
CREATE TABLE IF NOT EXISTS crate_requirements (
    workspace_id text NOT NULL,
    crate_id text NOT NULL,
    requirement_id text NOT NULL,
    PRIMARY KEY (workspace_id, crate_id, requirement_id),
    FOREIGN KEY (workspace_id, crate_id) REFERENCES crates(workspace_id, id) ON DELETE CASCADE
);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS crate_id text NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_crate_fk') THEN
        ALTER TABLE tasks ADD CONSTRAINT tasks_crate_fk
            FOREIGN KEY (workspace_id, crate_id) REFERENCES crates(workspace_id, id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS tasks_workspace_crate_idx ON tasks (workspace_id, crate_id)
    WHERE crate_id IS NOT NULL;
