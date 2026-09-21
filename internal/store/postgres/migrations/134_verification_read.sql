-- req-verification-kits REQ-6/REQ-7. feature-verification-kit-execution VK-9, DEC-43.
-- Read projections exclude evidence payloads and execution inputs.
-- Fixed UTC nanoseconds preserve timestamp ordering without timezone casts.

ALTER TABLE verification_contexts ADD COLUMN read_at TEXT GENERATED ALWAYS AS (CASE WHEN COALESCE(body #>> '{CreatedAt}','')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE (LEFT(body #>> '{CreatedAt}',19)||'.'||RPAD(REPLACE(REPLACE(SUBSTRING(body #>> '{CreatedAt}',20),'Z',''),'.',''),9,'0')||'Z') END) STORED;

CREATE INDEX verification_contexts_read_page ON verification_contexts(workspace_id,task_id,read_at,id COLLATE "C");

CREATE VIEW verification_read_contexts AS SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('work_order_id',LEFT(COALESCE(r.body #>> '{WorkOrderID}',''),2048),
  'created_by',LEFT(COALESCE(r.body #>> '{CreatedBy}',''),2048),
  'sealed_at',LEFT(COALESCE(r.body #>> '{SealedAt}',''),2048),
  'outcome',LEFT(COALESCE(r.body #>> '{Result,Submission,outcome}',''),2048),
  'deciding_actor',LEFT(COALESCE(r.body #>> '{Result,Actor}',''),2048),
  'disposition',LEFT(COALESCE(r.body #>> '{Result,Disposition}',''),2048),
  'required_action',LEFT(COALESCE(r.body #>> '{Result,Submission,feedback}',''),2048),
  'scope',LEFT(COALESCE(r.body #>> '{ReviewScope}',''),2048),
  'baseline_sha',LEFT(COALESCE(r.body #>> '{BaselineSHA}',''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(r.body #>> '{WorkOrderID}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{CreatedBy}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{SealedAt}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Submission,outcome}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Actor}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Disposition}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Submission,feedback}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{ReviewScope}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{BaselineSHA}',''))>2048 THEN TRUE::text ELSE FALSE::text END,
  'source_sha',COALESCE(w.head_sha,''),
  'claimant',COALESCE(w.claimant_id,''),
  'stage_state',COALESCE(w.state,''),
  'attempt_count',CAST((SELECT COUNT(*) FROM verification_attempts a WHERE a.workspace_id=r.workspace_id AND a.task_id=r.task_id AND a.context_id=r.id) AS TEXT)) AS metadata
 FROM verification_contexts r LEFT JOIN work_orders w ON w.workspace_id=r.workspace_id AND w.task_id=r.task_id AND w.id=r.body #>> '{WorkOrderID}';

ALTER TABLE verification_attempts ADD COLUMN read_at TEXT GENERATED ALWAYS AS (CASE WHEN COALESCE(body #>> '{StartedAt}','')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE (LEFT(body #>> '{StartedAt}',19)||'.'||RPAD(REPLACE(REPLACE(SUBSTRING(body #>> '{StartedAt}',20),'Z',''),'.',''),9,'0')||'Z') END) STORED;

CREATE INDEX verification_attempts_read_page ON verification_attempts(workspace_id,task_id,context_id,read_at,id COLLATE "C");

CREATE VIEW verification_read_attempts AS SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('created_by',LEFT(COALESCE(r.body #>> '{CreatedBy}',''),2048),
  'started_at',LEFT(COALESCE(r.body #>> '{StartedAt}',''),2048),
  'ended_at',LEFT(COALESCE(r.body #>> '{EndedAt}',''),2048),
  'outcome',LEFT(COALESCE(r.body #>> '{State}',''),2048),
  'required_action',LEFT(COALESCE(r.body #>> '{Explanation}',''),2048),
  'kind',LEFT(COALESCE(r.body #>> '{Subject,kind}',''),2048),
  'kit_id',LEFT(COALESCE(r.body #>> '{Subject,kit_id}',''),2048),
  'kit_version',LEFT(COALESCE(r.body #>> '{Subject,kit_version}',''),2048),
  'digest',LEFT(COALESCE(r.body #>> '{Subject,content_digest}',''),2048),
  'exercise_id',LEFT(COALESCE(r.body #>> '{Subject,exercise_id}',''),2048),
  'obligation_id',LEFT(COALESCE(r.body #>> '{Subject,obligation_id}',''),2048),
  'contract_digest',LEFT(COALESCE(r.body #>> '{Subject,contract_digest}',''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(r.body #>> '{CreatedBy}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{StartedAt}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{EndedAt}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{State}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Explanation}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Subject,kind}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Subject,kit_id}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Subject,kit_version}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Subject,content_digest}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Subject,exercise_id}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Subject,obligation_id}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Subject,contract_digest}',''))>2048 THEN TRUE::text ELSE FALSE::text END) AS metadata
 FROM verification_attempts r;

ALTER TABLE verification_evidence ADD COLUMN read_at TEXT GENERATED ALWAYS AS (CASE WHEN COALESCE(body #>> '{Envelope,received_at}','')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE (LEFT(body #>> '{Envelope,received_at}',19)||'.'||RPAD(REPLACE(REPLACE(SUBSTRING(body #>> '{Envelope,received_at}',20),'Z',''),'.',''),9,'0')||'Z') END) STORED;

CREATE INDEX verification_evidence_read_page ON verification_evidence(workspace_id,task_id,context_id,read_at,id COLLATE "C");

CREATE VIEW verification_read_evidence AS SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('type',LEFT(COALESCE(r.body #>> '{Envelope,type}',''),2048),
  'captured_at',LEFT(COALESCE(r.body #>> '{Envelope,captured_at}',''),2048),
  'submitted_by',LEFT(COALESCE(r.body #>> '{Envelope,submitted_by}',''),2048),
  'digest',LEFT(COALESCE(r.body #>> '{Digest}',''),2048),
  'assertion_id',LEFT(COALESCE(r.body #>> '{Envelope,payload,assertion_id}',''),2048),
  'outcome',LEFT(COALESCE(r.body #>> '{Envelope,payload,outcome}',''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(r.body #>> '{Envelope,type}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Envelope,captured_at}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Envelope,submitted_by}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Digest}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Envelope,payload,assertion_id}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Envelope,payload,outcome}',''))>2048 THEN TRUE::text ELSE FALSE::text END) AS metadata
 FROM verification_evidence r WHERE r.state='evidence';

ALTER TABLE verification_operations ADD COLUMN read_at TEXT GENERATED ALWAYS AS (CASE WHEN COALESCE(body #>> '{CreatedAt}','')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE (LEFT(body #>> '{CreatedAt}',19)||'.'||RPAD(REPLACE(REPLACE(SUBSTRING(body #>> '{CreatedAt}',20),'Z',''),'.',''),9,'0')||'Z') END) STORED;

CREATE INDEX verification_operations_read_page ON verification_operations(workspace_id,task_id,context_id,read_at,id COLLATE "C");

CREATE VIEW verification_read_operations AS SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('step_id',LEFT(COALESCE(r.body #>> '{StepID}',''),2048),
  'target',LEFT(COALESCE(r.body #>> '{Target}',''),2048),
  'retry_policy',LEFT(COALESCE(r.body #>> '{RetryPolicy}',''),2048),
  'created_by',LEFT(COALESCE(r.body #>> '{CreatedBy}',''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(r.body #>> '{StepID}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Target}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{RetryPolicy}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{CreatedBy}',''))>2048 THEN TRUE::text ELSE FALSE::text END) AS metadata
 FROM verification_operations r;

ALTER TABLE verification_publications ADD COLUMN read_at TEXT GENERATED ALWAYS AS (CASE WHEN COALESCE(body #>> '{CreatedAt}','')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE (LEFT(body #>> '{CreatedAt}',19)||'.'||RPAD(REPLACE(REPLACE(SUBSTRING(body #>> '{CreatedAt}',20),'Z',''),'.',''),9,'0')||'Z') END) STORED;

CREATE INDEX verification_publications_read_page ON verification_publications(workspace_id,task_id,context_id,read_at,id COLLATE "C");

CREATE VIEW verification_read_publications AS SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('head_sha',LEFT(COALESCE(r.body #>> '{HeadSHA}',''),2048),
  'body_digest',LEFT(COALESCE(r.body #>> '{BodyDigest}',''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(r.body #>> '{HeadSHA}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{BodyDigest}',''))>2048 THEN TRUE::text ELSE FALSE::text END) AS metadata
 FROM verification_publications r;

ALTER TABLE verification_obligations ADD COLUMN read_at TEXT GENERATED ALWAYS AS (CASE WHEN COALESCE(body #>> '{CreatedAt}','')='' THEN '1970-01-01T00:00:00.000000000Z' ELSE (LEFT(body #>> '{CreatedAt}',19)||'.'||RPAD(REPLACE(REPLACE(SUBSTRING(body #>> '{CreatedAt}',20),'Z',''),'.',''),9,'0')||'Z') END) STORED;

CREATE INDEX verification_obligations_read_page ON verification_obligations(workspace_id,task_id,context_id,read_at,id COLLATE "C");

CREATE VIEW verification_read_obligations AS SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('obligation_id',LEFT(COALESCE(r.body #>> '{ID}',''),2048),
  'description',LEFT(COALESCE(r.body #>> '{Description}',''),2048),
  'digest',LEFT(COALESCE(r.body #>> '{Digest}',''),2048),
  'created_by',LEFT(COALESCE(r.body #>> '{CreatedBy}',''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(r.body #>> '{ID}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Description}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Digest}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{CreatedBy}',''))>2048 THEN TRUE::text ELSE FALSE::text END) AS metadata
 FROM verification_obligations r;

CREATE VIEW verification_read_selections AS SELECT r.workspace_id,r.task_id,CONCAT(r.id,':',k.value #>> '{kit_id}') AS id,r.context_id,r.run_id,r.state,c.read_at,jsonb_build_object('kit_id',LEFT(COALESCE(k.value #>> '{kit_id}',''),2048),
  'digest',COALESCE(k.value #>> '{digest}',''),
  'eligibility',COALESCE(k.value #>> '{eligibility}',''),
  'reasons',LEFT(COALESCE((k.value->'reasons')::text,'[]'),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE((k.value->'reasons')::text,'[]'))>2048 THEN TRUE::text ELSE FALSE::text END) AS metadata FROM verification_selections r JOIN verification_contexts c ON c.workspace_id=r.workspace_id AND c.task_id=r.task_id AND c.id=r.context_id CROSS JOIN LATERAL jsonb_array_elements(r.body #> '{Receipt,kits}') k(value);

-- Required status comes from the frozen subject contract, never an evidence flag.
CREATE VIEW verification_read_assertions AS
SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('type','assertion_result','assertion_id',LEFT(r.body #>> '{Envelope,payload,assertion_id}',2048),
 'outcome',r.body #>> '{Envelope,payload,outcome}','evidence_id',r.id,
 'required',CASE WHEN
 EXISTS(SELECT 1 FROM verification_obligations o WHERE o.workspace_id=r.workspace_id AND o.task_id=r.task_id AND o.context_id=r.context_id
 AND o.body->>'ID'=r.body #>> '{Envelope,subject,obligation_id}' AND o.body->>'Digest'=r.body #>> '{Envelope,subject,contract_digest}'
 AND (o.body #> '{Contract,required_assertions}') ? (r.body #>> '{Envelope,payload,assertion_id}'))
 OR EXISTS(SELECT 1 FROM verification_selections s CROSS JOIN LATERAL jsonb_array_elements(s.body->'Subjects') k(value)
 WHERE s.workspace_id=r.workspace_id AND s.task_id=r.task_id AND s.context_id=r.context_id
 AND k.value->'Subject'=r.body #> '{Envelope,subject}' AND (k.value #> '{Contract,required_assertions}') ? (r.body #>> '{Envelope,payload,assertion_id}'))
 THEN TRUE::text ELSE FALSE::text END) AS metadata
 FROM verification_evidence r WHERE r.state='evidence' AND r.body #>> '{Envelope,type}'='assertion_result';
