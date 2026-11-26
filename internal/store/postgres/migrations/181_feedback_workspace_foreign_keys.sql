-- component-persistence invariants 21 and 34 to 37, component-production-feedback
-- invariant 1, and req-production-feedback AC-11.1. PostgreSQL keys every
-- workspace table to workspaces. Migration 179 created three feedback tables
-- whose workspace_id had no key, so this file adds one to each. SingleStore
-- has no foreign keys by design (invariant 35), so it has no matching file.
--
-- A row whose workspace does not exist is a stored GitHub answer or an
-- admission that no read can reach, and it is no ledger row. Each delete
-- removes only such rows, and every delete runs before any key is added. The
-- keys are validated, so a row that a concurrent writer adds for a missing
-- workspace fails this migration and the whole upgrade rolls back
-- (invariant 36). No column, index, check, or other table changes.
DELETE FROM feedback_comparisons c
    WHERE NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = c.workspace_id);
DELETE FROM feedback_admissions a
    WHERE NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = a.workspace_id);
DELETE FROM feedback_branch_heads h
    WHERE NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = h.workspace_id);

ALTER TABLE feedback_comparisons DROP CONSTRAINT IF EXISTS feedback_comparisons_workspace_id_fkey;
ALTER TABLE feedback_comparisons ADD CONSTRAINT feedback_comparisons_workspace_id_fkey
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE feedback_admissions DROP CONSTRAINT IF EXISTS feedback_admissions_workspace_id_fkey;
ALTER TABLE feedback_admissions ADD CONSTRAINT feedback_admissions_workspace_id_fkey
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE feedback_branch_heads DROP CONSTRAINT IF EXISTS feedback_branch_heads_workspace_id_fkey;
ALTER TABLE feedback_branch_heads ADD CONSTRAINT feedback_branch_heads_workspace_id_fkey
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;
