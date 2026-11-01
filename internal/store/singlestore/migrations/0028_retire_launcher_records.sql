-- component-persistence (Launcher-record retirement), component-work-orders,
-- and component-identity-membership, task 261101-699ace under DEC-97, DEC-94,
-- and DEC-98. migrate.go drops the four work_orders.continuation_* columns
-- behind information_schema checks before this file runs, because SingleStore
-- has no DROP COLUMN IF EXISTS and DDL commits implicitly. This file revokes
-- the run-child agent credentials and drops the retired tables. Every
-- statement is safe to retry. No event row is rewritten.
UPDATE user_tokens SET revoked_at = CURRENT_TIMESTAMP(6) WHERE kind = 'agent' AND label LIKE 'conveyor-run:%' AND revoked_at IS NULL;
DROP TABLE IF EXISTS work_order_transcript_captures;
DROP TABLE IF EXISTS work_order_activity_snapshots;
DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS worker_pairings;
