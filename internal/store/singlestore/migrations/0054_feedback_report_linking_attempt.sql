-- component-feedback-reports invariant 15, feature-production-feedback-listening
-- FR-2, and approved plan version 2 of task 261127-6d47db. migrate.go adds the
-- nullable feedback_reports.linking_last_attempt_at DATETIME(6) column, the
-- time of a report's last linking attempt, before this file runs. SingleStore
-- DDL commits implicitly and has no ADD COLUMN IF NOT EXISTS, so the add lives
-- in Go behind an information_schema check, and an interrupted startup can run
-- it again. NULL means that no attempt was claimed yet. No backfill.
SELECT 1;
