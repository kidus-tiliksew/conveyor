-- req-verification-kits REQ-4/REQ-7; feature-verification-kit-execution VK-6/VK-7 (DEC-43).
-- VK-7: sealing binds the completed verify order to its retained result.
ALTER TABLE work_orders ADD COLUMN verification_context_id TEXT NOT NULL DEFAULT '';
CREATE INDEX verification_contexts_sealing ON verification_contexts(workspace_id,task_id,state);
CREATE INDEX verification_operations_resolution ON verification_operations(workspace_id,task_id,state);
