-- Bound ambiguous issue creation without permanently stranding a confirmed
-- no-create failure. A new create attempt is authorized only after the worker
-- durably records the required exhaustive no-marker reconciliation passes.
ALTER TABLE github_lifecycles
    ADD COLUMN create_attempts integer NOT NULL DEFAULT 0
        CHECK (create_attempts >= 0),
    ADD COLUMN reconcile_misses integer NOT NULL DEFAULT 0
        CHECK (reconcile_misses >= 0);
