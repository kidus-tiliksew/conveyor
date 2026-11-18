package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func init() {
	readEgressCases.Register("requirement-list-page", buildRequirementListPage)
}

// The baseline statements are the delivery reads at
// storetest.RequirementListPageEgressBaselineRevision, copied inline. Do not
// edit them to follow the store.
const (
	legacyRequirementDeliveryEventsSQL = `SELECT e.id,e.task_id,e.job_id,e.kind,e.actor_id,e.actor_role,e.payload_json,e.at,e.workspace_id
		FROM events e JOIN tasks t ON t.id=e.task_id
		WHERE t.workspace_id=$1 AND e.task_id=ANY($2::text[]) AND e.kind IN (
			'merge.confirmed','merge.reconciled','review.round_completed',
			'task.context_requirement_added','task.context_requirement_activated','task.context_requirement_removed'
		)
		ORDER BY e.task_id,e.at,e.id`
	legacyRequirementDeliveryLinksSQL = `SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at,created_by_event_id FROM (
			SELECT l.*,ROW_NUMBER() OVER (PARTITION BY l.src_type,l.src_id ORDER BY l.dst_type COLLATE "C",l.dst_id COLLATE "C",l.kind COLLATE "C") AS rn
			FROM unnest($2::text[],$3::text[]) AS q(node_type,node_id)
			JOIN links l ON l.workspace_id=$1 AND l.src_type=q.node_type AND l.src_id=q.node_id
			WHERE (l.kind='serves' AND l.src_type='requirement' AND l.dst_type IN ('task','blueprint')) OR
				(l.kind='versions' AND l.src_type='blueprint' AND l.dst_type='blueprint_version') OR
				(l.kind='materializes' AND l.src_type='blueprint_version' AND l.dst_type='task')
		) selected WHERE rn<=$4 ORDER BY src_type COLLATE "C",src_id COLLATE "C",dst_type COLLATE "C",dst_id COLLATE "C",kind COLLATE "C"`
)

// buildRequirementListPage measures the requirement list page's store reads.
// The baseline walks delivery lineage with the pre-change frontier, which
// also fetches reached tasks, and reads whole delivery event payloads; the
// candidate calls the public batched walk and delivery event read. Both
// variants read the rows, version summaries, requirement events, and task
// summaries through the public methods.
func buildRequirementListPage(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, pad, err := storetest.ReadEgressSizes()
	if err != nil {
		t.Fatal(err)
	}
	pageWorkspace, fixture := storetest.SeedRequirementListPageEgress(t, ctx, st, noise, pad)
	if _, err := st.pool.Exec(ctx, "ANALYZE events"); err != nil {
		t.Fatal(err)
	}
	legacy := storetest.RequirementListPageReads{
		DeliveryLineage: func(ctx context.Context, ids []string, budget core.LineageTraversalBudget) (map[string][]core.LineageLink, error) {
			ctx = dbmetrics.WithReadLabel(ctx, "LegacyRequirementDeliveryLineage")
			return storetest.LegacyFetchRequirementDeliveryLineage(ctx, ids, budget, func(ctx context.Context, nodes []core.LineageNode, limit int) (map[core.LineageNode][]core.LineageLink, error) {
				return legacyRequirementDeliveryLinks(ctx, st, nodes, limit)
			})
		},
		DeliveryEvents: func(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
			out := map[string][]core.Event{}
			if len(taskIDs) == 0 {
				return out, nil
			}
			events, err := legacyEventRows(ctx, st, "LegacyRequirementDeliveryEventsForTasks", legacyRequirementDeliveryEventsSQL, workspace(ctx), taskIDs)
			for _, event := range events {
				out[event.TaskID] = append(out[event.TaskID], event)
			}
			return out, err
		},
	}
	current := storetest.RequirementListPageReads{DeliveryLineage: st.ListRequirementDeliveryLineageByRequirement, DeliveryEvents: st.ListRequirementDeliveryEventsForTasks}
	return storetest.ReadEgressCase{Name: "requirement-list-page", Fixture: fixture,
		BaselineRevision: storetest.RequirementListPageEgressBaselineRevision,
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			return storetest.ReadRequirementListPage(store.WithWorkspace(ctx, pageWorkspace), st, legacy)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			return storetest.ReadRequirementListPage(store.WithWorkspace(ctx, pageWorkspace), st, current)
		}}
}

func legacyRequirementDeliveryLinks(ctx context.Context, st *Store, nodes []core.LineageNode, limit int) (map[core.LineageNode][]core.LineageLink, error) {
	types, ids := make([]string, len(nodes)), make([]string, len(nodes))
	for i, node := range nodes {
		types[i], ids[i] = string(node.Type), node.ID
	}
	rows, err := st.boundary.Query(ctx, legacyRequirementDeliveryLinksSQL, workspace(ctx), types, ids, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[core.LineageNode][]core.LineageLink, len(nodes))
	for rows.Next() {
		var link core.LineageLink
		var legacy pgtype.Text
		var createdAt pgtype.Timestamptz
		var eventID pgtype.Int8
		if err := rows.Scan(&link.Workspace, &link.SrcType, &link.SrcID, &link.DstType, &link.DstID, &link.Kind, &legacy, &createdAt, &eventID); err != nil {
			return nil, err
		}
		link.LegacyCreatedByEvent, link.CreatedByEventID, link.CreatedAt = legacy.String, eventID.Int64, createdAt.Time
		node := core.LineageNode{Type: link.SrcType, ID: link.SrcID}
		out[node] = append(out[node], link)
	}
	return out, rows.Err()
}
