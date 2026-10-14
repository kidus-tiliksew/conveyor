-- REQ-9/AC-9.3 and DEC-71. Submission-time exact-head evidence, not a task policy pin.
CREATE ROWSTORE TABLE IF NOT EXISTS documentation_gate_evidence (
 workspace_id VARCHAR(191) NOT NULL,
 task_id VARCHAR(191) NOT NULL,
 head_sha VARCHAR(40) NOT NULL,
 payload_json JSON NOT NULL,
 PRIMARY KEY (workspace_id, task_id, head_sha),
 SHARD KEY (workspace_id, task_id)
);
