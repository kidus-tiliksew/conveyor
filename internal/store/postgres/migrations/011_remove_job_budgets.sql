-- v1.6 removes allocation and enforcement while retaining usage telemetry.
-- The only producer of paused jobs was the retired budget circuit breaker.
UPDATE jobs
SET state = 'failed', updated_at = now()
WHERE state = 'paused';

ALTER TABLE jobs DROP COLUMN IF EXISTS budget_usd;
