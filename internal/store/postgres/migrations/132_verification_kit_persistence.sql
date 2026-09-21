-- feature-verification-kit-execution VK-6; req-verification-kits REQ-5/6/7.
-- Existing artifacts retain their roles and unknown historical provenance.
ALTER TABLE artifact_links DROP CONSTRAINT artifact_links_role_check;
ALTER TABLE artifact_links ADD CONSTRAINT artifact_links_role_check CHECK (role IN ('task_context','generated_audit','generated_output','verification_evidence','typed_verification_evidence'));

CREATE TABLE verification_contexts (
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
CREATE INDEX verification_contexts_task ON verification_contexts(workspace_id,task_id,context_id);
CREATE TABLE verification_selections (
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
CREATE INDEX verification_selections_task ON verification_selections(workspace_id,task_id,context_id);
ALTER TABLE verification_selections ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE TABLE verification_obligations (
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
CREATE INDEX verification_obligations_task ON verification_obligations(workspace_id,task_id,context_id);
ALTER TABLE verification_obligations ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE TABLE verification_attempts (
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
CREATE INDEX verification_attempts_task ON verification_attempts(workspace_id,task_id,context_id);
ALTER TABLE verification_attempts ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE TABLE verification_operations (
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
CREATE INDEX verification_operations_task ON verification_operations(workspace_id,task_id,context_id);
ALTER TABLE verification_operations ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE TABLE verification_evidence (
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
CREATE INDEX verification_evidence_task ON verification_evidence(workspace_id,task_id,context_id);
ALTER TABLE verification_evidence ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE TABLE verification_evidence_links (
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
CREATE INDEX verification_evidence_links_task ON verification_evidence_links(workspace_id,task_id,context_id);
ALTER TABLE verification_evidence_links ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE TABLE verification_publications (
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
CREATE INDEX verification_publications_task ON verification_publications(workspace_id,task_id,context_id);
ALTER TABLE verification_publications ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE TABLE verification_upload_chunks (
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
CREATE INDEX verification_upload_chunks_task ON verification_upload_chunks(workspace_id,task_id,context_id);
ALTER TABLE verification_upload_chunks ADD FOREIGN KEY (workspace_id,context_id,task_id) REFERENCES verification_contexts(workspace_id,id,task_id);
CREATE INDEX verification_upload_chunks_expiry ON verification_upload_chunks(workspace_id,expires_at);
CREATE UNIQUE INDEX verification_operations_unresolved ON verification_operations(workspace_id,(body->>'SubjectKey'),(body->>'StepID'),(body->>'Target')) WHERE state NOT IN ('applied','not_applied','completed');


CREATE FUNCTION verification_record_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'verification records are retained'; END IF;
 IF NEW.workspace_id IS DISTINCT FROM OLD.workspace_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.context_id IS DISTINCT FROM OLD.context_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.logical_key IS DISTINCT FROM OLD.logical_key OR NEW.key_hash IS DISTINCT FROM OLD.key_hash THEN
  RAISE EXCEPTION 'verification provenance is immutable';
 END IF;
 IF TG_TABLE_NAME = 'verification_contexts' THEN
  IF OLD.state <> 'open' OR NEW.state <> 'sealed' OR NEW.body - 'SealedAt' <> OLD.body - 'SealedAt' THEN RAISE EXCEPTION 'invalid context transition'; END IF;
 ELSIF TG_TABLE_NAME = 'verification_attempts' THEN
  IF NEW.body - 'Recovery' = OLD.body - 'Recovery' AND jsonb_array_length(NULLIF(NEW.body->'Recovery','null'::jsonb)) = COALESCE(jsonb_array_length(NULLIF(OLD.body->'Recovery','null'::jsonb)),0)+1 AND (NEW.body->'Recovery') - (jsonb_array_length(NULLIF(NEW.body->'Recovery','null'::jsonb))-1) = COALESCE(NULLIF(OLD.body->'Recovery','null'::jsonb),'[]'::jsonb) THEN
   RETURN NEW;
  END IF;
  IF OLD.state NOT IN ('pending','running') OR NEW.state NOT IN ('succeeded','failed','timed_out','cancelled','blocked','waiting') OR NEW.body - ARRAY['State','Explanation','EndedAt','ExitCode'] <> OLD.body - ARRAY['State','Explanation','EndedAt','ExitCode'] THEN RAISE EXCEPTION 'invalid attempt transition'; END IF;
 ELSIF TG_TABLE_NAME = 'verification_operations' THEN
  IF NEW.body - 'History' <> OLD.body - 'History' OR jsonb_array_length(NEW.body->'History') <> jsonb_array_length(OLD.body->'History')+1 OR (NEW.body->'History') - (jsonb_array_length(NEW.body->'History')-1) <> OLD.body->'History' THEN RAISE EXCEPTION 'operation history is append only'; END IF;
 ELSE RAISE EXCEPTION 'verification record is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER verification_contexts_guard BEFORE UPDATE OR DELETE ON verification_contexts FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
CREATE TRIGGER verification_selections_guard BEFORE UPDATE OR DELETE ON verification_selections FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
CREATE TRIGGER verification_obligations_guard BEFORE UPDATE OR DELETE ON verification_obligations FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
CREATE TRIGGER verification_attempts_guard BEFORE UPDATE OR DELETE ON verification_attempts FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
CREATE TRIGGER verification_operations_guard BEFORE UPDATE OR DELETE ON verification_operations FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
CREATE TRIGGER verification_evidence_guard BEFORE UPDATE OR DELETE ON verification_evidence FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
CREATE TRIGGER verification_evidence_links_guard BEFORE UPDATE OR DELETE ON verification_evidence_links FOR EACH ROW EXECUTE FUNCTION verification_record_guard();
CREATE TRIGGER verification_publications_guard BEFORE UPDATE OR DELETE ON verification_publications FOR EACH ROW EXECUTE FUNCTION verification_record_guard();

ALTER TABLE verification_contexts ADD COLUMN work_order_id TEXT GENERATED ALWAYS AS (body->>'WorkOrderID') STORED;
ALTER TABLE verification_contexts ADD FOREIGN KEY (workspace_id,work_order_id) REFERENCES work_orders(workspace_id,id);
ALTER TABLE verification_evidence_links ADD COLUMN source_evidence_id TEXT GENERATED ALWAYS AS (body->>'From') STORED;
ALTER TABLE verification_evidence_links ADD COLUMN target_evidence_id TEXT GENERATED ALWAYS AS (body->>'To') STORED;
ALTER TABLE verification_evidence_links ADD FOREIGN KEY (workspace_id,source_evidence_id,task_id) REFERENCES verification_evidence(workspace_id,id,task_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE verification_evidence_links ADD FOREIGN KEY (workspace_id,target_evidence_id,task_id) REFERENCES verification_evidence(workspace_id,id,task_id) DEFERRABLE INITIALLY DEFERRED;
