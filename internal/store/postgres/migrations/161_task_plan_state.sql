-- component-plan-state invariants 1 and 8; req-task-plan-state REQ-1, AC-2.5;
-- DEC-122(5). Every task carries one plan state, and every plan version names
-- the act that recorded it and the commit its plan was read against. The Go
-- hook keyed by this version in migrate.go fills tasks.plan_state once per
-- task in this migration's transaction. Existing plan versions take the
-- plan_order origin and no base commit from the column defaults. No events row
-- is written (component-persistence invariant 37).
ALTER TABLE tasks
    ADD COLUMN plan_state text NOT NULL DEFAULT 'needed'
        CONSTRAINT tasks_plan_state_check
        CHECK (plan_state IN ('not_required','needed','awaiting_approval','completed'));

ALTER TABLE task_specs
    ADD COLUMN origin text NOT NULL DEFAULT 'plan_order'
        CONSTRAINT task_specs_origin_check
        CHECK (origin IN ('plan_order','intake','operator')),
    ADD COLUMN base_commit text NULL
        CONSTRAINT task_specs_base_commit_check
        CHECK (base_commit IS NULL OR base_commit ~ '^[0-9a-f]{40}$');
