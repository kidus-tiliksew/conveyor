-- Operator-run only, outside application startup. Select the application
-- database before any statement. Keep a restorable backup under operator
-- authority. These statements never run from Conveyor Open on an existing
-- installation. Migration 0044 recording a ledger row is not readiness.
--
-- Do not DROP, TRUNCATE, rename, or create tables or globals.
-- Do not source this file as one shot. Run preflight, apply only a missing
-- statement whose name is absent, then run postflight.
-- SingleStore ADD COLUMN and ADD KEY here do not take IF NOT EXISTS.
-- A present name with the approved definition is already prepared for that
-- object. A present name with a different definition is an error. Stop.
-- Repeating ADD does not repair an incompatible definition.
-- Each ALTER commits implicitly. Inspect a partial run. Do not roll back
-- with DROP.
--
-- COLUMNSTORE HASH is a valid actual index_type for USING HASH keys. HASH
-- is also valid. Rowstore keys report BTREE. SKIPLIST is also valid.
--
-- Preparation does not change a running Store snapshot. Close and reopen,
-- or restart, after postflight. Replace the quoted WORKSPACE_ID and entity
-- literals before EXPLAIN. Put no credentials in this file.

-- Preflight: existing names and definitions.
SHOW CREATE TABLE events_columnstore;
SHOW CREATE TABLE tasks;
SHOW CREATE TABLE artifacts;
SHOW CREATE TABLE artifact_links;

SELECT table_name, column_name
FROM information_schema.columns
WHERE table_schema=DATABASE()
  AND (
    (table_name='events_columnstore' AND column_name IN ('event_document_key','event_requirement_key','event_session_key'))
    OR (table_name='tasks' AND column_name IN ('task_state_key','task_intake_key'))
    OR (table_name='artifact_links' AND column_name IN ('artifact_lookup_key','task_lookup_key','requirement_lookup_key','session_lookup_key'))
  )
ORDER BY table_name, column_name;

SELECT table_name, index_name, column_name, seq_in_index, index_type, non_unique
FROM information_schema.statistics
WHERE table_schema=DATABASE()
  AND (
    (table_name='events_columnstore' AND index_name IN ('events_document_key_idx','events_requirement_key_idx','events_session_key_idx'))
    OR (table_name='tasks' AND index_name IN ('tasks_workspace_state_created_idx','tasks_workspace_intake_lookup_idx','tasks_workspace_created_idx'))
    OR (table_name='artifacts' AND index_name='artifacts_workspace_created_idx')
    OR (table_name='artifact_links' AND index_name IN ('artifact_links_artifact_lookup_idx','artifact_links_task_lookup_idx','artifact_links_requirement_lookup_idx','artifact_links_session_lookup_idx'))
  )
ORDER BY table_name, index_name, seq_in_index;

-- Approved generated columns. Each is PERSISTED VARCHAR(64).
ALTER TABLE events_columnstore
 ADD COLUMN event_document_key AS (SHA2(JSON_EXTRACT_STRING(payload_json,'document_id'),256)) PERSISTED VARCHAR(64);
ALTER TABLE events_columnstore
 ADD COLUMN event_requirement_key AS (SHA2(JSON_EXTRACT_STRING(payload_json,'requirement_id'),256)) PERSISTED VARCHAR(64);
ALTER TABLE events_columnstore
 ADD COLUMN event_session_key AS (SHA2(JSON_EXTRACT_STRING(payload_json,'session_id'),256)) PERSISTED VARCHAR(64);
ALTER TABLE tasks
 ADD COLUMN task_state_key AS (SHA2(state,256)) PERSISTED VARCHAR(64);
ALTER TABLE tasks
 ADD COLUMN task_intake_key AS (SHA2(COALESCE(intake_key,''),256)) PERSISTED VARCHAR(64);
ALTER TABLE artifact_links
 ADD COLUMN artifact_lookup_key AS (SHA2(artifact_id,256)) PERSISTED VARCHAR(64);
ALTER TABLE artifact_links
 ADD COLUMN task_lookup_key AS (SHA2(COALESCE(task_id,''),256)) PERSISTED VARCHAR(64);
ALTER TABLE artifact_links
 ADD COLUMN requirement_lookup_key AS (SHA2(COALESCE(requirement_id,''),256)) PERSISTED VARCHAR(64);
ALTER TABLE artifact_links
 ADD COLUMN session_lookup_key AS (SHA2(COALESCE(planning_session_id,''),256)) PERSISTED VARCHAR(64);

-- Approved keys. Hash keys use USING HASH. Rowstore keys are BTREE keys.
ALTER TABLE events_columnstore
 ADD KEY events_document_key_idx (workspace_id,event_document_key) USING HASH;
ALTER TABLE events_columnstore
 ADD KEY events_requirement_key_idx (workspace_id,event_requirement_key) USING HASH;
ALTER TABLE events_columnstore
 ADD KEY events_session_key_idx (workspace_id,event_session_key) USING HASH;
ALTER TABLE tasks
 ADD KEY tasks_workspace_state_created_idx (workspace_id,task_state_key,created_at,id);
ALTER TABLE tasks
 ADD KEY tasks_workspace_intake_lookup_idx (workspace_id,task_intake_key);
ALTER TABLE tasks ADD KEY tasks_workspace_created_idx (workspace_id,created_at,id);
ALTER TABLE artifacts ADD KEY artifacts_workspace_created_idx (workspace_id,created_at,id);
ALTER TABLE artifact_links
 ADD KEY artifact_links_artifact_lookup_idx (workspace_id,artifact_lookup_key) USING HASH;
ALTER TABLE artifact_links
 ADD KEY artifact_links_task_lookup_idx (workspace_id,task_lookup_key) USING HASH;
ALTER TABLE artifact_links
 ADD KEY artifact_links_requirement_lookup_idx (workspace_id,requirement_lookup_key) USING HASH;
ALTER TABLE artifact_links
 ADD KEY artifact_links_session_lookup_idx (workspace_id,session_lookup_key) USING HASH;

-- Postflight: SHOW CREATE must show each generated column as
-- AS (<approved expression>) PERSISTED VARCHAR(64).
SHOW CREATE TABLE events_columnstore;
SHOW CREATE TABLE tasks;
SHOW CREATE TABLE artifacts;
SHOW CREATE TABLE artifact_links;

SELECT table_name, index_name, column_name, seq_in_index, index_type, non_unique
FROM information_schema.statistics
WHERE table_schema=DATABASE()
  AND (
    (table_name='events_columnstore' AND index_name IN ('events_document_key_idx','events_requirement_key_idx','events_session_key_idx'))
    OR (table_name='tasks' AND index_name IN ('tasks_workspace_state_created_idx','tasks_workspace_intake_lookup_idx','tasks_workspace_created_idx'))
    OR (table_name='artifacts' AND index_name='artifacts_workspace_created_idx')
    OR (table_name='artifact_links' AND index_name IN ('artifact_links_artifact_lookup_idx','artifact_links_task_lookup_idx','artifact_links_requirement_lookup_idx','artifact_links_session_lookup_idx'))
  )
ORDER BY table_name, index_name, seq_in_index;

-- Postflight EXPLAIN: name the generated column and keep original equality.
-- Replace the quoted literals. A digest without the original comparison is
-- not the prepared predicate.
EXPLAIN SELECT id FROM events_columnstore
 WHERE workspace_id='WORKSPACE_ID'
   AND event_document_key=SHA2('DOCUMENT_ID',256)
   AND BINARY JSON_EXTRACT_STRING(payload_json,'document_id')=BINARY 'DOCUMENT_ID'
   AND kind LIKE 'system_design.%'
 ORDER BY at,id;

EXPLAIN SELECT id FROM events_columnstore
 WHERE workspace_id='WORKSPACE_ID'
   AND event_document_key=SHA2('DOCUMENT_ID',256)
   AND BINARY JSON_EXTRACT_STRING(payload_json,'document_id')=BINARY 'DOCUMENT_ID'
   AND kind LIKE 'reference_document.%'
 ORDER BY at,id;

EXPLAIN SELECT id FROM events_columnstore
 WHERE workspace_id='WORKSPACE_ID'
   AND event_requirement_key=SHA2('REQUIREMENT_ID',256)
   AND JSON_EXTRACT_STRING(payload_json,'requirement_id')='REQUIREMENT_ID'
 ORDER BY at,id;

EXPLAIN SELECT id FROM events_columnstore
 WHERE workspace_id='WORKSPACE_ID'
   AND event_session_key=SHA2('SESSION_ID',256)
   AND JSON_EXTRACT_STRING(payload_json,'session_id')='SESSION_ID'
 ORDER BY at,id;

EXPLAIN SELECT id FROM tasks
 WHERE workspace_id='WORKSPACE_ID'
   AND task_state_key=SHA2('STATE_VALUE',256)
   AND state='STATE_VALUE'
 ORDER BY created_at,id;

EXPLAIN SELECT intake_key,id,title,state FROM tasks
 WHERE workspace_id='WORKSPACE_ID'
   AND task_intake_key=SHA2('INTAKE_KEY',256)
   AND BINARY intake_key=BINARY 'INTAKE_KEY';

EXPLAIN SELECT id FROM tasks
 WHERE workspace_id='WORKSPACE_ID'
 ORDER BY created_at,id;

EXPLAIN SELECT id FROM artifacts
 WHERE workspace_id='WORKSPACE_ID'
 ORDER BY created_at,id;

EXPLAIN SELECT l.artifact_id FROM artifact_links l
 WHERE l.workspace_id='WORKSPACE_ID'
   AND l.artifact_lookup_key=SHA2('ARTIFACT_ID',256)
   AND BINARY l.artifact_id=BINARY 'ARTIFACT_ID';

EXPLAIN SELECT l.artifact_id FROM artifact_links l
 WHERE l.workspace_id='WORKSPACE_ID'
   AND l.task_lookup_key=SHA2('TASK_ID',256)
   AND BINARY l.task_id=BINARY 'TASK_ID';

EXPLAIN SELECT l.artifact_id FROM artifact_links l
 WHERE l.workspace_id='WORKSPACE_ID'
   AND l.requirement_lookup_key=SHA2('REQUIREMENT_ID',256)
   AND BINARY l.requirement_id=BINARY 'REQUIREMENT_ID';

EXPLAIN SELECT l.artifact_id FROM artifact_links l
 WHERE l.workspace_id='WORKSPACE_ID'
   AND l.session_lookup_key=SHA2('SESSION_ID',256)
   AND BINARY l.planning_session_id=BINARY 'SESSION_ID';
