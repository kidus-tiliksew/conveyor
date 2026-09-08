-- migrateRepositoryInstallColumn adds the column with a schema check because
-- SingleStore does not support ADD COLUMN IF NOT EXISTS.
CREATE ROWSTORE TABLE IF NOT EXISTS repository_install_tasks (
 workspace_id VARCHAR(255) NOT NULL,
 repository_name VARCHAR(255) NOT NULL,
 attempt INT NOT NULL,
 task_id VARCHAR(255) NOT NULL,
 PRIMARY KEY (workspace_id,repository_name,attempt),
 UNIQUE KEY (workspace_id,task_id),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
