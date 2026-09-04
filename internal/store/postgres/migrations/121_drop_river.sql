-- Migration 121 retires River. The durable queue lives on the event log
-- (internal/queue/logqueue); the migration runner moves every active River
-- job onto its job stream before this file runs, so nothing queued is lost.
DROP TABLE IF EXISTS river_job CASCADE;
DROP TABLE IF EXISTS river_leader CASCADE;
DROP TABLE IF EXISTS river_queue CASCADE;
DROP TABLE IF EXISTS river_client_queue CASCADE;
DROP TABLE IF EXISTS river_client CASCADE;
DROP TABLE IF EXISTS river_migration CASCADE;
