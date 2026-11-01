-- component-persistence (Launcher-record retirement), component-work-orders,
-- and component-identity-membership, task 261101-699ace under DEC-97, DEC-94,
-- and DEC-98. Conveyor launches no agent session, so the records that only the
-- worker plane, the run-order plane, and attempt observability wrote are
-- removed. Every statement is idempotent. No event row is rewritten: events
-- with actor role runner (the retired worker role) and task-less worker kinds
-- stay readable, and events_scope_check keeps those kinds valid.

-- Run-child agent credentials carried their run binding in the label. Revoke
-- each one so it is refused at its next use. No row is deleted.
UPDATE user_tokens
SET revoked_at = now()
WHERE kind = 'agent' AND label LIKE 'conveyor-run:%' AND revoked_at IS NULL;

DROP TABLE IF EXISTS work_order_transcript_captures;
DROP TABLE IF EXISTS work_order_activity_snapshots;
DROP TABLE IF EXISTS harness_model_failures;
DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS worker_pairings;

-- work_orders.worker_id stays as the historical claim owner of orders claimed
-- before DEC-97, and no write sets it.
ALTER TABLE work_orders
    DROP COLUMN IF EXISTS continuation_session_id,
    DROP COLUMN IF EXISTS continuation_attempt_id,
    DROP COLUMN IF EXISTS continuation_harness,
    DROP COLUMN IF EXISTS continuation_launch_environment;
