-- feature-verification-kit-execution VK-9, DEC-43. The source ledger is unchanged.
CREATE TABLE verification_publication_deliveries (
 workspace_id TEXT NOT NULL,
 id TEXT NOT NULL,
 task_id TEXT NOT NULL,
 context_id TEXT NOT NULL,
 source_publication_id TEXT NOT NULL,
 pr_key TEXT NOT NULL,
 generation BIGINT NOT NULL CHECK (generation > 0),
 state TEXT NOT NULL CHECK (state IN ('pending','retrying','published','failed','superseded')),
 body JSONB NOT NULL,
 next_attempt_at TIMESTAMPTZ,
 PRIMARY KEY (workspace_id,id),
 UNIQUE (workspace_id,pr_key,generation),
 UNIQUE (workspace_id,source_publication_id,pr_key),
 FOREIGN KEY (workspace_id,source_publication_id) REFERENCES verification_publications(workspace_id,id),
 FOREIGN KEY (workspace_id,task_id) REFERENCES tasks(workspace_id,id),
 CHECK (octet_length(body::text) <= 32768)
);
CREATE INDEX verification_delivery_source ON verification_publication_deliveries(workspace_id,task_id,context_id,source_publication_id);
CREATE INDEX verification_delivery_pending ON verification_publication_deliveries(workspace_id,state,next_attempt_at,pr_key,generation);
