-- The workspace badge's narrow read model. It folds only boolean attention
-- facts and never selects the general task/work-order JSON columns.
WITH pending_origin_tasks AS (
    SELECT DISTINCT origin_task_id AS task_id
    FROM system_design_versions
    WHERE workspace_id = $1
      AND NOT confirmed AND NOT dismissed AND origin_task_id IS NOT NULL
    UNION
    SELECT DISTINCT origin_task_id AS task_id
    FROM decisions
    WHERE workspace_id = $1
      AND status = 'proposed' AND origin_task_id IS NOT NULL
	UNION
	SELECT DISTINCT origin_task_id AS task_id
	FROM requirement_versions
	WHERE workspace_id = $1
	  AND origin = 'implementation' AND NOT confirmed AND NOT retired AND origin_task_id <> ''
	UNION
	SELECT DISTINCT proposal.task_id
	FROM task_context_proposals proposal
	JOIN tasks task ON task.workspace_id = proposal.workspace_id AND task.id = proposal.task_id
	WHERE proposal.workspace_id = $1 AND proposal.state = 'proposed'
	  AND task.state NOT IN ('merged', 'closed')
),
open_context_tasks AS (
	SELECT DISTINCT proposal.task_id
	FROM task_context_proposals proposal
	JOIN tasks task ON task.workspace_id = proposal.workspace_id AND task.id = proposal.task_id
	WHERE proposal.workspace_id = $1 AND proposal.state = 'proposed'
	  AND task.state NOT IN ('merged', 'closed')
),
superseded_reviews AS (
    SELECT DISTINCT jsonb_array_elements_text(
        COALESCE(e.payload_json #> '{review_transition,superseded_work_order_ids}', '[]'::jsonb)
    ) AS work_order_id
    FROM events e
    WHERE e.workspace_id = $1 AND e.kind = 'task.setup.changed'
),
current_reviews AS (
    SELECT w.id, w.task_id, w.state, w.review_round, w.review_seat,
           w.retry_suppressed, w.session_id, w.worker_id,
           w.last_attempt_outcome, w.last_failure_message,
           w.attempt_id, w.last_attempt_id
    FROM work_orders w
    WHERE w.workspace_id = $1 AND w.stage = 'review'
      AND NOT EXISTS (SELECT 1 FROM superseded_reviews s WHERE s.work_order_id = w.id)
),
latest_review_rounds AS (
    SELECT task_id, max(review_round) AS review_round
    FROM current_reviews
    GROUP BY task_id
),
latest_review_orders AS (
    SELECT w.*
    FROM current_reviews w
    JOIN latest_review_rounds latest USING (task_id, review_round)
),
completed_review_rounds AS (
    SELECT DISTINCT e.task_id, (e.payload_json ->> 'review_round')::integer AS review_round
    FROM events e
    WHERE e.workspace_id = $1
      AND e.kind = 'review.round_completed'
      AND COALESCE(e.payload_json ->> 'review_round', '') ~ '^[0-9]+$'
),
review_recovery_tasks AS (
    SELECT w.task_id
    FROM latest_review_orders w
    WHERE NOT EXISTS (
        SELECT 1 FROM completed_review_rounds completed
        WHERE completed.task_id = w.task_id AND completed.review_round = w.review_round
    )
    GROUP BY w.task_id, w.review_round
    HAVING bool_and(w.state NOT IN ('queued', 'claimed', 'submitted'))
       AND (
           bool_or(w.state = 'timed_out') OR
           bool_or(w.state = 'completed' AND (
               w.last_attempt_outcome IN ('child_failure', 'stalled') OR
               w.retry_suppressed OR w.last_failure_message <> ''
           ) AND (w.attempt_id = '' OR w.last_attempt_id = w.attempt_id))
       )
),
interrupted_review_tasks AS (
    SELECT w.task_id
    FROM latest_review_orders w
    GROUP BY w.task_id, w.review_round
    HAVING bool_and(w.state NOT IN ('claimed', 'submitted', 'timed_out', 'stale'))
       AND bool_or(w.state = 'queued' AND w.retry_suppressed AND w.session_id = '' AND w.worker_id = '')
),
stalled_tasks AS (
    SELECT DISTINCT w.task_id
    FROM work_orders w
    JOIN tasks t ON t.workspace_id = w.workspace_id AND t.id = w.task_id
    WHERE w.workspace_id = $1
      AND t.state NOT IN ('merged', 'closed')
      AND w.state IN ('queued', 'stale', 'timed_out')
      AND (
          w.retry_suppressed OR w.state = 'stale' OR
          (w.automatic_retry_count >= 2 AND btrim(w.last_failure_message) <> '') OR
          (w.stage = 'implement' AND EXISTS (
              SELECT 1
              FROM task_dependencies edge
              JOIN tasks dependency ON dependency.workspace_id = edge.workspace_id
                  AND dependency.id = edge.depends_on_task_id
              WHERE edge.workspace_id = w.workspace_id AND edge.task_id = w.task_id
                AND dependency.state <> 'merged'
          ))
      )
),
latest_issue_events AS (
    SELECT DISTINCT ON (e.task_id) e.task_id, e.kind
    FROM events e
    WHERE e.workspace_id = $1
      AND e.kind IN ('github_issue.publication_failed', 'github_issue.publication_published')
    ORDER BY e.task_id, e.at DESC, e.id DESC
),
latest_review_publication_events AS (
    SELECT DISTINCT ON (e.task_id, COALESCE(NULLIF(e.payload_json ->> 'review_work_order_id', ''), NULLIF(e.job_id, ''), e.id::text))
        e.task_id, e.kind
    FROM events e
    WHERE e.workspace_id = $1
      AND e.kind IN ('review.publication_failed', 'review.publication_published')
    ORDER BY e.task_id,
        COALESCE(NULLIF(e.payload_json ->> 'review_work_order_id', ''), NULLIF(e.job_id, ''), e.id::text), e.at DESC, e.id DESC
),
latest_merge_events AS (
    SELECT DISTINCT ON (e.task_id) e.task_id, e.kind
    FROM events e
    WHERE e.workspace_id = $1
      AND e.kind IN ('merge.failed', 'merge.confirmed', 'merge.reconciled')
    ORDER BY e.task_id, e.at DESC, e.id DESC
),
latest_conflict_events AS (
    SELECT DISTINCT ON (e.task_id) e.task_id, e.kind
    FROM events e
    WHERE e.workspace_id = $1
      AND e.kind IN ('merge.blocked', 'merge.conflict_cleared', 'merge.conflict_fix_dispatched',
                     'merge.conflict_recovery_blocked', 'merge.conflict_dispatch_exhausted')
    ORDER BY e.task_id, e.at DESC, e.id DESC
),
forge_failure_tasks AS (
    SELECT task_id FROM latest_issue_events WHERE kind = 'github_issue.publication_failed'
    UNION
    SELECT task_id FROM latest_review_publication_events WHERE kind = 'review.publication_failed'
    UNION
    SELECT task_id FROM latest_merge_events WHERE kind = 'merge.failed'
    UNION
    SELECT task_id FROM latest_conflict_events
      WHERE kind IN ('merge.conflict_recovery_blocked', 'merge.conflict_dispatch_exhausted')
),
pending_authority_tasks AS (
    SELECT DISTINCT w.task_id
    FROM work_orders w
    JOIN pending_origin_tasks origin ON origin.task_id = w.task_id
    WHERE w.workspace_id = $1
      AND (w.stage = 'implement' AND w.state = 'submitted' OR
           w.stage = 'review' AND w.state IN ('queued', 'claimed', 'submitted'))
),
latest_user_change_events AS (
    SELECT DISTINCT ON (e.task_id) e.task_id, e.kind
    FROM events e
    WHERE e.workspace_id = $1
      AND (
          (e.kind = 'pipeline.bounced' AND e.payload_json ->> 'source' = 'user-request-changes') OR
          (e.kind = 'work_order.claimed' AND e.payload_json ->> 'stage' = 'implement')
      )
    ORDER BY e.task_id, e.at DESC, e.id DESC
),
user_changes_requested_tasks AS (
    SELECT task_id FROM latest_user_change_events WHERE kind = 'pipeline.bounced'
),
attention_tasks AS (
    SELECT id AS task_id FROM tasks
      WHERE workspace_id = $1 AND state IN ('awaiting_human', 'parked')
    UNION SELECT task_id FROM forge_failure_tasks
    UNION SELECT task_id FROM review_recovery_tasks
    UNION SELECT task_id FROM interrupted_review_tasks
    UNION SELECT task_id FROM stalled_tasks
    UNION SELECT task_id FROM pending_authority_tasks
    UNION SELECT task_id FROM open_context_tasks
    UNION SELECT task_id FROM user_changes_requested_tasks
)
SELECT count(*)::bigint
FROM attention_tasks attention
JOIN tasks t ON t.workspace_id = $1 AND t.id = attention.task_id
WHERE NOT EXISTS (
    SELECT 1 FROM tasks child
    WHERE child.workspace_id = t.workspace_id AND child.parent_task_id = t.id
);
