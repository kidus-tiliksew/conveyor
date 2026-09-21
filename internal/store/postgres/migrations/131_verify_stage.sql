-- DEC-43; feature-verification-kit-execution VK-2. Existing rows retain their
-- frozen policy and stage. Verify is an additive work-order vocabulary entry.
ALTER TABLE work_orders DROP CONSTRAINT work_orders_stage_check;
ALTER TABLE work_orders ADD CONSTRAINT work_orders_stage_check
    CHECK (stage IN ('implement', 'review', 'spec', 'verify'));
