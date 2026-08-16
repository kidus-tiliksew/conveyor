-- Retire historical queued/stale work orders whose stage already has an
-- authoritative successor or whose task has advanced beyond that stage.
-- Selection is materialized before mutation so the audit projection is exact
-- and reruns are a no-op.
CREATE TEMP TABLE work_order_zombie_backfill ON COMMIT DROP AS
SELECT w.workspace_id,
       w.id AS work_order_id,
       w.task_id,
       w.job_id,
       w.stage,
       w.state AS prior_state,
       COALESCE((
           SELECT successor.id
           FROM work_orders successor
           WHERE successor.workspace_id = w.workspace_id
             AND successor.task_id = w.task_id
             AND successor.stage = w.stage
             AND successor.id <> w.id
             AND successor.state IN ('claimed', 'completed')
             AND (successor.created_at, successor.id) > (w.created_at, w.id)
           ORDER BY successor.created_at DESC, successor.id DESC
           LIMIT 1
       ), '') AS authoritative_work_order_id,
       CASE
           WHEN EXISTS (
               SELECT 1 FROM work_orders successor
               WHERE successor.workspace_id = w.workspace_id
                 AND successor.task_id = w.task_id
                 AND successor.stage = w.stage
                 AND successor.id <> w.id
                 AND successor.state IN ('claimed', 'completed')
                 AND (successor.created_at, successor.id) > (w.created_at, w.id)
           ) THEN 'historical same-stage successor'
           ELSE 'task advanced beyond work-order stage'
       END AS reason
FROM work_orders w
JOIN tasks t ON t.workspace_id = w.workspace_id AND t.id = w.task_id
WHERE w.state IN ('queued', 'stale')
  AND (
      EXISTS (
          SELECT 1 FROM work_orders successor
          WHERE successor.workspace_id = w.workspace_id
            AND successor.task_id = w.task_id
            AND successor.stage = w.stage
            AND successor.id <> w.id
            AND successor.state IN ('claimed', 'completed')
            AND (successor.created_at, successor.id) > (w.created_at, w.id)
      )
      OR t.state IN ('approved', 'merged', 'closed')
      OR (w.stage = 'spec' AND (
          t.next_stage IN ('implement', 'review')
          OR t.recovery_stage IN ('implement', 'review')
          OR EXISTS (SELECT 1 FROM work_orders later_stage
                     WHERE later_stage.workspace_id = w.workspace_id
                       AND later_stage.task_id = w.task_id
                       AND later_stage.stage IN ('implement', 'review')
                       AND later_stage.state <> 'cancelled')
      ))
      OR (w.stage = 'implement' AND (
          t.next_stage = 'review'
          OR t.recovery_stage = 'review'
          OR EXISTS (SELECT 1 FROM work_orders later_stage
                     WHERE later_stage.workspace_id = w.workspace_id
                       AND later_stage.task_id = w.task_id
                       AND later_stage.stage = 'review'
                       AND later_stage.state <> 'cancelled')
      ))
  );

UPDATE work_orders w
SET state = 'cancelled',
    claimant_id = '',
    session_id = '',
    attempt_id = '',
    client_token_hash = '',
    agent = '',
    model = '',
    worker_id = '',
    lease_expires_at = NULL,
    model_enforcement = '',
    last_attempt_outcome = 'cancelled',
    next_retry_at = NULL,
    retry_suppressed = true,
    retry_suppression_reason = 'superseded',
    updated_at = now()
FROM work_order_zombie_backfill repair
WHERE w.workspace_id = repair.workspace_id
  AND w.id = repair.work_order_id
  AND w.state = repair.prior_state;

UPDATE jobs j
SET state = 'failed',
    ended_at = COALESCE(j.ended_at, now()),
    updated_at = now()
FROM work_order_zombie_backfill repair
WHERE j.id = repair.job_id;

-- The migration runner appends one work_order.retired event per row from the
-- temporary repair table. Application code owns ledger writes so schema SQL
-- never mutates the append-only event stream directly.
