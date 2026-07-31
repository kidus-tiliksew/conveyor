ALTER TABLE work_orders
    ADD COLUMN attempt_id text NOT NULL DEFAULT '',
    ADD COLUMN last_attempt_id text NOT NULL DEFAULT '',
    ADD COLUMN last_failure_category text NOT NULL DEFAULT '';
