-- component-persistence v13; req-task-branch-assignment AC-4.1, AC-4.2.
-- Assigned branches are unique among open tasks in one workspace repository.
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_branch_key;
CREATE UNIQUE INDEX IF NOT EXISTS tasks_open_repo_branch_idx
  ON tasks (workspace_id, repo_name, branch)
  WHERE state NOT IN ('merged', 'closed');
