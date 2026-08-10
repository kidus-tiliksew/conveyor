package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ListPendingProposals is one workspace-scoped read over the three durable
// authority tiers. Confirmation/dismissal changes the source rows, so the
// projection clears without stored attention state (REQ-1, REQ-3).
func (s *Store) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT proposal_id,title,tier,version,origin_type,origin_id,proposed_at
		FROM (
			SELECT v.document_id AS proposal_id,d.title,'system_design' AS tier,v.version,
				CASE WHEN v.origin_task_id IS NOT NULL THEN 'task' WHEN v.origin_session_id IS NOT NULL THEN 'session' ELSE 'operator' END AS origin_type,
				coalesce(v.origin_task_id,v.origin_session_id,'') AS origin_id,v.created_at AS proposed_at
			FROM system_design_versions v
			JOIN system_designs d ON d.workspace_id=v.workspace_id AND d.id=v.document_id
			WHERE v.workspace_id=$1 AND NOT v.confirmed AND NOT v.dismissed
			UNION ALL
			SELECT v.requirement_id,r.title,'requirement' AS tier,v.version,
				CASE WHEN v.origin_session_id <> '' THEN 'session' WHEN v.origin_drift_id <> '' THEN 'drift' ELSE v.origin END AS origin_type,
				coalesce(nullif(v.origin_session_id,''),nullif(v.origin_drift_id,''),'') AS origin_id,v.created_at AS proposed_at
			FROM requirement_versions v
			JOIN requirements r ON r.workspace_id=v.workspace_id AND r.id=v.requirement_id
			WHERE v.workspace_id=$1 AND NOT v.confirmed AND v.version > coalesce(r.current_version,0)
			UNION ALL
			SELECT d.id,d.statement,'decision' AS tier,NULL::integer AS version,
				CASE WHEN d.origin_task_id IS NOT NULL THEN 'task' WHEN d.origin_session_id IS NOT NULL THEN 'session' ELSE 'operator' END AS origin_type,
				coalesce(d.origin_task_id,d.origin_session_id,'') AS origin_id,d.created_at AS proposed_at
			FROM decisions d
			WHERE d.workspace_id=$1 AND d.status='proposed'
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
		if err = rows.Scan(&item.ID, &item.Title, &item.Tier, &version, &item.OriginType, &item.OriginID, &item.ProposedAt); err != nil {
			return nil, err
		}
		if version != nil {
			item.Version = *version
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
