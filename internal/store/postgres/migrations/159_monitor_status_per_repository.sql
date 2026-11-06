-- component-monitor-drift invariants 4 and 15, and req-delivery-and-forge AC-4.5.
-- monitor_status moves from one row per workspace to one row per workspace and
-- repository, and stores each repository's last-processed tip. An existing row
-- describes whichever repository was polled last and names none, so no pass
-- could ever clear it. It is removed, and the poll at daemon start writes each
-- monitored repository's row. No observation or drift record changes, and a
-- repository with no tip keeps the window read.
DELETE FROM monitor_status;

ALTER TABLE monitor_status
    ADD COLUMN repository text NOT NULL,
    ADD COLUMN last_processed_tip text NOT NULL DEFAULT '',
    DROP CONSTRAINT monitor_status_pkey,
    ADD PRIMARY KEY (workspace_id, repository);
