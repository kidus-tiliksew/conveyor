-- REQ-9/AC-9.3; DEC-71. Submission-time exact-head evidence, not a task policy pin.
CREATE TABLE documentation_gate_evidence (
 workspace_id text NOT NULL,
 task_id text NOT NULL,
 head_sha text NOT NULL,
 payload_json jsonb NOT NULL,
 PRIMARY KEY (workspace_id, task_id, head_sha),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id) ON DELETE CASCADE
);
