-- VK-4; req-verification-kits REQ-7/AC-7.3. Immutable grants and append-only revocations.
CREATE TABLE verification_permission_grants (
 workspace_id TEXT NOT NULL REFERENCES workspaces(id),
 id TEXT NOT NULL,
 task_id TEXT NOT NULL,
 context_id TEXT NOT NULL,
 run_id TEXT NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT '',
 body JSONB NOT NULL,
 expires_at TIMESTAMPTZ,
 PRIMARY KEY (workspace_id,id),
 UNIQUE (workspace_id,key_hash),
 UNIQUE (workspace_id,id,task_id),
 FOREIGN KEY (workspace_id,task_id) REFERENCES tasks(workspace_id,id)
);
CREATE INDEX verification_permission_grants_task ON verification_permission_grants(workspace_id,task_id,context_id);
ALTER TABLE verification_permission_grants ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE TABLE verification_permission_revocations (
 workspace_id TEXT NOT NULL REFERENCES workspaces(id),
 id TEXT NOT NULL,
 task_id TEXT NOT NULL,
 context_id TEXT NOT NULL,
 run_id TEXT NOT NULL DEFAULT '',
 logical_key TEXT NOT NULL,
 key_hash TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT '',
 body JSONB NOT NULL,
 expires_at TIMESTAMPTZ,
 PRIMARY KEY (workspace_id,id),
 UNIQUE (workspace_id,key_hash),
 UNIQUE (workspace_id,id,task_id),
 FOREIGN KEY (workspace_id,task_id) REFERENCES tasks(workspace_id,id)
);
CREATE INDEX verification_permission_revocations_task ON verification_permission_revocations(workspace_id,task_id,context_id);
ALTER TABLE verification_permission_revocations ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);

CREATE TRIGGER verification_permission_grants_guard BEFORE UPDATE OR DELETE ON verification_permission_grants FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
CREATE TRIGGER verification_permission_revocations_guard BEFORE UPDATE OR DELETE ON verification_permission_revocations FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
