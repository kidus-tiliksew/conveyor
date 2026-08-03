-- Typed lineage artifact discovery probes each ownership column separately.
-- The owner-specific unique indexes cover task, requirement, and planning
-- session branches; evidence lookup needs the equivalent partial path by
-- workspace and content-addressed artifact id.
CREATE INDEX artifact_links_lineage_evidence_idx
    ON artifact_links (workspace_id, artifact_id)
    WHERE role = 'verification_evidence';
