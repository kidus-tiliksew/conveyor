-- Operator-run only, outside application startup. Connect to the application
-- database. Keep a restorable backup under operator authority. migrate.go
-- runs migration 171 only when the original migration ledger is empty.
-- A 171 ledger row on an existing installation is not readiness.
--
-- Do not DROP, TRUNCATE, rename, or create tables or globals.
-- Do not source this file as one shot. Run preflight, then the CREATE INDEX
-- statements, then postflight.
-- CREATE INDEX IF NOT EXISTS is rerunnable when the name is absent, or when
-- the same name already exists. It does not compare definitions. A present
-- name with a different definition is an error. Stop. Repeating IF NOT EXISTS
-- does not repair an incompatible index.
-- PostgreSQL does not use SingleStore generated column names. These indexes
-- match 171_hot_read_indexes.sql.
--
-- Preparation does not change a running Store snapshot. Close and reopen,
-- or restart, after postflight. Replace the quoted WORKSPACE_ID and entity
-- literals before EXPLAIN. Put no credentials in this file.

-- Preflight: existing names and definitions.
SELECT c.relname AS index_name, pg_get_indexdef(c.oid) AS definition
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'i'
  AND n.nspname = current_schema()
  AND c.relname IN (
    'tasks_workspace_state_created_idx',
    'tasks_workspace_created_idx',
    'artifacts_workspace_created_idx',
    'artifact_links_artifact_lookup_idx',
    'artifact_links_task_lookup_idx',
    'artifact_links_requirement_lookup_idx',
    'artifact_links_session_lookup_idx',
    'events_planning_session_lookup_idx',
    'events_reference_document_lookup_idx'
  )
ORDER BY c.relname;

-- Approved PostgreSQL 171 indexes.
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

-- Postflight: each definition must match the CREATE INDEX statement above.
SELECT c.relname AS index_name, pg_get_indexdef(c.oid) AS definition
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'i'
  AND n.nspname = current_schema()
  AND c.relname IN (
    'tasks_workspace_state_created_idx',
    'tasks_workspace_created_idx',
    'artifacts_workspace_created_idx',
    'artifact_links_artifact_lookup_idx',
    'artifact_links_task_lookup_idx',
    'artifact_links_requirement_lookup_idx',
    'artifact_links_session_lookup_idx',
    'events_planning_session_lookup_idx',
    'events_reference_document_lookup_idx'
  )
ORDER BY c.relname;

-- Postflight EXPLAIN: original-value predicates the 171 indexes serve.
-- Replace the quoted literals.
EXPLAIN SELECT id FROM tasks
 WHERE workspace_id = 'WORKSPACE_ID'
   AND state = 'STATE_VALUE'
 ORDER BY created_at, id;

EXPLAIN SELECT id FROM tasks
 WHERE workspace_id = 'WORKSPACE_ID'
 ORDER BY created_at, id;

EXPLAIN SELECT id FROM artifacts
 WHERE workspace_id = 'WORKSPACE_ID'
 ORDER BY created_at, id;

EXPLAIN SELECT artifact_id FROM artifact_links
 WHERE workspace_id = 'WORKSPACE_ID'
   AND artifact_id = 'ARTIFACT_ID';

EXPLAIN SELECT artifact_id FROM artifact_links
 WHERE workspace_id = 'WORKSPACE_ID'
   AND task_id = 'TASK_ID';

EXPLAIN SELECT artifact_id FROM artifact_links
 WHERE workspace_id = 'WORKSPACE_ID'
   AND requirement_id = 'REQUIREMENT_ID';

EXPLAIN SELECT artifact_id FROM artifact_links
 WHERE workspace_id = 'WORKSPACE_ID'
   AND planning_session_id = 'SESSION_ID';

EXPLAIN SELECT id FROM events
 WHERE workspace_id = 'WORKSPACE_ID'
   AND payload_json->>'session_id' = 'SESSION_ID'
 ORDER BY at, id;

EXPLAIN SELECT id FROM events
 WHERE workspace_id = 'WORKSPACE_ID'
   AND payload_json->>'document_id' = 'DOCUMENT_ID'
   AND kind LIKE 'reference_document.%'
 ORDER BY at, id;
