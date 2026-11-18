-- component-crates, feature-crates CR-1, and req-crates REQ-1 and REQ-2. A crate is a
-- workspace record that groups tasks for tracking and reporting: a name, an
-- optional target date, a declared requirement list, and a state of open or
-- closed. The Go step keyed by this version in migrate.go adds the nullable
-- tasks.crate_id column after these tables exist, so existing tasks keep a
-- NULL crate. SingleStore has no CHECK or foreign key, so the Go writers keep
-- the name, date, and state rules and the workspace of each reference. No
-- dispatch, claim, gate, review, merge, next-action, or lineage query reads
-- these tables or the column (component-crates invariant 1 and DEC-141).
CREATE ROWSTORE TABLE IF NOT EXISTS crates (
 workspace_id VARCHAR(255) NOT NULL,
 id VARCHAR(64) NOT NULL,
 name VARCHAR(255) NOT NULL,
 target_date DATE NULL,
 state VARCHAR(16) NOT NULL,
 created_by VARCHAR(255) NOT NULL,
 created_at DATETIME(6) NOT NULL,
 closed_at DATETIME(6) NULL,
 PRIMARY KEY (workspace_id, id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- The declared list. An edit replaces it whole, and it changes no task's
-- context attachments (component-crates invariant 5 and req-crates AC-3.3).
CREATE ROWSTORE TABLE IF NOT EXISTS crate_requirements (
 workspace_id VARCHAR(255) NOT NULL,
 crate_id VARCHAR(64) NOT NULL,
 requirement_id VARCHAR(255) NOT NULL,
 PRIMARY KEY (workspace_id, crate_id, requirement_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
