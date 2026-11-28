-- component-feedback-reports invariant 17 and feature-production-feedback-listening
-- FR-3. A user who may create tasks can reopen a closed report, and each act
-- stores its actor and time. The reopening columns that migration 182 added
-- held only a reopening by a report (reopen_source 'report'). This admits
-- 'user' as the source of a reopening that a person recorded: its label is
-- empty and its actor is the user. The four reopening columns stay all set or
-- all NULL. No backfill: every stored reopening was made by a report, so
-- every stored row meets the new check. The check is added NOT VALID, as
-- migration 182 adds events_scope_check, so adding it scans no stored row
-- under the table lock. It holds for every write from now on.
ALTER TABLE feedback_reports DROP CONSTRAINT IF EXISTS feedback_reports_reopen_source_check;
ALTER TABLE feedback_reports ADD CONSTRAINT feedback_reports_reopen_source_check CHECK (reopen_source IN ('report', 'user')) NOT VALID;
