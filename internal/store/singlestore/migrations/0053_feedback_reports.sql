-- component-feedback-reports, feature-production-feedback-listening FR-1,
-- req-production-feedback REQ-4 and REQ-11, req-environments AC-2.4,
-- DEC-149(1), and DEC-139(2). One record for each report, its links with
-- their evidence, its notes, and the answer of each line read. Nothing is
-- stored for each occurrence. The four tables share the workspace as shard
-- key with the environment tables, so every row of one report request is on
-- one partition, beside the environments row that the workspace environment
-- claim names as its witness (component-persistence invariant 8). The Go
-- writers hold that claim and keep the length and reference rules. Rows are
-- updated in place, so every table is rowstore. An empty reported value was
-- not stated. No dispatch, claim, gate, review, merge, lineage, next-action,
-- or work-order query reads these tables (component-feedback-reports
-- invariant 1). Restart-safe: every statement is IF NOT EXISTS. No backfill.

-- One row for each report. key_digest is the SHA-256 of the exact problem
-- key, so no collation decides which keys are equal. first_seen is the
-- earliest first-seen time of every entry, and first_entry_seen is the first
-- entry's own first-seen time, which no later entry changes
-- (component-feedback-reports invariants 6, 11, and 13). summary is redacted
-- and summary_redactions counts its removals by pattern class (invariant 5).
--
-- Every time is DATETIME(6). A stated time is never earlier than
-- 1970-01-01T00:00:00Z and never later than the arrival
-- (component-feedback-reports v2 invariant 4), so it lies inside the
-- DATETIME(6) range. The last_seen key keeps newest-last-seen-first reads on
-- an index.
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_reports (
 workspace_id VARCHAR(255) NOT NULL,
 id VARCHAR(64) NOT NULL,
 environment_id VARCHAR(64) NOT NULL,
 key_digest VARCHAR(64) NOT NULL,
 problem_key LONGTEXT NOT NULL,
 kind VARCHAR(16) NOT NULL,
 summary LONGTEXT NOT NULL,
 summary_redactions LONGTEXT NOT NULL,
 service LONGTEXT NOT NULL,
 occurrence_count BIGINT NOT NULL,
 first_seen DATETIME(6) NOT NULL,
 last_seen DATETIME(6) NOT NULL,
 first_entry_seen DATETIME(6) NOT NULL,
 file LONGTEXT NOT NULL,
 line INT NULL,
 first_repository LONGTEXT NOT NULL,
 first_commit VARCHAR(64) NOT NULL,
 first_version LONGTEXT NOT NULL,
 latest_repository LONGTEXT NOT NULL,
 latest_commit VARCHAR(64) NOT NULL,
 latest_version LONGTEXT NOT NULL,
 state VARCHAR(16) NOT NULL,
 in_inbox TINYINT NOT NULL,
 source VARCHAR(32) NOT NULL,
 source_label VARCHAR(255) NOT NULL,
 actor_id VARCHAR(255) NOT NULL,
 created_at DATETIME(6) NOT NULL,
 closed_at DATETIME(6) NULL,
 closed_by VARCHAR(255) NULL,
 close_reason LONGTEXT NULL,
 reopened_at DATETIME(6) NULL,
 reopen_source VARCHAR(32) NULL,
 reopen_source_label VARCHAR(255) NULL,
 reopen_actor_id VARCHAR(255) NULL,
 linking_ended_at DATETIME(6) NULL,
 fix_task_id VARCHAR(255) NULL,
 fix_task_set_by VARCHAR(255) NULL,
 fix_task_set_at DATETIME(6) NULL,
 PRIMARY KEY (workspace_id, id),
 UNIQUE KEY feedback_reports_key_idx (workspace_id, environment_id, key_digest),
 KEY feedback_reports_environment_state_idx (workspace_id, environment_id, state),
 KEY feedback_reports_last_seen_idx (workspace_id, last_seen DESC),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- One row for each link of a report to a task or a crate, with its evidence.
-- A removed link stays, so no rule links that task again
-- (component-feedback-reports invariants 11, 16, and 17). The writer keeps
-- one link for each report and task, and for each report and crate.
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_report_links (
 workspace_id VARCHAR(255) NOT NULL,
 id VARCHAR(64) NOT NULL,
 report_id VARCHAR(64) NOT NULL,
 task_id VARCHAR(255) NULL,
 crate_id VARCHAR(255) NULL,
 state VARCHAR(16) NOT NULL,
 evidence LONGTEXT NOT NULL,
 created_at DATETIME(6) NOT NULL,
 created_by VARCHAR(255) NOT NULL,
 confirmed_by VARCHAR(255) NULL,
 confirmed_at DATETIME(6) NULL,
 removed_by VARCHAR(255) NULL,
 removed_at DATETIME(6) NULL,
 PRIMARY KEY (workspace_id, id),
 KEY feedback_report_links_report_idx (workspace_id, report_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- One row for each note of a report (component-feedback-reports invariants
-- 12, 14, and 15). detail holds what the note names.
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_report_notes (
 workspace_id VARCHAR(255) NOT NULL,
 id VARCHAR(64) NOT NULL,
 report_id VARCHAR(64) NOT NULL,
 kind VARCHAR(32) NOT NULL,
 detail LONGTEXT NOT NULL,
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, id),
 KEY feedback_report_notes_report_idx (workspace_id, report_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;

-- The answer of the line read of a report: which commit last changed the
-- line of the file as of the resolved commit. An answered read is never asked
-- again (component-feedback-reports invariants 12 and 15).
CREATE ROWSTORE TABLE IF NOT EXISTS feedback_report_line_reads (
 workspace_id VARCHAR(255) NOT NULL,
 report_id VARCHAR(64) NOT NULL,
 repository LONGTEXT NOT NULL,
 resolved_commit VARCHAR(64) NOT NULL,
 file LONGTEXT NOT NULL,
 line INT NOT NULL,
 answer LONGTEXT NOT NULL,
 answered_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, report_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
