-- AC-5.1 (component-document-corpus): optional operator history, never content.
ALTER TABLE requirement_versions ADD COLUMN dismissal_note text;
ALTER TABLE system_design_versions ADD COLUMN dismissal_note text;
