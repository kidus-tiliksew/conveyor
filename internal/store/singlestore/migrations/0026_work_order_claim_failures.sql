-- component-work-orders (Claim admission) and component-persistence (Schema
-- domains). One bounded record per work order of its consecutive claims that
-- failed with store.ErrBackendOperation. A successful claim deletes the row.
-- No driver text is stored. Go checks a positive count and a non-empty class.
CREATE ROWSTORE TABLE IF NOT EXISTS work_order_claim_failures (
 workspace_id VARCHAR(255) NOT NULL,
 work_order_id VARCHAR(255) NOT NULL,
 consecutive_failures INT NOT NULL,
 first_failed_at DATETIME(6) NOT NULL,
 last_failed_at DATETIME(6) NOT NULL,
 error_class VARCHAR(64) NOT NULL,
 PRIMARY KEY (workspace_id, work_order_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
