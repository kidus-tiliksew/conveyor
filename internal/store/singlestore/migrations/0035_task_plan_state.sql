-- component-plan-state invariants 1 and 8, req-task-plan-state REQ-1 and
-- AC-2.5, and DEC-122(5). migrate.go adds tasks.plan_state and task_specs.origin and
-- task_specs.base_commit, then fills each task's plan state once, after this
-- file runs. SingleStore DDL commits implicitly and has no ADD COLUMN IF NOT
-- EXISTS, so each add lives in Go behind an information_schema check, and the
-- fill writes only rows still without a plan state, so a rerun after an
-- interrupted startup fills nothing twice. SingleStore has no CHECK, so the Go
-- writers enforce the value sets. No event is written.
SELECT 1;
