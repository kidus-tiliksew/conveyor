-- Projection fields distinguish automatic dismissal archives from operator archives.
-- migrate.go repairs historical documents and appends events in this transaction.
ALTER TABLE requirements ADD COLUMN archive_reason text NOT NULL DEFAULT '', ADD COLUMN archive_note text NOT NULL DEFAULT '';
ALTER TABLE system_designs ADD COLUMN archive_reason text NOT NULL DEFAULT '', ADD COLUMN archive_note text NOT NULL DEFAULT '';
