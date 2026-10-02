-- Documentation-closure policy pins and per-head gate evidence (component-task-lifecycle, component-persistence).
-- The policy is pinned once per task at its first implement claim, and the evidence
-- is recorded per accepted head at submit_for_review.
CREATE TABLE task_documentation_policies (
 workspace_id text NOT NULL,
 task_id text NOT NULL,
 payload_json jsonb NOT NULL,
 PRIMARY KEY (workspace_id, task_id),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id)
);
CREATE TABLE documentation_gate_evidence (
 workspace_id text NOT NULL,
 task_id text NOT NULL,
 head_sha text NOT NULL,
 payload_json jsonb NOT NULL,
 PRIMARY KEY (workspace_id, task_id, head_sha),
 FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id, id)
);
