-- feature-review-seat-minimum RS-2 under DEC-77 and DEC-78. The nullable columns
-- work_orders.declared_review_seats and task_specs.review_seats are added by
-- migrate.go before this file, behind information_schema checks, because
-- SingleStore DDL commits implicitly. No backfill.
CREATE ROWSTORE TABLE IF NOT EXISTS review_round_seats (
 workspace_id VARCHAR(255) NOT NULL,
 task_id VARCHAR(255) NOT NULL,
 review_round INT NOT NULL,
 seat_count INT NOT NULL,
 seat_count_source VARCHAR(16) NOT NULL,
 minimum_seat_count INT NOT NULL,
 implementation_work_order_id VARCHAR(255) NOT NULL DEFAULT '',
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, task_id, review_round),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
CREATE ROWSTORE TABLE IF NOT EXISTS review_seat_additions (
 workspace_id VARCHAR(255) NOT NULL,
 request_id VARCHAR(255) NOT NULL,
 task_id VARCHAR(255) NOT NULL,
 review_round INT NOT NULL,
 prior_seat_count INT NOT NULL,
 seat_count INT NOT NULL,
 reason LONGTEXT NOT NULL,
 actor_id LONGTEXT NOT NULL,
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, request_id),
 SHARD KEY (workspace_id)
) COLLATE utf8mb4_bin;
