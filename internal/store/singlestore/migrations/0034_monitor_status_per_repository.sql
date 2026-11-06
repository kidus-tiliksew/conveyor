-- component-monitor-drift invariants 4 and 15, and req-delivery-and-forge AC-4.5.
-- monitor_status moves from one row per workspace to one row per workspace and
-- repository, and stores each repository's last-processed tip. SingleStore can't
-- change a table's primary key, so the table is recreated. An existing row
-- describes whichever repository was polled last and names none, so no pass
-- could ever clear it, and the poll at daemon start writes each monitored
-- repository's row. A rerun after an interrupted startup recreates the same
-- empty table, and a repository with no tip keeps the window read.
DROP TABLE IF EXISTS monitor_status;

CREATE ROWSTORE TABLE IF NOT EXISTS `monitor_status` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `repository` VARCHAR(255) NOT NULL,
 `last_successful_at` DATETIME(6),
 `current_error` LONGTEXT NOT NULL DEFAULT '',
 `forge_error_category` LONGTEXT NOT NULL DEFAULT '',
 `backoff_until` DATETIME(6),
 `last_processed_tip` VARCHAR(255) NOT NULL DEFAULT '',
 PRIMARY KEY (`workspace_id`, `repository`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
