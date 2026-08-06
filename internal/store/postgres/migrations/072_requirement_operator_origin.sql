-- Operator-authored REST proposals carry server-owned provenance without a
-- planning session or drift edge. Confirmation remains a separate mutation.
ALTER TABLE requirement_versions DROP CONSTRAINT requirement_versions_origin_check;
ALTER TABLE requirement_versions ADD CONSTRAINT requirement_versions_origin_check
  CHECK (origin IN ('chat', 'drift_amendment', 'feature_migration', 'operator'));

ALTER TABLE requirement_versions DROP CONSTRAINT requirement_versions_check1;
ALTER TABLE requirement_versions ADD CONSTRAINT requirement_versions_origin_provenance_check CHECK (
  (origin = 'chat' AND origin_session_id <> '' AND origin_drift_id = '')
  OR (origin = 'drift_amendment' AND origin_drift_id <> '' AND origin_session_id = '')
  OR (origin IN ('feature_migration', 'operator') AND origin_session_id = '' AND origin_drift_id = '')
);
