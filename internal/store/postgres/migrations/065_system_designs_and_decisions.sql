-- Phase 8.2 desired-state mechanism documents and decisions (spec §21.58).
CREATE TABLE system_designs (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id text NOT NULL,
  slug text NOT NULL,
  title text NOT NULL,
  category text NOT NULL,
  current_version integer,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (workspace_id,id),
  UNIQUE (workspace_id,slug),
  CHECK (current_version IS NULL OR current_version > 0)
);

CREATE TABLE system_design_versions (
  workspace_id text NOT NULL,
  document_id text NOT NULL,
  version integer NOT NULL CHECK (version > 0),
  content text NOT NULL,
  governs jsonb NOT NULL CHECK (jsonb_typeof(governs)='array'),
  origin text NOT NULL CHECK (origin IN ('planning_session','implementation_deliberation','operator')),
  origin_session_id text,
  origin_task_id text,
  confirmed boolean NOT NULL DEFAULT false,
  confirmed_by text,
  confirmed_at timestamptz,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (workspace_id,document_id,version),
  FOREIGN KEY (workspace_id,document_id) REFERENCES system_designs(workspace_id,id),
  CHECK ((origin='planning_session' AND origin_session_id IS NOT NULL AND origin_task_id IS NULL)
      OR (origin='implementation_deliberation' AND origin_task_id IS NOT NULL AND origin_session_id IS NULL)
      OR (origin='operator' AND origin_session_id IS NULL AND origin_task_id IS NULL)),
  CHECK ((confirmed AND confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)
      OR (NOT confirmed AND confirmed_by IS NULL AND confirmed_at IS NULL))
);

CREATE TABLE decision_sequences (
  workspace_id text PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  high_water_mark integer NOT NULL DEFAULT 0 CHECK (high_water_mark >= 0)
);

CREATE TABLE decisions (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id text NOT NULL CHECK (id ~ '^DEC-[1-9][0-9]*$'),
  statement text NOT NULL,
  context text NOT NULL,
  alternatives_rejected text NOT NULL,
  status text NOT NULL CHECK (status IN ('proposed','confirmed','superseded')),
  origin text NOT NULL CHECK (origin IN ('planning_session','implementation_deliberation','operator')),
  origin_session_id text,
  origin_task_id text,
  supersedes text,
  confirmed_by text,
  confirmed_at timestamptz,
  superseded_by text,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (workspace_id,id),
  FOREIGN KEY (workspace_id,supersedes) REFERENCES decisions(workspace_id,id),
  FOREIGN KEY (workspace_id,superseded_by) REFERENCES decisions(workspace_id,id),
  UNIQUE (workspace_id,supersedes),
  CHECK ((origin='planning_session' AND origin_session_id IS NOT NULL)
      OR (origin='implementation_deliberation' AND origin_task_id IS NOT NULL)
      OR origin='operator')
);

ALTER TABLE planning_sessions DROP CONSTRAINT planning_sessions_goal_check;
ALTER TABLE planning_sessions ADD CONSTRAINT planning_sessions_goal_check
  CHECK (goal IN ('requirement','system_design','blueprint','open'));
ALTER TABLE planning_sessions ADD COLUMN system_design_context_id text;
ALTER TABLE planning_sessions ADD COLUMN produced_system_design_id text;
ALTER TABLE planning_sessions ADD CONSTRAINT planning_sessions_system_design_context_fk
  FOREIGN KEY (workspace_id,system_design_context_id) REFERENCES system_designs(workspace_id,id);
ALTER TABLE planning_sessions ADD CONSTRAINT planning_sessions_produced_system_design_fk
  FOREIGN KEY (workspace_id,produced_system_design_id) REFERENCES system_designs(workspace_id,id);

ALTER TABLE repository_drift ADD COLUMN system_design_id text;
ALTER TABLE repository_drift ADD COLUMN system_design_version integer;
ALTER TABLE repository_drift ADD COLUMN causal_event_id bigint;
ALTER TABLE repository_drift ADD COLUMN matching_paths jsonb NOT NULL DEFAULT '[]'::jsonb
  CHECK (jsonb_typeof(matching_paths)='array');
ALTER TABLE repository_drift ADD CONSTRAINT repository_drift_system_design_fk
  FOREIGN KEY (workspace_id,system_design_id) REFERENCES system_designs(workspace_id,id);

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'decision.proposed','decision.confirmed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);
