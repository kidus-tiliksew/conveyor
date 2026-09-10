-- AC-5.1 (component-document-corpus): migrateDocumentDismissalNotes adds
-- nullable dismissal_note TEXT columns to requirement_versions and
-- system_design_versions under the startup lock, with schema checks because
-- SingleStore does not support ADD COLUMN IF NOT EXISTS.
SELECT 1;
