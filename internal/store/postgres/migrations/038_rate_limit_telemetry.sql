ALTER TABLE work_orders
    ADD COLUMN rate_limit jsonb,
    ADD COLUMN rate_limit_observed_at timestamptz;
