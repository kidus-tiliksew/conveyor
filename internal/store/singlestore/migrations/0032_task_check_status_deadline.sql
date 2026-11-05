-- req-review-gates-evidence AC-1.5; component-submission-merge (Check status,
-- invariant 61). migrate.go adds the nullable tasks.check_status_deadline and
-- tasks.check_status_deadline_head columns before this file runs. SingleStore
-- DDL commits implicitly and has no ADD COLUMN IF NOT EXISTS, so the add lives
-- in Go behind an information_schema check. There is no backfill.
SELECT 1;
