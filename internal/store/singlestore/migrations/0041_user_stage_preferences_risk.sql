-- req-stage-preferences AC-1.1, component-stage-preferences v2, and
-- DEC-135(13). migrate.go adds the nullable user_stage_preferences.risk_json
-- column before this file runs. SingleStore DDL commits implicitly and has no
-- ADD COLUMN IF NOT EXISTS, so the add lives in Go behind an
-- information_schema check. There is no backfill: a row saved before this
-- migration keeps its other stages and version and reads risk as unset.
SELECT 1;
