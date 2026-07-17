-- Preserve the full worker execution definition used by an in-flight review
-- seat so workspace hot reloads cannot mutate its enforcement contract
-- (spec §21.12 change 4).
ALTER TABLE work_orders
    ADD COLUMN required_harness_config jsonb NOT NULL DEFAULT '{}'::jsonb;
