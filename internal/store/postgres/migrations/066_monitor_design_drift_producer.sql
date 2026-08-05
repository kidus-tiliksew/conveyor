ALTER TABLE monitor_observations
    ADD COLUMN changed_paths text[] NOT NULL DEFAULT '{}',
    ADD COLUMN causal_event_id bigint;

CREATE INDEX monitor_observations_causal_event_idx
    ON monitor_observations (workspace_id, causal_event_id)
    WHERE causal_event_id IS NOT NULL;

ALTER TABLE repository_drift DROP CONSTRAINT repository_drift_check;
ALTER TABLE repository_drift ADD CONSTRAINT repository_drift_check CHECK (
    (resolved_at IS NULL AND outcome = '') OR
    (resolved_at IS NOT NULL AND outcome IN (
        'requirements_amended','design_document_updated','conflict_resolved','change_reverted'
    ))
);
