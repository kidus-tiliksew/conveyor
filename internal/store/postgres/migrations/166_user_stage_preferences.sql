-- req-stage-preferences REQ-1 and component-stage-preferences. One row per
-- person holds that person's stage preferences: the set's version, each
-- stage's JSON value or NULL when unset, and the update time. The set applies
-- in every workspace, so the row carries no workspace. Version 0 is the
-- transient row a writer inserts to take the person's row lock; a committed
-- write leaves version 1 or later. No dispatch, claim, submission, review,
-- verification, or merge path reads this table (REQ-3; DEC-131(3)).
CREATE TABLE IF NOT EXISTS user_stage_preferences (
 user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 version bigint NOT NULL CHECK (version >= 0),
 plan_json jsonb,
 implement_json jsonb,
 verify_json jsonb,
 review_json jsonb,
 updated_at timestamptz NOT NULL
);
