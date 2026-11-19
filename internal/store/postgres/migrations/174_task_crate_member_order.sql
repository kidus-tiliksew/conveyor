-- component-crates invariant 20; component-persistence invariants 35 and 36.
-- The work-order crate field reads the first members of one crate in
-- creation time and then ID order (ListCrateMembersLimited, DEC-105(4)). This
-- index gives that read an ordered path, so the engine can stop after limit
-- plus one rows. The partial index of migration 172 stays for the crate
-- filter and the member counts. All pending files apply in one transaction,
-- and IF NOT EXISTS makes a replay a no-op. No task row changes.
CREATE INDEX IF NOT EXISTS tasks_workspace_crate_created_idx ON tasks (workspace_id, crate_id, created_at, id)
    WHERE crate_id IS NOT NULL;
