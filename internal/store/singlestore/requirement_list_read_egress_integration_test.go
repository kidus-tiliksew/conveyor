package singlestore

import (
	"context"
	"strings"
	"testing"

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
	legacyRequirementDeliveryEventsSQL = `SELECT id,task_id,COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND task_id IN (%s) AND kind IN ('merge.confirmed','merge.reconciled','review.round_completed','task.context_requirement_added','task.context_requirement_activated','task.context_requirement_removed') ORDER BY task_id,at,id`
	legacyRequirementDeliveryLinksSQL  = `SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy,created_at,event_id FROM (
			SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,
				COALESCE(legacy_created_by_event,'') AS legacy,created_at,COALESCE(created_by_event_id,0) AS event_id,
				ROW_NUMBER() OVER (PARTITION BY src_type,src_id ORDER BY dst_type,dst_id,kind) AS rn
			FROM links WHERE workspace_id=? AND (%s) AND (
				(kind='serves' AND src_type='requirement' AND dst_type IN ('task','blueprint')) OR
				(kind='versions' AND src_type='blueprint' AND dst_type='blueprint_version') OR
				(kind='materializes' AND src_type='blueprint_version' AND dst_type='task')
			)
		) selected WHERE rn<=? ORDER BY src_type,src_id,dst_type,dst_id,kind`
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
			ctx = dbmetrics.WithReadLabel(ctx, "LegacyRequirementDeliveryEventsForTasks")
			rows, err := documentBatchRows(ctx, st.db, legacyRequirementDeliveryEventsSQL, documentWorkspace(ctx), taskIDs)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			for rows.Next() {
				var e core.Event
				if err = rows.Scan(&e.ID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &e.Payload, &e.At); err != nil {
					return nil, err
				}
				out[e.TaskID] = append(out[e.TaskID], e)
			}
			return out, rows.Err()
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
	clauses := make([]string, 0, len(nodes))
	args := []any{documentWorkspace(ctx)}
	for _, node := range nodes {
		clauses = append(clauses, "(src_type=? AND src_id=?)")
		args = append(args, string(node.Type), node.ID)
	}
	args = append(args, limit)
	rows, err := documentRows(ctx, st.db, strings.Replace(legacyRequirementDeliveryLinksSQL, "%s", strings.Join(clauses, " OR "), 1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[core.LineageNode][]core.LineageLink, len(nodes))
	for rows.Next() {
		var link core.LineageLink
		if err := rows.Scan(&link.Workspace, &link.SrcType, &link.SrcID, &link.DstType, &link.DstID, &link.Kind, &link.LegacyCreatedByEvent, &link.CreatedAt, &link.CreatedByEventID); err != nil {
			return nil, err
		}
		node := core.LineageNode{Type: link.SrcType, ID: link.SrcID}
		out[node] = append(out[node], link)
	}
	return out, rows.Err()
}
