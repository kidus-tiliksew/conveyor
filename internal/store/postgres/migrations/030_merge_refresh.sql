-- Merge approvals are bound to the reviewed PR head, and refresh/conflict
-- orders carry immutable scope metadata (spec §21.30).
ALTER TABLE tasks
    ADD COLUMN reviewed_head_sha text NOT NULL DEFAULT '',
    ADD COLUMN approved_head_sha text NOT NULL DEFAULT '',
    ADD COLUMN approval_stale boolean NOT NULL DEFAULT false,
    ADD COLUMN refresh_baseline_sha text NOT NULL DEFAULT '',
    ADD COLUMN refresh_head_sha text NOT NULL DEFAULT '',
    ADD COLUMN refresh_review_scope text NOT NULL DEFAULT ''
        CHECK (refresh_review_scope IN ('', 'delta', 'full', 'none'));

ALTER TABLE work_orders
    ADD COLUMN reason_code text NOT NULL DEFAULT '',
    ADD COLUMN review_kind text NOT NULL DEFAULT '' CHECK (review_kind IN ('', 'refresh')),
    ADD COLUMN review_scope text NOT NULL DEFAULT '' CHECK (review_scope IN ('', 'delta', 'full')),
    ADD COLUMN baseline_sha text NOT NULL DEFAULT '',
    ADD COLUMN head_sha text NOT NULL DEFAULT '';
