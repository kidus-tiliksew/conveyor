-- feature-bounded-automatic-recovery AR-2. migrateAutomaticRecoveryLedger adds
-- the nullable columns before this file runs. SingleStore DDL commits
-- implicitly and has no ADD COLUMN IF NOT EXISTS, so the adds live in Go
-- behind information_schema checks. There is no backfill. Counts are read as
-- COALESCE(..., 0). automatic_reenqueue_at is persisted for AR-5 and unread here.
SELECT 1;
