-- req-operator-next-action AC-1.2. gate_kind is a row projection of the
-- transition that entered awaiting_human. NULL and '' both read as empty.
-- Unmapped latest commands stay NULL and are flagged by the audit. No event
-- is written. Replay is a no-op: the column add is IF NOT EXISTS and the
-- backfill only fills a still-NULL awaiting row whose latest command maps.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS gate_kind TEXT;

UPDATE tasks AS t
SET gate_kind = mapped.kind
FROM (
    SELECT task.id, kinds.kind
    FROM tasks AS task
    JOIN LATERAL (
        SELECT e.payload_json->>'command' AS command
        FROM events AS e
        WHERE e.task_id = task.id
          AND e.kind = 'task.state_changed'
        ORDER BY e.at DESC, e.id DESC
        LIMIT 1
    ) AS latest ON true
    JOIN (
        VALUES
            ('gate.spec', 'plan_approval'),
            ('gate.plan_revision', 'plan_revision'),
            ('gate.merge', 'merge_approval'),
            ('stage.bounce_limit', 'review_bounce_limit'),
            ('job.fail', 'job_failure')
    ) AS kinds(command, kind) ON kinds.command = latest.command
    WHERE task.state = 'awaiting_human'
      AND task.gate_kind IS NULL
) AS mapped
WHERE t.id = mapped.id;
