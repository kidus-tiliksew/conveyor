-- component-persistence, Read migration invariants. migrate.go runs these
-- statements only when the original migration ledger is empty. Existing
-- installations use the matching hand-run operator preparation script.
CREATE INDEX IF NOT EXISTS tasks_workspace_state_created_idx
    ON tasks (workspace_id, state, created_at, id);
CREATE INDEX IF NOT EXISTS tasks_workspace_created_idx
    ON tasks (workspace_id, created_at, id);
CREATE INDEX IF NOT EXISTS artifacts_workspace_created_idx
    ON artifacts (workspace_id, created_at, id);
CREATE INDEX IF NOT EXISTS artifact_links_artifact_lookup_idx
    ON artifact_links (workspace_id, artifact_id);
CREATE INDEX IF NOT EXISTS artifact_links_task_lookup_idx
    ON artifact_links (workspace_id, task_id) WHERE task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS artifact_links_requirement_lookup_idx
    ON artifact_links (workspace_id, requirement_id) WHERE requirement_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS artifact_links_session_lookup_idx
    ON artifact_links (workspace_id, planning_session_id) WHERE planning_session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS events_planning_session_lookup_idx
    ON events (workspace_id, (payload_json->>'session_id'), at, id);
CREATE INDEX IF NOT EXISTS events_reference_document_lookup_idx
    ON events (workspace_id, (payload_json->>'document_id'), at, id)
    WHERE kind LIKE 'reference_document.%';
