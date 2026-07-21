-- Admit spec-stage work orders (spec §21.33 change 1). The 009 constraint
-- predates spec delegation and rejected every spec-order INSERT with
-- SQLSTATE 23514, so tasks routed to spec failed dispatch and requeued.
ALTER TABLE work_orders DROP CONSTRAINT work_orders_stage_check;
ALTER TABLE work_orders ADD CONSTRAINT work_orders_stage_check
    CHECK (stage IN ('implement', 'review', 'spec'));
