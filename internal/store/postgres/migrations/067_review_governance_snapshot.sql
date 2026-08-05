-- Pin review governance authority at claim, matching migration 064 semantics.
ALTER TABLE work_orders
    ADD COLUMN governance_snapshot jsonb;

ALTER TABLE work_orders
    ADD CONSTRAINT work_orders_governance_snapshot_object_check
    CHECK (governance_snapshot IS NULL OR jsonb_typeof(governance_snapshot) = 'object');
