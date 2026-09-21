-- req-verification-kits REQ-4/REQ-7; feature-verification-kit-execution VK-6/VK-7 (DEC-43).
-- VK-7: only the atomic sealing transaction sets this binding. Existing
-- completed scaffold orders have no sealed result and remain unapprovable.
ALTER TABLE work_orders ADD COLUMN verification_context_id TEXT NOT NULL DEFAULT '';
CREATE INDEX verification_contexts_sealing ON verification_contexts(workspace_id,task_id,state);
CREATE INDEX verification_operations_resolution ON verification_operations(workspace_id,task_id,state);

-- Recovery appends an authorization or consumes its successor binding once.
-- Original identity, disposition, actor and authority never change.
CREATE FUNCTION verification_recovery_extension(prior JSONB, next JSONB) RETURNS BOOLEAN LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE a JSONB := COALESCE(NULLIF(prior,'null'::jsonb),'[]'::jsonb);
 b JSONB := COALESCE(NULLIF(next,'null'::jsonb),'[]'::jsonb);
 i INTEGER;
BEGIN
 IF jsonb_typeof(a)<>'array' OR jsonb_typeof(b)<>'array' OR jsonb_array_length(b)<jsonb_array_length(a) OR jsonb_array_length(b)>jsonb_array_length(a)+1 THEN RETURN FALSE; END IF;
 FOR i IN 0..jsonb_array_length(a)-1 LOOP
  IF (a->i)-'Successor' IS DISTINCT FROM (b->i)-'Successor' OR
   (NULLIF(a->i->'Successor','null'::jsonb) IS NOT NULL AND a->i IS DISTINCT FROM b->i) THEN RETURN FALSE; END IF;
 END LOOP;
 RETURN TRUE;
END $$;

CREATE OR REPLACE FUNCTION verification_record_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE prior JSONB; next JSONB;
BEGIN
 IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'verification records are retained'; END IF;
 IF NEW.workspace_id IS DISTINCT FROM OLD.workspace_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.context_id IS DISTINCT FROM OLD.context_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.logical_key IS DISTINCT FROM OLD.logical_key OR NEW.key_hash IS DISTINCT FROM OLD.key_hash THEN
  RAISE EXCEPTION 'verification provenance is immutable';
 END IF;
 IF TG_TABLE_NAME = 'verification_contexts' THEN
  IF OLD.state <> 'open' OR NEW.state NOT IN ('open','sealed') OR NEW.body - ARRAY['SealedAt','Result','Coverage'] <> OLD.body - ARRAY['SealedAt','Result','Coverage'] THEN RAISE EXCEPTION 'invalid context transition'; END IF;
  IF NEW.state='open' AND (NEW.body->'SealedAt' IS DISTINCT FROM OLD.body->'SealedAt' OR NEW.body->'Result' IS DISTINCT FROM OLD.body->'Result') THEN RAISE EXCEPTION 'unsealed result is invalid'; END IF;
  IF NEW.state='sealed' AND (NULLIF(NEW.body->'SealedAt','null'::jsonb) IS NULL OR NULLIF(NEW.body->'Result','null'::jsonb) IS NULL) THEN RAISE EXCEPTION 'sealed result required'; END IF;
 ELSIF TG_TABLE_NAME = 'verification_attempts' THEN
  IF NOT verification_recovery_extension(OLD.body->'Recovery',NEW.body->'Recovery') THEN RAISE EXCEPTION 'recovery authority is immutable'; END IF;
  IF NEW.state=OLD.state AND NEW.body-'Recovery'=OLD.body-'Recovery' THEN RETURN NEW; END IF;
  IF OLD.state NOT IN ('pending','running') OR NEW.state NOT IN ('succeeded','failed','timed_out','cancelled','blocked','waiting') OR NEW.body - ARRAY['State','Explanation','EndedAt','ExitCode','Recovery'] <> OLD.body - ARRAY['State','Explanation','EndedAt','ExitCode','Recovery'] THEN RAISE EXCEPTION 'invalid attempt transition'; END IF;
 ELSIF TG_TABLE_NAME = 'verification_operations' THEN
  IF NEW.body - ARRAY['History','Successors','Recovery'] <> OLD.body - ARRAY['History','Successors','Recovery'] OR NOT verification_recovery_extension(OLD.body->'Recovery',NEW.body->'Recovery') THEN RAISE EXCEPTION 'operation provenance is immutable'; END IF;
  prior:=COALESCE(NULLIF(OLD.body->'Successors','null'::jsonb),'[]'::jsonb);
  next:=COALESCE(NULLIF(NEW.body->'Successors','null'::jsonb),'[]'::jsonb);
  IF next<>prior AND (jsonb_array_length(next)<>jsonb_array_length(prior)+1 OR next-(jsonb_array_length(next)-1)<>prior) THEN RAISE EXCEPTION 'successor history is append only'; END IF;
  prior:=OLD.body->'History'; next:=NEW.body->'History';
  IF next=prior THEN
   IF NEW.state<>OLD.state THEN RAISE EXCEPTION 'operation state needs observation'; END IF;
  ELSIF jsonb_array_length(next)<>jsonb_array_length(prior)+1 OR next-(jsonb_array_length(next)-1)<>prior OR next->(jsonb_array_length(next)-1)->>'State'<>NEW.state THEN RAISE EXCEPTION 'operation history is append only'; END IF;
 ELSE RAISE EXCEPTION 'verification record is immutable';
 END IF;
 RETURN NEW;
END $$;
