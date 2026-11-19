-- component-crates invariant 20 and component-persistence invariants 35 and 36.
-- The Go step after this file adds the ordered rowstore key
-- tasks_workspace_crate_created_idx (workspace_id, crate_id, created_at, id)
-- for the bounded work-order crate member read. SingleStore has no
-- CREATE INDEX IF NOT EXISTS and commits DDL implicitly, so the step checks
-- information_schema under the startup migration lock, accepts an existing
-- key only with exactly those ordered columns, and records this version only
-- after the key is in place. No task row changes.
SELECT 1;
