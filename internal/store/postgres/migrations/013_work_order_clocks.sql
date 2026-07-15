-- Queue residence, execution wall clock, and claim leases are independent.
-- External jobs remain unstarted until their first successful claim.
ALTER TABLE jobs
    ALTER COLUMN started_at DROP NOT NULL;

ALTER TABLE work_orders
    DROP CONSTRAINT work_orders_state_check,
    ADD CONSTRAINT work_orders_state_check CHECK (
        state IN ('queued', 'claimed', 'submitted', 'completed', 'cancelled', 'stale', 'timed_out')
    ),
    ADD COLUMN queue_entered_at timestamptz,
    ADD COLUMN queue_deadline timestamptz,
    ADD COLUMN execution_started_at timestamptz,
    ADD COLUMN execution_deadline timestamptz,
    ADD COLUMN redispatch_count integer NOT NULL DEFAULT 0 CHECK (redispatch_count >= 0);

UPDATE work_orders
SET queue_entered_at = created_at,
    queue_deadline = created_at + interval '24 hours';

-- Preserve historical claim linkage. Execution deadlines for already active
-- rows remain unset because their configured stage timeout is workspace data,
-- not migration-time schema data; the next claim records a fixed deadline.
UPDATE work_orders wo
SET execution_started_at = COALESCE((
    SELECT min(e.at)
    FROM events e
    WHERE e.job_id = wo.job_id AND e.kind = 'work_order.claimed'
), j.started_at)
FROM jobs j
WHERE j.id = wo.job_id
  AND (
      wo.state IN ('claimed', 'submitted', 'completed')
      OR EXISTS (
          SELECT 1 FROM events e
          WHERE e.job_id = wo.job_id AND e.kind = 'work_order.claimed'
      )
  );

UPDATE jobs j
SET started_at = NULL,
    updated_at = now()
FROM work_orders wo
WHERE wo.job_id = j.id
  AND j.harness = 'external-mcp'
  AND wo.execution_started_at IS NULL;

ALTER TABLE work_orders
    ALTER COLUMN queue_entered_at SET NOT NULL,
    ALTER COLUMN queue_entered_at SET DEFAULT now(),
    ALTER COLUMN queue_deadline SET NOT NULL,
    ALTER COLUMN queue_deadline SET DEFAULT (now() + interval '24 hours');

DROP INDEX work_orders_queue_idx;
CREATE INDEX work_orders_queue_idx
    ON work_orders (workspace_id, state, queue_deadline, queue_entered_at);
