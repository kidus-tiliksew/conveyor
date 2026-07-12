ALTER TABLE tasks
    ADD COLUMN next_stage text NOT NULL DEFAULT '',
    ADD COLUMN recovery_stage text NOT NULL DEFAULT '';

-- Preserve the recovery point for tasks that were active when this projection
-- was introduced. This is a one-time migration only; runtime dispatch never
-- derives transitions from historical output again.
UPDATE tasks t
SET next_stage = CASE WHEN t.escalation_level = '' THEN 'implement' ELSE 'triage' END
WHERE t.state IN ('claiming', 'queued', 'running')
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.task_id = t.id);

WITH latest_job AS (
    SELECT DISTINCT ON (task_id) id, task_id, stage, state
    FROM jobs
    ORDER BY task_id, started_at DESC, id DESC
)
UPDATE tasks t
SET next_stage = CASE
    WHEN latest_job.state <> 'done' THEN latest_job.stage
    WHEN latest_job.stage = 'implement' THEN 'review'
    WHEN latest_job.stage = 'spec' AND EXISTS (
        SELECT 1 FROM task_specs s
        WHERE s.task_id = t.id AND s.approved
          AND s.version = (SELECT MAX(version) FROM task_specs WHERE task_id = t.id)
    ) THEN 'implement'
    WHEN latest_job.stage = 'review' AND EXISTS (
        SELECT 1 FROM events e
        WHERE e.task_id = t.id AND e.job_id = latest_job.id
          AND e.kind = 'review.completed'
          AND e.payload_json->>'verdict' = 'changes_requested'
    ) THEN 'implement'
    WHEN latest_job.stage = 'triage' THEN COALESCE((
        SELECT CASE
            WHEN t.escalation_level = 'L3' OR e.payload_json->>'route' IN ('human', 'parked') THEN ''
            WHEN t.escalation_level = 'L2' OR e.payload_json->>'route' = 'spec' THEN 'spec'
            ELSE 'implement'
        END
        FROM events e
        WHERE e.task_id = t.id AND e.job_id = latest_job.id AND e.kind = 'triage.completed'
        ORDER BY e.id DESC LIMIT 1
    ), '')
    ELSE ''
END
FROM latest_job
WHERE latest_job.task_id = t.id
  AND t.state IN ('queued', 'running');

WITH latest_job AS (
    SELECT DISTINCT ON (task_id) task_id, stage, state
    FROM jobs
    ORDER BY task_id, started_at DESC, id DESC
)
UPDATE tasks t
SET next_stage = latest_job.stage
FROM latest_job
WHERE latest_job.task_id = t.id
  AND t.state = 'awaiting_human'
  AND latest_job.state = 'paused';

UPDATE tasks
SET recovery_stage = next_stage,
    next_stage = ''
WHERE state = 'awaiting_human' AND next_stage <> '';

CREATE INDEX tasks_next_stage_idx ON tasks (workspace_id, next_stage)
    WHERE next_stage <> '';
