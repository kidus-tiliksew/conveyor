ALTER TABLE work_orders
    ADD COLUMN served_requirement_snapshot jsonb;

ALTER TABLE work_orders
    ADD CONSTRAINT work_orders_served_requirement_snapshot_array_check
    CHECK (served_requirement_snapshot IS NULL OR jsonb_typeof(served_requirement_snapshot) = 'array');
