-- Phase 5.3 keeps the approved issue projection durable and idempotent. The
-- task ID is both the local association key and the remote marker key (spec
-- §21.12 change 5, amended by §21.15).
CREATE TABLE github_lifecycles (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository text NOT NULL,
    spec_version integer NOT NULL,
    source text NOT NULL DEFAULT '',
    source_issue_number integer NOT NULL DEFAULT 0 CHECK (source_issue_number >= 0),
    issue_number integer NOT NULL DEFAULT 0 CHECK (issue_number >= 0),
    issue_url text NOT NULL DEFAULT '',
    outcome text NOT NULL DEFAULT '' CHECK (outcome IN ('', 'created', 'reused')),
    state text NOT NULL CHECK (state IN ('queued', 'retrying', 'published', 'failed')),
    attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, task_id)
);

CREATE UNIQUE INDEX github_lifecycles_issue_idx
    ON github_lifecycles (workspace_id, repository, issue_number)
    WHERE issue_number > 0;

CREATE INDEX github_lifecycles_state_idx
    ON github_lifecycles (workspace_id, state, updated_at);
