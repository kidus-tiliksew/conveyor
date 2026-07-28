-- Keep the persisted intervention invariant aligned with the canonical action
-- set in internal/core/types.go. This forward-only repair leaves the
-- checksummed Phase 2 migration and existing valid rows untouched.
ALTER TABLE interventions
    DROP CONSTRAINT interventions_action_check,
    ADD CONSTRAINT interventions_action_check CHECK (
        action IN ({{intervention_actions}})
    );
