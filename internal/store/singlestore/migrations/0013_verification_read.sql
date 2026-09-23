-- req-verification-kits REQ-6/REQ-7. feature-verification-kit-execution VK-9, DEC-43.
-- Read projections exclude evidence payloads and execution inputs.
-- Fixed UTC nanoseconds preserve timestamp ordering without timezone casts.

ALTER TABLE verification_contexts ADD COLUMN read_at AS (CASE WHEN COALESCE(JSON_EXTRACT_STRING(body,'CreatedAt'),'')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE CONCAT(LEFT(JSON_EXTRACT_STRING(body,'CreatedAt'),19),'.',RPAD(REPLACE(REPLACE(SUBSTRING(JSON_EXTRACT_STRING(body,'CreatedAt'),20),'Z',''),'.',''),9,'0'),'Z') END) PERSISTED VARCHAR(30) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE INDEX verification_contexts_read_page ON verification_contexts(workspace_id,task_id,read_at,id);


ALTER TABLE verification_attempts ADD COLUMN read_at AS (CASE WHEN COALESCE(JSON_EXTRACT_STRING(body,'StartedAt'),'')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE CONCAT(LEFT(JSON_EXTRACT_STRING(body,'StartedAt'),19),'.',RPAD(REPLACE(REPLACE(SUBSTRING(JSON_EXTRACT_STRING(body,'StartedAt'),20),'Z',''),'.',''),9,'0'),'Z') END) PERSISTED VARCHAR(30) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE INDEX verification_attempts_read_page ON verification_attempts(workspace_id,task_id,context_id,read_at,id);


ALTER TABLE verification_evidence ADD COLUMN read_at AS (CASE WHEN COALESCE(JSON_EXTRACT_STRING(body,'Envelope','received_at'),'')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE CONCAT(LEFT(JSON_EXTRACT_STRING(body,'Envelope','received_at'),19),'.',RPAD(REPLACE(REPLACE(SUBSTRING(JSON_EXTRACT_STRING(body,'Envelope','received_at'),20),'Z',''),'.',''),9,'0'),'Z') END) PERSISTED VARCHAR(30) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE INDEX verification_evidence_read_page ON verification_evidence(workspace_id,task_id,context_id,read_at,id);


ALTER TABLE verification_operations ADD COLUMN read_at AS (CASE WHEN COALESCE(JSON_EXTRACT_STRING(body,'CreatedAt'),'')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE CONCAT(LEFT(JSON_EXTRACT_STRING(body,'CreatedAt'),19),'.',RPAD(REPLACE(REPLACE(SUBSTRING(JSON_EXTRACT_STRING(body,'CreatedAt'),20),'Z',''),'.',''),9,'0'),'Z') END) PERSISTED VARCHAR(30) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE INDEX verification_operations_read_page ON verification_operations(workspace_id,task_id,context_id,read_at,id);


ALTER TABLE verification_publications ADD COLUMN read_at AS (CASE WHEN COALESCE(JSON_EXTRACT_STRING(body,'CreatedAt'),'')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE CONCAT(LEFT(JSON_EXTRACT_STRING(body,'CreatedAt'),19),'.',RPAD(REPLACE(REPLACE(SUBSTRING(JSON_EXTRACT_STRING(body,'CreatedAt'),20),'Z',''),'.',''),9,'0'),'Z') END) PERSISTED VARCHAR(30) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE INDEX verification_publications_read_page ON verification_publications(workspace_id,task_id,context_id,read_at,id);


ALTER TABLE verification_obligations ADD COLUMN read_at AS (CASE WHEN COALESCE(JSON_EXTRACT_STRING(body,'CreatedAt'),'')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE CONCAT(LEFT(JSON_EXTRACT_STRING(body,'CreatedAt'),19),'.',RPAD(REPLACE(REPLACE(SUBSTRING(JSON_EXTRACT_STRING(body,'CreatedAt'),20),'Z',''),'.',''),9,'0'),'Z') END) PERSISTED VARCHAR(30) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE INDEX verification_obligations_read_page ON verification_obligations(workspace_id,task_id,context_id,read_at,id);
