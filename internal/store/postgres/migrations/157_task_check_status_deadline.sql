-- req-review-gates-evidence AC-1.5 and component-submission-merge (Check status,
-- invariant 61). The first read of a head that finds a check pending, or that
-- fails, records a deadline 2 hours later and that head. Both columns stay NULL
-- on every existing task. No events row is written.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS check_status_deadline timestamptz;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS check_status_deadline_head text;
CREATE INDEX IF NOT EXISTS tasks_check_status_deadline_idx
    ON tasks (workspace_id, check_status_deadline)
    WHERE check_status_deadline IS NOT NULL;
