ALTER TABLE artifact_links
    ADD COLUMN role text NOT NULL DEFAULT 'task_context';

ALTER TABLE artifact_links
    ADD CONSTRAINT artifact_links_role_check
    CHECK (role IN ('task_context', 'generated_audit', 'generated_output'));

-- Existing transcript blobs predate explicit provenance. Their durable
-- transcript -> job -> task relationship is authoritative compatibility
-- evidence that the task link is generated audit data, not user context.
UPDATE artifact_links al
SET role = 'generated_audit'
FROM transcripts tr
JOIN jobs j ON j.id = tr.job_id
JOIN tasks t ON t.id = j.task_id
WHERE tr.uri = 'artifact://' || al.artifact_id
  AND al.workspace_id = t.workspace_id
  AND al.task_id = j.task_id;

DROP INDEX artifact_links_workspace_unique;
DROP INDEX artifact_links_task_unique;
DROP INDEX artifact_links_feature_unique;

CREATE UNIQUE INDEX artifact_links_workspace_unique
    ON artifact_links (workspace_id, artifact_id, role)
    WHERE task_id IS NULL AND feature_id IS NULL;
CREATE UNIQUE INDEX artifact_links_task_unique
    ON artifact_links (workspace_id, artifact_id, task_id, role)
    WHERE task_id IS NOT NULL;
CREATE UNIQUE INDEX artifact_links_feature_unique
    ON artifact_links (workspace_id, artifact_id, feature_id, role)
    WHERE feature_id IS NOT NULL;
