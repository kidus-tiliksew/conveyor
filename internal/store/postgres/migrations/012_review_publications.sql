CREATE TABLE review_publications (
    review_work_order_id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    job_id text NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    verdict text NOT NULL CHECK (verdict IN ('approve', 'changes_requested')),
    reason_code text NOT NULL,
    summary text NOT NULL,
    feedback text NOT NULL DEFAULT '',
    reviewed_commit_sha text NOT NULL DEFAULT '',
    reviewer_model text NOT NULL DEFAULT '',
    reviewer_session text NOT NULL DEFAULT 'distinct',
    same_model_as_implementer text NOT NULL DEFAULT 'unknown',
    state text NOT NULL CHECK (state IN ('queued', 'retrying', 'published', 'failed')),
    attempts integer NOT NULL DEFAULT 0,
    check_run_id bigint NOT NULL DEFAULT 0,
    comment_id bigint NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX review_publications_task_idx ON review_publications (task_id, created_at);
