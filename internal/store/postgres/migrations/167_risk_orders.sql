-- feature-risk-based-review RR-11 and RR-15; req-risk-based-review AC-5.6 and
-- AC-12.1 to AC-12.3; component-risk-review invariant 12. Existing rows keep
-- their stages, sources, and values.
--
-- A risk order is a work order at the additive stage 'risk'.
ALTER TABLE work_orders DROP CONSTRAINT work_orders_stage_check;
ALTER TABLE work_orders ADD CONSTRAINT work_orders_stage_check
    CHECK (stage IN ('implement', 'review', 'risk', 'spec', 'verify'));

-- A completed risk order records a rating with source 'risk_agent'. Ratings
-- recorded with source 'plan' or 'reviewer' stay valid.
ALTER TABLE task_risk_ratings DROP CONSTRAINT IF EXISTS task_risk_ratings_source_check;
ALTER TABLE task_risk_ratings ADD CONSTRAINT task_risk_ratings_source_check
    CHECK (source IN ('plan', 'path_rule', 'reviewer', 'person', 'risk_agent'));

-- One row per risk order names what it rates: exactly one plan version, or one
-- head with its base and the changed paths and matched rules copied at
-- creation. A task has at most one risk order per subject.
CREATE TABLE IF NOT EXISTS risk_orders (
    workspace_id text NOT NULL,
    work_order_id text NOT NULL,
    task_id text NOT NULL,
    subject_key text NOT NULL,
    plan_version integer NOT NULL DEFAULT 0 CHECK (plan_version >= 0),
    head_sha text NOT NULL DEFAULT '',
    base_sha text NOT NULL DEFAULT '',
    changed_paths jsonb NOT NULL DEFAULT '[]'::jsonb,
    matched_rules jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, work_order_id),
    UNIQUE (workspace_id, task_id, subject_key),
    CHECK ((plan_version > 0 AND head_sha = '' AND base_sha = '')
        OR (plan_version = 0 AND head_sha <> '' AND base_sha <> '')),
    FOREIGN KEY (workspace_id, work_order_id) REFERENCES work_orders(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);

-- The claim ledger keeps each accepted claim's session ID and client-token
-- hash, because a release, expiry, or cancellation clears them from the work
-- order row. A risk claim is refused for any identity that claimed a plan,
-- implementation, or review order of the task. No event or read exposes the
-- hash.
CREATE TABLE IF NOT EXISTS work_order_claim_identities (
    workspace_id text NOT NULL,
    task_id text NOT NULL,
    work_order_id text NOT NULL,
    attempt_id text NOT NULL,
    stage text NOT NULL,
    session_id text NOT NULL DEFAULT '',
    client_token_hash text NOT NULL DEFAULT '',
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, work_order_id, attempt_id),
    FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS work_order_claim_identities_task_idx
    ON work_order_claim_identities (workspace_id, task_id);

-- Claims before this migration keep the identity their order row still holds.
INSERT INTO work_order_claim_identities
    (workspace_id, task_id, work_order_id, attempt_id, stage, session_id, client_token_hash, claimed_at)
SELECT workspace_id, task_id, id, COALESCE(NULLIF(attempt_id, ''), 'migration-167'), stage, session_id, client_token_hash,
    COALESCE(execution_started_at, updated_at, created_at)
FROM work_orders
WHERE session_id <> '' OR client_token_hash <> ''
ON CONFLICT DO NOTHING;
