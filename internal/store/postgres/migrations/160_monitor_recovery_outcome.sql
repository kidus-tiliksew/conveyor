-- component-monitor-drift invariant 17. A recovery observation links the open
-- post-merge task with the outcome recovery_observed, which the CHECK from
-- migration 040 refused with SQLSTATE 23514. SingleStore declares this column
-- with no CHECK, so only this migration set needs a file (component-persistence
-- invariant 35). No recorded row changes, and earlier recovery observations stay
-- unlinked.
ALTER TABLE monitor_observations
    DROP CONSTRAINT monitor_observations_task_outcome_check,
    ADD CONSTRAINT monitor_observations_task_outcome_check
        CHECK (task_outcome IN ('','created','reused','recovery_observed'));
