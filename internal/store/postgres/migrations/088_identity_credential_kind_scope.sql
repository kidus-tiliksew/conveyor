ALTER TABLE user_tokens
    ADD COLUMN kind text,
    ADD COLUMN scope text;

UPDATE user_tokens t
SET kind = CASE WHEN t.id LIKE 'agt\_%' ESCAPE '\' THEN lower('AGENT') ELSE lower('USER') END,
    scope = CASE
        WHEN t.id LIKE 'agt\_%' ESCAPE '\' THEN lower('USER')
        WHEN t.label = 'legacy API token' OR EXISTS (
            SELECT 1
            FROM workspace_role_bindings b
            WHERE b.user_id = t.user_id AND b.role = 'operator'
        ) THEN lower('OPERATOR')
        ELSE lower('USER')
    END;

ALTER TABLE user_tokens
    ALTER COLUMN kind SET DEFAULT 'user',
    ALTER COLUMN kind SET NOT NULL,
    ALTER COLUMN scope SET DEFAULT 'user',
    ALTER COLUMN scope SET NOT NULL,
    ADD CONSTRAINT user_tokens_kind_check CHECK (kind IN ('user', 'agent')),
    ADD CONSTRAINT user_tokens_scope_check CHECK (scope IN ('user', 'operator')),
    ADD CONSTRAINT user_tokens_agent_scope_check CHECK (kind <> 'agent' OR scope = 'user');

CREATE UNIQUE INDEX user_tokens_legacy_mapping_unique
    ON user_tokens (label)
    WHERE label = 'legacy API token' AND kind = 'user';

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','workspace.membership_granted','workspace.membership_revoked','identity.legacy_token_rotated',
    'worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed','requirement.staleness_acknowledged',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'planning_bundle.finalized','planning_bundle.revised','planning_bundle.approved','planning_bundle.rejected',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'decision.proposed','decision.confirmed','decision.dismissed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);
