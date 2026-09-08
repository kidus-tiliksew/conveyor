-- req-repository-onboarding AC-3.1, AC-3.4; component-persistence.
-- Existing repositories remain off. Registration writes supply the new default.
ALTER TABLE repos ADD COLUMN install_conveyor boolean NOT NULL DEFAULT false;
CREATE TABLE repository_install_tasks (
 workspace_id text NOT NULL,
 repository_name text NOT NULL,
 attempt integer NOT NULL CHECK (attempt > 0),
 task_id text NOT NULL REFERENCES tasks(id),
 PRIMARY KEY (workspace_id, repository_name, attempt),
 UNIQUE (workspace_id, task_id),
 FOREIGN KEY (workspace_id, repository_name) REFERENCES repos(workspace_id,name)
);
