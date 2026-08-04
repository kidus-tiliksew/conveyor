-- Migration 061's four INSERT ... SELECT blocks could not observe reference-
-- document events: the preceding scope constraint made those task-less events
-- impossible before 061, while the broken runtime writer rolled back after it.
-- Remove any projector-owned development rows encoded with that obsolete
-- direction. Durable events remain authoritative and RebuildLineage recreates
-- the corrected session -> document-version and requirement-version ->
-- document-version edges with their original event provenance.
DELETE FROM links
WHERE created_by_event_id IS NOT NULL
  AND kind IN ('consulted', 'derived_from');

ALTER TABLE reference_document_versions
  DROP CONSTRAINT reference_document_versions_workspace_id_document_id_fkey;
ALTER TABLE reference_document_versions
  ADD CONSTRAINT reference_document_versions_workspace_id_document_id_fkey
  FOREIGN KEY (workspace_id, document_id)
  REFERENCES reference_documents(workspace_id, id)
  ON DELETE CASCADE;
