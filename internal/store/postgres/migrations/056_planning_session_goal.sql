-- Migration 056: planning sessions declare an immutable goal artifact
-- (spec §9, §21.57 change 3). The goal steers the agent's finalize target and
-- is enforced at finalize time for the non-open goals.
--
-- Existing rows predate the declaration, so they take `open` — which is
-- precisely their historical behavior: either finalizer is legal.

ALTER TABLE planning_sessions
    ADD COLUMN goal text NOT NULL DEFAULT 'open'
        CHECK (goal IN ('requirement', 'blueprint', 'open'));
