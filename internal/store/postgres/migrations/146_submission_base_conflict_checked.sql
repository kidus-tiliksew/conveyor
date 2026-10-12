-- feature-bounded-automatic-recovery AR-4 (pre-dispatch conflict check). The
-- dispatcher stamps the submitted implement order once, before its single
-- mergeability read, so each submitted head is checked at most once. Nullable,
-- no backfill: existing submissions are treated as unchecked.
ALTER TABLE work_orders
    ADD COLUMN IF NOT EXISTS base_conflict_checked_at timestamptz;
