-- Phase 8.3 retirement flip: pin the exact unapproved legacy spec versions
-- that were sitting at the plan/spec approval gate when this migration ran.
-- New versions are never inserted here. The compatibility set is therefore
-- finite, auditable, and cannot become a second creation path.
CREATE TABLE legacy_spec_gate_versions (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  task_id text NOT NULL,
  spec_version integer NOT NULL,
  captured_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, task_id, spec_version),
  FOREIGN KEY (task_id, spec_version) REFERENCES task_specs(task_id, version)
);

INSERT INTO legacy_spec_gate_versions (workspace_id, task_id, spec_version)
SELECT t.workspace_id, t.id, s.version
FROM tasks t
JOIN task_specs s ON s.task_id = t.id
WHERE t.state = 'awaiting_human'
  AND NOT s.approved
  AND s.version = (SELECT MAX(latest.version) FROM task_specs latest WHERE latest.task_id = t.id)
  AND (
    SELECT j.stage
    FROM jobs j
    WHERE j.task_id = t.id
    ORDER BY j.started_at DESC, j.id DESC
    LIMIT 1
  ) = 'spec';
