-- component-persistence v13, req-task-branch-assignment AC-4.1 and AC-4.2.
-- SingleStore has no partial unique indexes. Go migrateOpenTaskBranchUnique
-- drops UNIQUE KEY tasks_branch_key under the startup lock because DDL
-- commits implicitly and must be restart-safe. CreateTask and AttachTaskBranch
-- enforce the open-task (workspace, repository, branch) predicate.
SELECT 1;
