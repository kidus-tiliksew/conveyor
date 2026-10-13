-- feature-bounded-automatic-recovery AR-2. Nullable ledger columns with no
-- backfill. automatic_recovery_count is read as COALESCE(..., 0). The
-- re-enqueue stamps are persisted for AR-5 and are not read by this slice.
ALTER TABLE work_orders
    ADD COLUMN IF NOT EXISTS automatic_recovery_count integer,
    ADD COLUMN IF NOT EXISTS automatic_recovery_window_started_at timestamptz,
    ADD COLUMN IF NOT EXISTS next_automatic_recovery_at timestamptz;

ALTER TABLE github_lifecycles
    ADD COLUMN IF NOT EXISTS automatic_reenqueue_at timestamptz;

ALTER TABLE review_publications
    ADD COLUMN IF NOT EXISTS automatic_reenqueue_at timestamptz;
