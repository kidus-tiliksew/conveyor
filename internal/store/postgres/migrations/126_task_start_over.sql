-- req-task-lifecycle-and-queue REQ-7: restart projections and retry identity.
ALTER TABLE tasks ADD COLUMN supersedes text REFERENCES tasks(id);
ALTER TABLE tasks ADD COLUMN superseded_by text REFERENCES tasks(id);
ALTER TABLE tasks ADD COLUMN intake_operator_direction text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX tasks_supersedes_unique ON tasks(workspace_id, supersedes) WHERE supersedes IS NOT NULL;
CREATE TABLE task_start_overs (
 workspace_id text NOT NULL REFERENCES workspaces(id),
 task_id text NOT NULL REFERENCES tasks(id),
 request_id text NOT NULL,
 successor_id text NOT NULL REFERENCES tasks(id),
 reason text NOT NULL,
 note text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (workspace_id, task_id, request_id),
 UNIQUE (workspace_id, task_id),
 UNIQUE (workspace_id, successor_id)
);
