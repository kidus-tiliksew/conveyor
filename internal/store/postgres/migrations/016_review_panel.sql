-- Phase 5.2 snapshots each adversarial review seat on its durable work order
-- and carries honest model-enforcement provenance into publication records
-- (spec §21.12 change 4).
ALTER TABLE work_orders
    ADD COLUMN review_round integer NOT NULL DEFAULT 0 CHECK (review_round >= 0),
    ADD COLUMN review_seat integer NOT NULL DEFAULT 0 CHECK (review_seat >= 0),
    ADD COLUMN required_model text NOT NULL DEFAULT '',
    ADD COLUMN required_harness text NOT NULL DEFAULT '',
    ADD COLUMN model_enforcement text NOT NULL DEFAULT ''
        CHECK (model_enforcement IN ('', 'worker-pinned', 'self-reported'));

CREATE UNIQUE INDEX work_orders_review_seat_idx
    ON work_orders (workspace_id, task_id, review_round, review_seat)
    WHERE stage = 'review' AND review_round > 0;

ALTER TABLE review_publications
    ADD COLUMN review_round integer NOT NULL DEFAULT 0 CHECK (review_round >= 0),
    ADD COLUMN review_seat integer NOT NULL DEFAULT 0 CHECK (review_seat >= 0),
    ADD COLUMN required_model text NOT NULL DEFAULT '',
    ADD COLUMN required_harness text NOT NULL DEFAULT '',
    ADD COLUMN model_enforcement text NOT NULL DEFAULT ''
        CHECK (model_enforcement IN ('', 'worker-pinned', 'self-reported'));
