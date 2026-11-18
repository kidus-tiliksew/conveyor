-- Operator-run only, outside application startup. Connect to the application
-- database. Keep a restorable backup under operator authority. migrate.go
-- runs migration 171 only when the original migration ledger is empty.
-- A 171 ledger row on an existing installation is not readiness.
--
-- Do not DROP, TRUNCATE, rename, or create tables or globals.
-- Do not source this file as one shot. Run preflight, then the CREATE INDEX
-- statements, then postflight.
-- Each CREATE INDEX CONCURRENTLY statement builds its index while writes to
-- the table continue. Run each one as its own statement, outside a
-- transaction: no BEGIN, no psql --single-transaction, and no psql -1.
-- PostgreSQL refuses CREATE INDEX CONCURRENTLY inside a transaction block.
-- CREATE INDEX CONCURRENTLY IF NOT EXISTS is rerunnable when the name is
-- absent, or when the same name already exists. It does not compare
-- definitions. A present name with a different definition is an error. Stop.
-- Repeating IF NOT EXISTS does not repair an incompatible index.
-- A concurrent build that fails or is cancelled leaves its index in place
-- with valid = false. IF NOT EXISTS then skips that name, and the INVALID
-- index serves no read. Before you retry, run the preflight query. For each
-- row with valid = false whose definition matches the statement below, drop
-- that index, one statement at a time, outside a transaction:
--   DROP INDEX CONCURRENTLY IF EXISTS <index_name>;
-- Then run its CREATE INDEX CONCURRENTLY statement again. Drop only an index
-- this script names. Do not drop a valid index.
-- PostgreSQL does not use SingleStore generated column names. These indexes
-- match 171_hot_read_indexes.sql.
--
-- Preparation does not change a running Store snapshot. Close and reopen,
-- or restart, after postflight. Replace the quoted WORKSPACE_ID and entity
-- literals before EXPLAIN. Put no credentials in this file.

-- Preflight: existing names, validity, and definitions.
SELECT c.relname AS index_name, i.indisvalid AS valid,
       pg_get_indexdef(c.oid) AS definition
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_index i ON i.indexrelid = c.oid
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

-- Approved PostgreSQL 171 indexes. One statement at a time, outside a
-- transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS tasks_workspace_state_created_idx
    ON tasks (workspace_id, state, created_at, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS tasks_workspace_created_idx
    ON tasks (workspace_id, created_at, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS artifacts_workspace_created_idx
    ON artifacts (workspace_id, created_at, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS artifact_links_artifact_lookup_idx
    ON artifact_links (workspace_id, artifact_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS artifact_links_task_lookup_idx
    ON artifact_links (workspace_id, task_id) WHERE task_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS artifact_links_requirement_lookup_idx
    ON artifact_links (workspace_id, requirement_id) WHERE requirement_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS artifact_links_session_lookup_idx
    ON artifact_links (workspace_id, planning_session_id) WHERE planning_session_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_planning_session_lookup_idx
    ON events (workspace_id, (payload_json->>'session_id'), at, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_reference_document_lookup_idx
    ON events (workspace_id, (payload_json->>'document_id'), at, id)
    WHERE kind LIKE 'reference_document.%';

-- Postflight: each index must be valid and its definition must match the
-- CREATE INDEX statement above. Treat an index with valid = false as described
-- at the top of this file.
SELECT c.relname AS index_name, i.indisvalid AS valid,
       pg_get_indexdef(c.oid) AS definition
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_index i ON i.indexrelid = c.oid
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
