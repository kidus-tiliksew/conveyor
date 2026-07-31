-- Migration 048: immutable planning model and repository-snapshot provenance
-- plus durable exploration-output accounting (spec §§21.50-21.52).

ALTER TABLE planning_sessions
    ADD COLUMN model text NOT NULL DEFAULT '',
    ADD COLUMN effort text NOT NULL DEFAULT '',
    ADD COLUMN exploration_output_tokens integer NOT NULL DEFAULT 0
        CHECK (exploration_output_tokens >= 0),
    ADD COLUMN exploration_tokens_used bigint NOT NULL DEFAULT 0
        CHECK (exploration_tokens_used >= 0),
    ADD COLUMN primary_repo text NOT NULL DEFAULT '',
    ADD COLUMN pinned_revisions jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(pinned_revisions) = 'object');

-- Repository pins are workspace-scoped planning audit events just like the
-- other planning-session lifecycle mutations introduced in migration 046.
ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
    task_id IS NOT NULL
    OR (kind IN (
        'config.updated', 'workspace.created', 'worker.pairing_issued',
        'worker.enrolled', 'worker.revoked', 'worker.heartbeat',
        'requirement.created', 'requirement.version_proposed',
        'requirement.version_confirmed', 'planning_session.created',
        'planning_session.message_appended', 'planning_session.finalized',
        'planning_session.abandoned', 'planning_session.repo_pinned'
    ) AND job_id IS NULL)
);
