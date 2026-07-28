-- Keep the persisted intervention invariant aligned with the canonical action
-- set accepted when this immutable migration ships. Future action additions
-- require another forward migration; never template an applied constraint.
ALTER TABLE interventions
    DROP CONSTRAINT interventions_action_check,
    ADD CONSTRAINT interventions_action_check CHECK (
        action IN ('approve', 'reject', 'redirect', 'pull_to_local', 'cancel')
    );
