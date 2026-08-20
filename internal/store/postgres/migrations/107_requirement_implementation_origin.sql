-- Task-authored requirement proposals carry the implementation task that
-- produced them. They remain pending until an operator confirms or dismisses
-- them through the existing requirement-version surfaces.
ALTER TABLE requirement_versions
  ADD COLUMN origin_task_id text NOT NULL DEFAULT '';

ALTER TABLE requirement_versions DROP CONSTRAINT requirement_versions_origin_check;
ALTER TABLE requirement_versions ADD CONSTRAINT requirement_versions_origin_check
  CHECK (origin IN ('chat', 'drift_amendment', 'feature_migration', 'operator', 'implementation'));

ALTER TABLE requirement_versions DROP CONSTRAINT requirement_versions_origin_provenance_check;
ALTER TABLE requirement_versions ADD CONSTRAINT requirement_versions_origin_provenance_check CHECK (
  (origin = 'chat' AND origin_session_id <> '' AND origin_task_id = '' AND origin_drift_id = '')
  OR (origin = 'drift_amendment' AND origin_drift_id <> '' AND origin_session_id = '' AND origin_task_id = '')
  OR (origin IN ('feature_migration', 'operator') AND origin_session_id = '' AND origin_task_id = '' AND origin_drift_id = '')
  OR (origin = 'implementation' AND origin_task_id <> '' AND origin_session_id = '' AND origin_drift_id = '')
);

CREATE INDEX requirement_versions_pending_task_idx
  ON requirement_versions (workspace_id, origin_task_id, requirement_id, version)
  WHERE origin = 'implementation' AND NOT confirmed AND NOT retired;
