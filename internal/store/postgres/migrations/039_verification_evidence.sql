ALTER TABLE artifact_links
    DROP CONSTRAINT artifact_links_role_check;

ALTER TABLE artifact_links
    ADD CONSTRAINT artifact_links_role_check
    CHECK (role IN ('task_context', 'generated_audit', 'generated_output', 'verification_evidence'));
