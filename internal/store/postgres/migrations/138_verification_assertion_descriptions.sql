-- feature-verification-kit-execution v6 VK-3.1 (req-verification-kits REQ-10).
-- Schema-2 contracts store a described required assertion as {"id","description"};
-- undescribed assertions remain bare ID strings. Required status matches either
-- form. The view keeps its columns; no stored record changes.
CREATE OR REPLACE VIEW verification_read_assertions AS
SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 jsonb_build_object('type','assertion_result','assertion_id',LEFT(r.body #>> '{Envelope,payload,assertion_id}',2048),
 'outcome',r.body #>> '{Envelope,payload,outcome}','evidence_id',r.id,
 'required',CASE WHEN
 EXISTS(SELECT 1 FROM verification_obligations o WHERE o.workspace_id=r.workspace_id AND o.task_id=r.task_id AND o.context_id=r.context_id
 AND o.body->>'ID'=r.body #>> '{Envelope,subject,obligation_id}' AND o.body->>'Digest'=r.body #>> '{Envelope,subject,contract_digest}'
 AND ((o.body #> '{Contract,required_assertions}') ? (r.body #>> '{Envelope,payload,assertion_id}')
 OR (o.body #> '{Contract,required_assertions}') @> jsonb_build_array(jsonb_build_object('id',r.body #>> '{Envelope,payload,assertion_id}'))))
 OR EXISTS(SELECT 1 FROM verification_selections s CROSS JOIN LATERAL jsonb_array_elements(s.body->'Subjects') k(value)
 WHERE s.workspace_id=r.workspace_id AND s.task_id=r.task_id AND s.context_id=r.context_id
 AND k.value->'Subject'=r.body #> '{Envelope,subject}' AND ((k.value #> '{Contract,required_assertions}') ? (r.body #>> '{Envelope,payload,assertion_id}')
 OR (k.value #> '{Contract,required_assertions}') @> jsonb_build_array(jsonb_build_object('id',r.body #>> '{Envelope,payload,assertion_id}'))))
 THEN TRUE::text ELSE FALSE::text END) AS metadata
 FROM verification_evidence r WHERE r.state='evidence' AND r.body #>> '{Envelope,type}'='assertion_result';
