package postgres

import (
	"context"
	_ "embed"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

//go:embed pending_proposals_attention.sql
var pendingProposalsAttentionSQL string

func attentionTasksCTE() string {
	const marker = "\nSELECT count(*)::bigint"
	index := strings.LastIndex(pendingProposalsAttentionSQL, marker)
	if index < 0 {
		panic("pending proposals attention SQL lost its terminal count query")
	}
	return pendingProposalsAttentionSQL[:index]
}

// ListPendingProposals is one workspace-scoped read over the three durable
// authority tiers. Confirmation/dismissal changes the source rows, so the
// projection clears without stored attention state (REQ-1, REQ-3).
func (s *Store) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT proposal_id,title,tier,version,origin_type,origin_id,target_kind,justification,proposed_at
		FROM (
			SELECT v.document_id AS proposal_id,d.title,'system_design' AS tier,v.version,
				CASE WHEN v.origin_task_id IS NOT NULL THEN 'task' WHEN v.origin_session_id IS NOT NULL THEN 'session' ELSE 'operator' END AS origin_type,
				coalesce(v.origin_task_id,v.origin_session_id,'') AS origin_id,'' AS target_kind,'' AS justification,v.created_at AS proposed_at
			FROM system_design_versions v
			JOIN system_designs d ON d.workspace_id=v.workspace_id AND d.id=v.document_id
			WHERE v.workspace_id=$1 AND d.archived_at IS NULL AND NOT v.confirmed AND NOT v.dismissed
			UNION ALL
			SELECT v.requirement_id,r.title,'requirement' AS tier,v.version,
				CASE WHEN v.origin_task_id <> '' THEN 'task' WHEN v.origin_session_id <> '' THEN 'session' WHEN v.origin_drift_id <> '' THEN 'drift' ELSE v.origin END AS origin_type,
				coalesce(nullif(v.origin_task_id,''),nullif(v.origin_session_id,''),nullif(v.origin_drift_id,''),'') AS origin_id,'' AS target_kind,'' AS justification,v.created_at AS proposed_at
			FROM requirement_versions v
			JOIN requirements r ON r.workspace_id=v.workspace_id AND r.id=v.requirement_id
			WHERE v.workspace_id=$1 AND r.archived_at IS NULL AND NOT v.confirmed AND NOT v.retired
			  AND v.version > coalesce(r.current_version,0)
			UNION ALL
			SELECT d.id,d.statement,'decision' AS tier,NULL::integer AS version,
				CASE WHEN d.origin_task_id IS NOT NULL THEN 'task' WHEN d.origin_session_id IS NOT NULL THEN 'session' ELSE 'operator' END AS origin_type,
				coalesce(d.origin_task_id,d.origin_session_id,'') AS origin_id,'' AS target_kind,'' AS justification,d.created_at AS proposed_at
			FROM decisions d
			WHERE d.workspace_id=$1 AND d.status='proposed'
			UNION ALL
			SELECT p.target_id,p.target_title,'task_context',NULL::integer,'task',p.task_id,p.target_kind,p.justification,p.created_at
			FROM task_context_proposals p
			JOIN tasks t ON t.workspace_id=p.workspace_id AND t.id=p.task_id
			WHERE p.workspace_id=$1 AND p.state='proposed' AND t.state NOT IN ('merged','closed')
		) pending
		ORDER BY proposed_at,tier,proposal_id,version NULLS FIRST`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.PendingProposal, 0)
	for rows.Next() {
		var item core.PendingProposal
		var version *int
		if err = rows.Scan(&item.ID, &item.Title, &item.Tier, &version, &item.OriginType, &item.OriginID, &item.TargetKind, &item.Justification, &item.ProposedAt); err != nil {
			return nil, err
		}
		if version != nil {
			item.Version = *version
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) PendingProposalsProjection(ctx context.Context) (store.PendingProposalsProjection, error) {
	items, err := s.ListPendingProposals(ctx)
	if err != nil {
		return store.PendingProposalsProjection{}, err
	}
	var count int64
	err = s.pool.QueryRow(ctx, pendingProposalsAttentionSQL, workspace(ctx)).Scan(&count)
	if err != nil {
		return store.PendingProposalsProjection{}, err
	}
	return store.PendingProposalsProjection{Items: items, TaskCount: int(count)}, nil
}
