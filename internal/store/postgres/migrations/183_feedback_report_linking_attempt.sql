-- component-feedback-reports invariant 15, feature-production-feedback-listening
-- FR-2, and approved plan version 2 of task 261127-6d47db. The time of a
-- report's last linking attempt. The background linking work claims one
-- attempt for one report no more than once a minute: under the report's row
-- lock it compares the exact attempt time with this stored time, and it writes
-- the new time only when the claim succeeds. NULL means that no attempt was
-- claimed yet. No backfill: every stored report starts with no attempt.
ALTER TABLE feedback_reports ADD COLUMN IF NOT EXISTS linking_last_attempt_at timestamptz NULL;
