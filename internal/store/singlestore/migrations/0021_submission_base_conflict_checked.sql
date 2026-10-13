-- feature-bounded-automatic-recovery AR-4 (pre-dispatch conflict check).
-- migrate.go adds the nullable work_orders.base_conflict_checked_at column
-- before this file runs. SingleStore DDL commits implicitly and has no ADD
-- COLUMN IF NOT EXISTS, so the add lives in Go behind an information_schema
-- check. There is no backfill.
SELECT 1;
