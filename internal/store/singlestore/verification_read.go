package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// feature-verification-kit-execution VK-9 / DEC-43: backend-bounded metadata selection, separate from claim-bound snapshots.
func (s *Store) ReadVerificationPage(ctx context.Context, a store.VerificationAccess, p store.VerificationPageRequest) (store.VerificationReadPage, error) {
	cursor, err := store.ValidateVerificationPage(ctx, a, &p)
	if err != nil {
		return store.VerificationReadPage{}, err
	}
	if err = store.AuthorizeVerificationUserRead(ctx, s, a); err != nil {
		return store.VerificationReadPage{}, err
	}
	var result store.VerificationReadPage
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.verificationScopeTx(ctx, tx, a, false); err != nil {
			return err
		}
		ws := documentWorkspace(ctx)
		if p.ContextID != "" {
			var found string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM verification_contexts WHERE workspace_id=? AND task_id=? AND id=?`, ws, a.TaskID, p.ContextID).Scan(&found); err != nil {
				return store.ErrVerificationAccess
			}
		}
		// ValidateVerificationPage selects kind from the closed collection set.
		rows, err := tx.QueryContext(ctx, `SELECT id,context_id,run_id,state,read_at,metadata FROM (`+verificationReadProjection(p.Kind)+`) projected`+` WHERE workspace_id=? AND task_id=? AND (?='' OR context_id=?) AND (?='' OR read_at<? OR (read_at=? AND id<?)) ORDER BY read_at DESC,id DESC LIMIT ?`, ws, a.TaskID, p.ContextID, p.ContextID, cursor.At, cursor.At, cursor.At, cursor.ID, p.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		items := []store.VerificationReadItem{}
		for rows.Next() {
			var v store.VerificationReadItem
			if err = rows.Scan(&v.ID, &v.ContextID, &v.RunID, &v.State, &v.At, &v.Metadata); err != nil {
				return err
			}
			items = append(items, v)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		result = store.VerificationPageResult(ctx, a, p, items)
		return nil
	})
	return result, err
}

func (s *Store) ReadVerificationDetail(ctx context.Context, a store.VerificationAccess, contextID, evidenceID string) (store.VerificationEvidenceRecord, error) {
	if a.UserID == "" {
		return store.VerificationEvidenceRecord{}, store.ErrVerificationAccess
	}
	if err := store.AuthorizeVerificationUserRead(ctx, s, a); err != nil {
		return store.VerificationEvidenceRecord{}, err
	}
	var result store.VerificationEvidenceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.verificationScopeTx(ctx, tx, a, false); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT e.body FROM verification_evidence e JOIN verification_contexts c ON c.workspace_id=e.workspace_id AND c.task_id=e.task_id AND c.id=e.context_id WHERE e.workspace_id=? AND e.task_id=? AND e.context_id=? AND e.id=? AND e.state='evidence'`, documentWorkspace(ctx), a.TaskID, contextID, evidenceID).Scan(&raw); err != nil {
			return store.ErrVerificationAccess
		}
		if json.Unmarshal(raw, &result) != nil || store.VerificationEvidenceDigest(result.Envelope) != result.Digest {
			return store.ErrVerificationInvalid
		}
		return nil
	})
	if err != nil {
		return store.VerificationEvidenceRecord{}, err
	}
	return result, nil
}

// Named projections keep the paged SELECT independent of retained payloads.
func verificationReadProjection(kind string) string {
	switch kind {
	case "assertions":
		return `SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 JSON_BUILD_OBJECT('type','assertion_result','assertion_id',LEFT(JSON_EXTRACT_STRING(r.body,'Envelope','payload','assertion_id'),2048),
 'outcome',JSON_EXTRACT_STRING(r.body,'Envelope','payload','outcome'),'evidence_id',r.id,
 'required',CASE WHEN
 EXISTS(SELECT 1 FROM verification_obligations o WHERE o.workspace_id=r.workspace_id AND o.task_id=r.task_id AND o.context_id=r.context_id
 AND JSON_EXTRACT_STRING(o.body,'ID')=JSON_EXTRACT_STRING(r.body,'Envelope','subject','obligation_id') AND JSON_EXTRACT_STRING(o.body,'Digest')=JSON_EXTRACT_STRING(r.body,'Envelope','subject','contract_digest')
 AND JSON_ARRAY_CONTAINS_STRING(JSON_EXTRACT_JSON(o.body,'Contract','required_assertions'),JSON_EXTRACT_STRING(r.body,'Envelope','payload','assertion_id')))
 OR EXISTS(SELECT 1 FROM verification_selections s JOIN TABLE(JSON_TO_ARRAY(JSON_EXTRACT_JSON(s.body,'Subjects'))) k
 WHERE s.workspace_id=r.workspace_id AND s.task_id=r.task_id AND s.context_id=r.context_id
 AND JSON_EXTRACT_JSON(k.table_col,'Subject')=JSON_EXTRACT_JSON(r.body,'Envelope','subject')
 AND JSON_ARRAY_CONTAINS_STRING(JSON_EXTRACT_JSON(k.table_col,'Contract','required_assertions'),JSON_EXTRACT_STRING(r.body,'Envelope','payload','assertion_id')))
 THEN 'true' ELSE 'false' END) AS metadata
 FROM verification_evidence r WHERE r.state='evidence' AND JSON_EXTRACT_STRING(r.body,'Envelope','type')='assertion_result'`

	case "contexts":
		return `SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 JSON_BUILD_OBJECT('work_order_id',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'WorkOrderID'),''),2048),
  'created_by',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'CreatedBy'),''),2048),
  'sealed_at',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'SealedAt'),''),2048),
  'outcome',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Result','Submission','outcome'),''),2048),
  'deciding_actor',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Result','Actor'),''),2048),
  'disposition',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Result','Disposition'),''),2048),
  'required_action',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Result','Submission','feedback'),''),2048),
  'scope',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'ReviewScope'),''),2048),
  'baseline_sha',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'BaselineSHA'),''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'WorkOrderID'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'CreatedBy'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'SealedAt'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Result','Submission','outcome'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Result','Actor'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Result','Disposition'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Result','Submission','feedback'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'ReviewScope'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'BaselineSHA'),''))>2048 THEN 'true' ELSE 'false' END,
  'source_sha',COALESCE(w.head_sha,''),
  'claimant',COALESCE(w.claimant_id,''),
  'stage_state',COALESCE(w.state,''),
  'attempt_count',CAST((SELECT COUNT(*) FROM verification_attempts a WHERE a.workspace_id=r.workspace_id AND a.task_id=r.task_id AND a.context_id=r.id) AS CHAR)) AS metadata
 FROM verification_contexts r LEFT JOIN work_orders w ON w.workspace_id=r.workspace_id AND w.task_id=r.task_id AND w.id=JSON_EXTRACT_STRING(r.body,'WorkOrderID')`
	case "attempts":
		return `SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 JSON_BUILD_OBJECT('created_by',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'CreatedBy'),''),2048),
  'started_at',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'StartedAt'),''),2048),
  'ended_at',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'EndedAt'),''),2048),
  'outcome',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'State'),''),2048),
  'required_action',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Explanation'),''),2048),
  'kind',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','kind'),''),2048),
  'kit_id',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','kit_id'),''),2048),
  'kit_version',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','kit_version'),''),2048),
  'digest',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','content_digest'),''),2048),
  'exercise_id',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','exercise_id'),''),2048),
  'obligation_id',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','obligation_id'),''),2048),
  'contract_digest',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','contract_digest'),''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'CreatedBy'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'StartedAt'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'EndedAt'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'State'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Explanation'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','kind'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','kit_id'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','kit_version'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','content_digest'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','exercise_id'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','obligation_id'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Subject','contract_digest'),''))>2048 THEN 'true' ELSE 'false' END) AS metadata
 FROM verification_attempts r`
	case "evidence":
		return `SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 JSON_BUILD_OBJECT('type',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','type'),''),2048),
  'captured_at',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','captured_at'),''),2048),
  'submitted_by',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','submitted_by'),''),2048),
  'digest',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Digest'),''),2048),
  'assertion_id',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','payload','assertion_id'),''),2048),
  'outcome',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','payload','outcome'),''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','type'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','captured_at'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','submitted_by'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Digest'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','payload','assertion_id'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Envelope','payload','outcome'),''))>2048 THEN 'true' ELSE 'false' END) AS metadata
 FROM verification_evidence r WHERE r.state='evidence'`
	case "operations":
		return `SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 JSON_BUILD_OBJECT('step_id',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'StepID'),''),2048),
  'target',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Target'),''),2048),
  'retry_policy',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'RetryPolicy'),''),2048),
  'created_by',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'CreatedBy'),''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'StepID'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Target'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'RetryPolicy'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'CreatedBy'),''))>2048 THEN 'true' ELSE 'false' END) AS metadata
 FROM verification_operations r`
	case "publications":
		return `SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 JSON_BUILD_OBJECT('head_sha',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'HeadSHA'),''),2048),
  'body_digest',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'BodyDigest'),''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'HeadSHA'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'BodyDigest'),''))>2048 THEN 'true' ELSE 'false' END) AS metadata
 FROM verification_publications r`
	case "obligations":
		return `SELECT r.workspace_id,r.task_id,r.id,r.context_id,r.run_id,r.state,r.read_at,
 JSON_BUILD_OBJECT('obligation_id',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'ID'),''),2048),
  'description',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Description'),''),2048),
  'digest',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'Digest'),''),2048),
  'created_by',LEFT(COALESCE(JSON_EXTRACT_STRING(r.body,'CreatedBy'),''),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'ID'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Description'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'Digest'),''))>2048 OR CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(r.body,'CreatedBy'),''))>2048 THEN 'true' ELSE 'false' END) AS metadata
 FROM verification_obligations r`
	case "selections":
		return `SELECT r.workspace_id,r.task_id,CONCAT(r.id,':',JSON_EXTRACT_STRING(k.table_col,'kit_id')) AS id,r.context_id,r.run_id,r.state,c.read_at,JSON_BUILD_OBJECT('kit_id',LEFT(COALESCE(JSON_EXTRACT_STRING(k.table_col,'kit_id'),''),2048),
  'digest',COALESCE(JSON_EXTRACT_STRING(k.table_col,'digest'),''),
  'eligibility',COALESCE(JSON_EXTRACT_STRING(k.table_col,'eligibility'),''),
  'reasons',LEFT(COALESCE(JSON_EXTRACT_STRING(k.table_col,'reasons'),'[]'),2048),
  'truncated',CASE WHEN CHAR_LENGTH(COALESCE(JSON_EXTRACT_STRING(k.table_col,'reasons'),'[]'))>2048 THEN 'true' ELSE 'false' END) AS metadata FROM verification_selections r JOIN verification_contexts c ON c.workspace_id=r.workspace_id AND c.task_id=r.task_id AND c.id=r.context_id JOIN TABLE(JSON_TO_ARRAY(JSON_EXTRACT_JSON(r.body,'Receipt','kits'))) k`
	}
	panic("unvalidated verification collection")
}
