CREATE TABLE reference_documents (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id text NOT NULL,
  name text NOT NULL,
  current_version integer NOT NULL CHECK (current_version > 0),
  deleted_at timestamptz,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (workspace_id, id)
);
CREATE UNIQUE INDEX reference_documents_live_name_idx
  ON reference_documents (workspace_id, lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE reference_document_versions (
  workspace_id text NOT NULL,
  document_id text NOT NULL,
  version integer NOT NULL CHECK (version > 0),
  filename text NOT NULL,
  content_type text NOT NULL,
  content text NOT NULL,
  supersedes_version integer,
  created_by text NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (workspace_id, document_id, version),
  FOREIGN KEY (workspace_id, document_id) REFERENCES reference_documents(workspace_id, id)
);

ALTER TABLE requirement_versions ADD COLUMN derived_from jsonb;

ALTER TABLE events DROP CONSTRAINT events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN (
    'config.updated','workspace.created','worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt'
  ) AND job_id IS NULL)
);

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'reference_document_version',(event.payload_json->>'document_id')||':v'||(event.payload_json->>'version'),
       'planning_session',event.payload_json->>'session_id','consulted',event.id,event.at
FROM events event WHERE event.kind='reference_document.consulted'
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'reference_document',event.payload_json->>'document_id',
       'reference_document_version',(event.payload_json->>'document_id')||':v'||(event.payload_json->>'version'),'versions',event.id,event.at
FROM events event WHERE event.kind IN ('reference_document.created','reference_document.superseded')
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'reference_document_version',(event.payload_json->>'document_id')||':v'||(event.payload_json->>'version'),
       'reference_document_version',(event.payload_json->>'document_id')||':v'||(event.payload_json->>'supersedes_version'),'supersedes',event.id,event.at
FROM events event WHERE event.kind='reference_document.superseded'
ON CONFLICT DO NOTHING;

INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
SELECT event.workspace_id,'reference_document_version',(event.payload_json->>'derived_document_id')||':v'||(event.payload_json->>'derived_document_version'),
       'requirement_version',(event.payload_json->>'requirement_id')||':v'||(event.payload_json->>'version'),'derived_from',event.id,event.at
FROM events event WHERE event.kind='requirement.version_confirmed' AND event.payload_json ? 'derived_document_id'
ON CONFLICT DO NOTHING;
