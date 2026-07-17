-- Snapshot the configured execution timeout at dispatch so later workspace
-- edits cannot change an already queued work order's deadline contract
-- (spec §21.18 change 5).
ALTER TABLE work_orders
    ADD COLUMN execution_timeout text NOT NULL DEFAULT '';
