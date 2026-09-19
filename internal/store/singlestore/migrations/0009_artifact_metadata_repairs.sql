-- component-persistence ART-STORE-4. No historical metadata is rewritten.
CREATE ROWSTORE TABLE IF NOT EXISTS artifact_metadata_repairs (
 workspace_id VARCHAR(255) NOT NULL,
 request_id VARCHAR(255) NOT NULL,
 artifact_id VARCHAR(255) NOT NULL,
 expected_old_content_type TEXT NOT NULL,
 new_content_type TEXT NOT NULL,
 result JSON NOT NULL,
 actor TEXT NOT NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY(workspace_id,request_id),
 SHARD KEY(workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
