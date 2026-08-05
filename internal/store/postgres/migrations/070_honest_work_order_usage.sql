ALTER TABLE work_orders
    ADD COLUMN usage_reported boolean NOT NULL DEFAULT false;

-- Historical non-zero telemetry and rows backed by an accepted usage event are
-- reported. All other zero rows remain unavailable. Usage remains observational
-- and cannot affect lifecycle behavior (DEC-1).
UPDATE work_orders AS work_order
SET usage_reported = true
WHERE work_order.tokens_in <> 0
   OR work_order.tokens_out <> 0
   OR work_order.cost_usd <> 0
   OR EXISTS (
       SELECT 1
       FROM events AS event
       WHERE event.workspace_id = work_order.workspace_id
         AND event.task_id = work_order.task_id
         AND event.kind = 'work_order.usage_reported'
         AND event.payload_json ->> 'work_order_id' = work_order.id
   );
