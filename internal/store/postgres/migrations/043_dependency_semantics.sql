-- Dependency outcomes, operator unlink receipts, and suspended queue clocks
-- (spec §§6.3, 13.2, 16; §21.47).
ALTER TABLE work_orders
    ADD COLUMN queue_blocked_at timestamptz;

UPDATE work_orders wo
SET queue_blocked_at = queue_entered_at
WHERE wo.stage = 'implement'
  AND wo.state = 'queued'
  AND EXISTS (
      SELECT 1
      FROM task_dependencies edge
      JOIN tasks dependency
        ON dependency.workspace_id = edge.workspace_id
       AND dependency.id = edge.depends_on_task_id
      WHERE edge.workspace_id = wo.workspace_id
        AND edge.task_id = wo.task_id
        AND dependency.state <> 'merged'
  );

CREATE INDEX work_orders_blocked_queue_idx
    ON work_orders (workspace_id, task_id, queue_blocked_at)
    WHERE queue_blocked_at IS NOT NULL;

CREATE TABLE task_dependency_removals (
    workspace_id       text NOT NULL REFERENCES workspaces(id),
    request_id         text NOT NULL,
    task_id            text NOT NULL,
    depends_on_task_id text NOT NULL,
    reason             text NOT NULL,
    actor_id           text NOT NULL,
    actor_role         text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, request_id),
    FOREIGN KEY (workspace_id, task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, depends_on_task_id)
        REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX events_dependency_unsatisfiable_edge_outcome_idx
    ON events (
        task_id,
        (payload_json ->> 'depends_on_task_id'),
        (payload_json ->> 'dependency_state')
    )
    WHERE kind = 'task.dependency_unsatisfiable';
