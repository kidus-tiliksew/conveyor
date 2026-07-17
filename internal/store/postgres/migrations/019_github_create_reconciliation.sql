-- A create attempt becomes reconciliation-only before Conveyor crosses the
-- GitHub side-effect boundary. Kept as a follow-up migration so databases that
-- already exercised Phase 5.3 during review are upgraded safely.
ALTER TABLE github_lifecycles
    ADD COLUMN create_state text NOT NULL DEFAULT 'not_started'
    CHECK (create_state IN ('not_started', 'reconciling', 'confirmed'));
