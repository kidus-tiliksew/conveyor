-- Phase 8.3 task-level desired-state attachments (spec §21.58 change 3).
-- Events remain the append-only source of truth; these indexes keep the
-- task-local active fold and workspace rebuild scans bounded.
CREATE INDEX events_task_context_task_idx
  ON events (workspace_id, task_id, id)
  WHERE kind IN (
    'task.context_requirement_added', 'task.context_requirement_removed',
    'task.context_design_added', 'task.context_design_removed'
  );

CREATE INDEX events_task_context_reference_idx
  ON events (workspace_id, kind, (payload_json ->> 'id'), id)
  WHERE kind IN (
    'task.context_requirement_added', 'task.context_requirement_removed',
    'task.context_design_added', 'task.context_design_removed'
  );
