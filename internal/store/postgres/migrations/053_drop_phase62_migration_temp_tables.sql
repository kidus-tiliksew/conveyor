-- Migration 050 used temporary staging tables while repairing invalid monitor
-- requirement references. Explicitly remove them for sessions where the
-- migration runner keeps one connection alive across multiple migrations.
DROP TABLE IF EXISTS migration_050_invalid_observation_requirements;
DROP TABLE IF EXISTS migration_050_invalid_drift_requirements;
