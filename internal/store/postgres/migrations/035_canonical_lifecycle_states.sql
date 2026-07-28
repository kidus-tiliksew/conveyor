-- Canonical state sets from internal/core/lifecycle.go (spec §21.37).
-- Historical edge auditing is performed by store.AuditLifecycleHistory before
-- this migration is enabled; this migration never rewrites event history.
ALTER TABLE tasks
    ADD CONSTRAINT tasks_state_check CHECK (
        state IN ({{task_states}})
    );

ALTER TABLE work_orders
    DROP CONSTRAINT work_orders_state_check,
    ADD CONSTRAINT work_orders_state_check CHECK (
        state IN ({{work_order_states}})
    );
