-- req-operator-next-action AC-1.2. migrateTaskGateKind adds tasks.gate_kind
-- VARCHAR(32) NULL behind an information_schema check because SingleStore DDL
-- commits implicitly and does not support ADD COLUMN IF NOT EXISTS. This
-- statement is the idempotent backfill. It correlates events by workspace_id
-- and task_id, the composite task identity, and orders by (at DESC, id DESC).
-- Unmapped commands and non-awaiting rows stay NULL. Replay writes nothing
-- once gate_kind is set, and writes no events.
UPDATE tasks
SET gate_kind = (
    SELECT CASE JSON_EXTRACT_STRING(e.payload_json, 'command')
        WHEN 'gate.spec' THEN 'plan_approval'
        WHEN 'gate.plan_revision' THEN 'plan_revision'
        WHEN 'gate.merge' THEN 'merge_approval'
        WHEN 'stage.bounce_limit' THEN 'review_bounce_limit'
        WHEN 'job.fail' THEN 'job_failure'
        ELSE NULL
    END
    FROM events AS e
    WHERE e.workspace_id = tasks.workspace_id
      AND e.task_id = tasks.id
      AND e.kind = 'task.state_changed'
    ORDER BY e.at DESC, e.id DESC
    LIMIT 1
)
WHERE state = 'awaiting_human'
  AND gate_kind IS NULL
  AND (
    SELECT CASE JSON_EXTRACT_STRING(e.payload_json, 'command')
        WHEN 'gate.spec' THEN 'plan_approval'
        WHEN 'gate.plan_revision' THEN 'plan_revision'
        WHEN 'gate.merge' THEN 'merge_approval'
        WHEN 'stage.bounce_limit' THEN 'review_bounce_limit'
        WHEN 'job.fail' THEN 'job_failure'
        ELSE NULL
    END
    FROM events AS e
    WHERE e.workspace_id = tasks.workspace_id
      AND e.task_id = tasks.id
      AND e.kind = 'task.state_changed'
    ORDER BY e.at DESC, e.id DESC
    LIMIT 1
  ) IS NOT NULL;
