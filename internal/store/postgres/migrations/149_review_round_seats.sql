-- feature-review-seat-minimum RS-2; DEC-77, DEC-78; req-review-gates-evidence
-- REQ-2 (AC-2.4, AC-2.6, AC-2.7). Each review round records its seat count and
-- where that count came from. No backfill: a round with orders and no record is
-- a legacy round whose count is its non-superseded seat orders.
CREATE TABLE IF NOT EXISTS review_round_seats (
 workspace_id text NOT NULL,
 task_id text NOT NULL,
 review_round integer NOT NULL CHECK (review_round >= 1),
 seat_count integer NOT NULL CHECK (seat_count >= 1),
 seat_count_source text NOT NULL CHECK (seat_count_source IN ('declared', 'planned', 'carried', 'minimum')),
 minimum_seat_count integer NOT NULL CHECK (minimum_seat_count >= 1),
 implementation_work_order_id text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL,
 PRIMARY KEY (workspace_id, task_id, review_round),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);

-- The RS-5 operator seat-addition ledger. It stays empty until RS-5 lands.
CREATE TABLE IF NOT EXISTS review_seat_additions (
 workspace_id text NOT NULL,
 request_id text NOT NULL,
 task_id text NOT NULL,
 review_round integer NOT NULL CHECK (review_round >= 1),
 prior_seat_count integer NOT NULL CHECK (prior_seat_count >= 0),
 seat_count integer NOT NULL CHECK (seat_count > prior_seat_count),
 reason text NOT NULL,
 actor_id text NOT NULL,
 created_at timestamptz NOT NULL,
 PRIMARY KEY (workspace_id, request_id),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);

-- Nullable and never backfilled. RS-4 writes the declared count on implement
-- orders; RS-3 writes the planned count on plan versions.
ALTER TABLE work_orders ADD COLUMN IF NOT EXISTS declared_review_seats integer
 CHECK (declared_review_seats IS NULL OR declared_review_seats >= 1);
ALTER TABLE task_specs ADD COLUMN IF NOT EXISTS review_seats integer
 CHECK (review_seats IS NULL OR review_seats >= 1);
