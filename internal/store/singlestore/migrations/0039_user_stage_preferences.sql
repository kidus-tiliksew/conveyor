-- req-stage-preferences REQ-1 and component-stage-preferences. One row per
-- person holds that person's stage preferences: the set's version, each
-- stage's JSON value or NULL when unset, and the update time. The set applies
-- in every workspace, so the row carries no workspace and shards by user.
-- SingleStore has no CHECK or foreign key, so the Go writer keeps the version
-- at 1 or later on every committed write. Version 0 is the transient row a
-- writer inserts to take the person's row lock. No dispatch, claim,
-- submission, review, verification, or merge path reads this table (REQ-3;
-- DEC-131(3)).
CREATE ROWSTORE TABLE IF NOT EXISTS user_stage_preferences (
 user_id VARCHAR(255) NOT NULL,
 version BIGINT NOT NULL,
 plan_json JSON NULL,
 implement_json JSON NULL,
 verify_json JSON NULL,
 review_json JSON NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY (user_id),
 SHARD KEY (user_id)
) COLLATE utf8mb4_bin;
