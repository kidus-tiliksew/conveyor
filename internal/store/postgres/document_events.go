package postgres

import (
	"context"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Match the historical detail membership, but fetch payloads only for the page.
// One statement keeps the count and page on the same database snapshot.
func (s *Store) ListDocumentEventPage(ctx context.Context, kind core.LineageNodeType, id string, q store.DocumentEventQuery) (store.DocumentEventPage, error) {
	if err := store.ValidateDocumentEventQuery(kind, q); err != nil {
		return store.DocumentEventPage{}, err
	}
	page := store.DocumentEventPage{Events: []core.Event{}, Limit: q.Limit, Offset: q.Offset}
	var payload []byte
	query := requirementDocumentEventPageSQL
	if kind == core.LineageSystemDesign {
		query = systemDesignDocumentEventPageSQL
	}
	err := s.pool.QueryRow(ctx, query, workspace(ctx), id, q.SnapshotID, q.Limit, q.Offset).Scan(&page.Total, &page.SnapshotID, &payload)
	if err != nil {
		return page, err
	}
	err = json.Unmarshal(payload, &page.Events)
	return page, err
}

// Separate membership branches let PostgreSQL use document and task indexes.
// req-260802-72fc68 AC-5.1; req-accounts-and-membership AC-4.4;
// component-persistence: one statement pins count, snapshot and page together.
// OFFSET 0 keeps task lookups parameterized; flattening this lateral branch can
// choose a merge join that walks unrelated task IDs through the events index.
// The inner order exposes events_task_timeline_idx even for skewed task counts;
// the outer page still applies its own descending order after deduplication.
const requirementDocumentEventPageSQL = `WITH matched AS MATERIALIZED (
 SELECT e.id,e.at FROM events e
 WHERE e.workspace_id=$1 AND e.task_id IS NULL AND e.payload_json->>'requirement_id'=$2
 AND ($3::bigint=0 OR e.id<=$3)
 UNION
 SELECT e.id,e.at FROM task_context_proposals p
 JOIN LATERAL (
 SELECT e.id,e.at FROM events e
 WHERE e.workspace_id=$1 AND e.task_id=p.task_id AND ($3::bigint=0 OR e.id<=$3)
 ORDER BY e.at,e.id OFFSET 0
 ) e ON true
 WHERE p.workspace_id=$1 AND p.target_kind='requirement' AND p.target_id=$2 AND p.state='confirmed'
` + documentEventPageSQL

const systemDesignDocumentEventPageSQL = `WITH matched AS MATERIALIZED (
 SELECT e.id,e.at FROM events e
 WHERE e.workspace_id=$1 AND e.kind LIKE 'system_design.%' AND e.payload_json->>'document_id'=$2
 AND ($3::bigint=0 OR e.id<=$3)
` + documentEventPageSQL

const documentEventPageSQL = `), selected AS (
 SELECT id,at FROM matched ORDER BY at DESC,id DESC LIMIT $4 OFFSET $5
 ) SELECT (SELECT count(*) FROM matched),
 CASE WHEN $3::bigint>0 THEN $3 ELSE COALESCE((SELECT max(id) FROM matched),0) END,
 COALESCE((SELECT jsonb_agg(jsonb_build_object('id',e.id,'task_id',COALESCE(e.task_id,''),'job_id',COALESCE(e.job_id,''),
 'kind',e.kind,'actor_id',e.actor_id,'actor_role',e.actor_role,'payload',e.payload_json,'at',e.at) ORDER BY e.at DESC,e.id DESC)
 FROM selected p JOIN events e ON e.id=p.id AND e.workspace_id=$1),'[]'::jsonb)`
