-- feature-verification-kit-execution VK-13 (proposed component-persistence VK-STORE-15
-- and component-http-api VK-HTTP-9); req-verification-kits REQ-3/AC-3.2, REQ-4/AC-4.3.
-- Context headers expose the sealed checkpoint as bounded scalars: reason from the
-- submission, required action and a server-composed ground summary from the
-- checkpoint record. The view keeps its columns; no stored record changes.
CREATE OR REPLACE VIEW verification_read_contexts AS SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('work_order_id',LEFT(COALESCE(r.body #>> '{WorkOrderID}',''),2048),
  'created_by',LEFT(COALESCE(r.body #>> '{CreatedBy}',''),2048),
  'sealed_at',LEFT(COALESCE(r.body #>> '{SealedAt}',''),2048),
  'outcome',LEFT(COALESCE(r.body #>> '{Result,Submission,outcome}',''),2048),
  'deciding_actor',LEFT(COALESCE(r.body #>> '{Result,Actor}',''),2048),
  'disposition',LEFT(COALESCE(r.body #>> '{Result,Disposition}',''),2048),
  'reason',LEFT(COALESCE(r.body #>> '{Result,Submission,feedback}',''),2048),
  'required_action',LEFT(COALESCE(r.body #>> '{Result,Checkpoint,required_action}',''),2048),
  'checkpoint_grounds',LEFT(COALESCE(r.body #>> '{Result,Checkpoint,summary}',''),2048),
  'scope',LEFT(COALESCE(r.body #>> '{ReviewScope}',''),2048),
  'baseline_sha',LEFT(COALESCE(r.body #>> '{BaselineSHA}',''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(r.body #>> '{WorkOrderID}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{CreatedBy}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{SealedAt}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Submission,outcome}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Actor}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Disposition}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Submission,feedback}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Checkpoint,required_action}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{Result,Checkpoint,summary}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{ReviewScope}',''))>2048 OR CHAR_LENGTH(COALESCE(r.body #>> '{BaselineSHA}',''))>2048 THEN TRUE::text ELSE FALSE::text END,
  'source_sha',COALESCE(w.head_sha,''),
  'claimant',COALESCE(w.claimant_id,''),
  'stage_state',COALESCE(w.state,''),
  'attempt_count',CAST((SELECT COUNT(*) FROM verification_attempts a WHERE a.workspace_id=r.workspace_id AND a.task_id=r.task_id AND a.context_id=r.id) AS TEXT)) AS metadata
 FROM verification_contexts r LEFT JOIN work_orders w ON w.workspace_id=r.workspace_id AND w.task_id=r.task_id AND w.id=r.body #>> '{WorkOrderID}';
