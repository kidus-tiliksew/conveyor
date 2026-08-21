DELETE FROM task_context_proposals proposal
USING tasks task
WHERE proposal.workspace_id = task.workspace_id
  AND proposal.task_id = task.id
  AND proposal.state = 'proposed'
  AND task.state IN ('merged', 'closed');
