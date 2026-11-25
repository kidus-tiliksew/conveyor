-- component-production-feedback, feature-production-feedback-live FB-3,
-- req-production-feedback REQ-1 (AC-1.4) and REQ-3, req-environments
-- AC-1.2, and DEC-149(1) and (4). These rows are records for people to read.
-- No dispatch, claim, gate, review, merge, next-action, lineage, or
-- work-order query reads these tables (component-production-feedback
-- invariant 1). Every row names its workspace. No backfill, and no write to
-- the task ledger.

-- The stored answer for each version, keyed by the FB-2 version identity:
-- the environment, the service digest, and the version digest. A resolved
-- row holds the workspace repository and the full commit ID and is never
-- changed. An unresolved row keeps its reason and the earliest time a later
-- signal can try it again (invariants 7 and 8).
CREATE TABLE IF NOT EXISTS feedback_resolutions (
    workspace_id text NOT NULL,
    environment_id text NOT NULL,
    service_digest text NOT NULL,
    version_digest text NOT NULL,
    service text NOT NULL,
    reported_repository text NOT NULL,
    reported_commit text NOT NULL,
    reported_version text NOT NULL,
    resolved_repository text NOT NULL DEFAULT '',
    resolved_commit text NOT NULL DEFAULT '' CHECK (resolved_commit = '' OR resolved_commit ~ '^[0-9a-f]{40}$'),
    reason text NOT NULL DEFAULT '',
    attempted_at timestamptz NOT NULL,
    next_attempt_at timestamptz NULL,
    PRIMARY KEY (workspace_id, environment_id, service_digest, version_digest),
    FOREIGN KEY (workspace_id, environment_id) REFERENCES environments(workspace_id, id) ON DELETE CASCADE,
    CHECK ((resolved_commit <> '' AND reason = '' AND next_attempt_at IS NULL) OR (resolved_commit = '' AND reason <> '' AND next_attempt_at IS NOT NULL))
);

-- GitHub's answer for each pair of commits compared, keyed by workspace,
-- canonical forge repository, full base commit, and full head commit. A
-- stored answer is never asked again (invariant 14).
CREATE TABLE IF NOT EXISTS feedback_comparisons (
    workspace_id text NOT NULL,
    repository text NOT NULL,
    base_commit text NOT NULL CHECK (base_commit ~ '^[0-9a-f]{40}$'),
    head_commit text NOT NULL CHECK (head_commit ~ '^[0-9a-f]{40}$'),
    status text NOT NULL CHECK (status IN ('ahead', 'identical', 'behind', 'diverged', 'missing')),
    compared_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, repository, base_commit, head_commit)
);

-- Admitted comparisons and branch reads. The rows in the last minute count
-- against the workspace's 60-a-minute allowance, and a row in its lease keeps
-- a second request for the same pair out.
CREATE TABLE IF NOT EXISTS feedback_admissions (
    workspace_id text NOT NULL,
    repository text NOT NULL,
    base_commit text NOT NULL,
    head_ref text NOT NULL,
    admitted_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, repository, base_commit, head_ref)
);

CREATE INDEX IF NOT EXISTS feedback_admissions_time_idx
    ON feedback_admissions (workspace_id, admitted_at);

-- The newest observed head of each tracked branch, read in the background.
CREATE TABLE IF NOT EXISTS feedback_branch_heads (
    workspace_id text NOT NULL,
    repository text NOT NULL,
    branch text NOT NULL,
    head_commit text NOT NULL CHECK (head_commit ~ '^[0-9a-f]{40}$'),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, repository, branch)
);
