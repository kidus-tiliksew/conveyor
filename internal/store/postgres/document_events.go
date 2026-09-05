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
	err := s.pool.QueryRow(ctx, `WITH matched AS MATERIALIZED (
 SELECT e.id,e.at FROM events e WHERE e.workspace_id=$1 AND ($4::bigint=0 OR e.id<=$4) AND (
 ($2='requirement' AND ((e.task_id IS NULL AND e.payload_json->>'requirement_id'=$3) OR EXISTS (
 SELECT 1 FROM task_context_proposals p WHERE p.workspace_id=$1 AND p.task_id=e.task_id AND p.target_kind='requirement' AND p.target_id=$3 AND p.state='confirmed')))
 OR ($2='system_design' AND e.kind LIKE 'system_design.%' AND e.payload_json->>'document_id'=$3))
 ), selected AS (
 SELECT id,at FROM matched ORDER BY at DESC,id DESC LIMIT $5 OFFSET $6
 ) SELECT (SELECT count(*) FROM matched),
 CASE WHEN $4::bigint>0 THEN $4 ELSE COALESCE((SELECT max(id) FROM matched),0) END,
 COALESCE((SELECT jsonb_agg(jsonb_build_object('id',e.id,'task_id',COALESCE(e.task_id,''),'job_id',COALESCE(e.job_id,''),
 'kind',e.kind,'actor_id',e.actor_id,'actor_role',e.actor_role,'payload',e.payload_json,'at',e.at) ORDER BY e.at DESC,e.id DESC)
 FROM selected p JOIN events e ON e.id=p.id AND e.workspace_id=$1),'[]'::jsonb)`, workspace(ctx), string(kind), id, q.SnapshotID, q.Limit, q.Offset).Scan(&page.Total, &page.SnapshotID, &payload)
	if err != nil {
		return page, err
	}
	err = json.Unmarshal(payload, &page.Events)
	return page, err
}
