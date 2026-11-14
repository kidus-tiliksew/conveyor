-- req-stage-preferences AC-1.1, component-stage-preferences v2, and
-- DEC-135(13). The set gains the risk stage: one entry or NULL like plan,
-- implement, and verify. A row saved before this migration keeps its other
-- stages and version and reads risk as unset. No dispatch, claim, submission,
-- rating, review, verification, or merge path reads this table (REQ-3;
-- DEC-131(3)).
ALTER TABLE user_stage_preferences ADD COLUMN IF NOT EXISTS risk_json jsonb;
