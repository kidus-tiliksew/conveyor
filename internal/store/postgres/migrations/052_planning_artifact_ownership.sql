ALTER TABLE artifact_links
    ADD COLUMN planning_session_id text;

ALTER TABLE artifact_links
    ADD CONSTRAINT artifact_links_planning_session_fk
        FOREIGN KEY (workspace_id, planning_session_id)
        REFERENCES planning_sessions(workspace_id, id) ON DELETE CASCADE;

CREATE UNIQUE INDEX artifact_links_planning_session_unique
    ON artifact_links (workspace_id, artifact_id, planning_session_id, role)
    WHERE planning_session_id IS NOT NULL;
CREATE INDEX artifact_links_planning_session_idx
    ON artifact_links (planning_session_id) WHERE planning_session_id IS NOT NULL;

ALTER TABLE artifact_links DROP CONSTRAINT artifact_links_check;
ALTER TABLE artifact_links ADD CONSTRAINT artifact_links_check CHECK (
    (task_id IS NOT NULL)::int
    + (feature_id IS NOT NULL)::int
    + (requirement_id IS NOT NULL)::int
    + (planning_session_id IS NOT NULL)::int <= 1
);

DROP INDEX artifact_links_workspace_unique;
CREATE UNIQUE INDEX artifact_links_workspace_unique
    ON artifact_links (workspace_id, artifact_id, role)
    WHERE task_id IS NULL AND feature_id IS NULL AND requirement_id IS NULL
      AND planning_session_id IS NULL;
