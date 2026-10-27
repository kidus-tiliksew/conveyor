package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) ListLineageNodeRecords(ctx context.Context, nodes []core.LineageNode) (map[core.LineageNode]store.LineageNodeRecord, error) {
	taskNodes := map[string][]core.LineageNode{}
	requirementNodes := map[string][]core.LineageNode{}
	referenceNodes := map[string][]core.LineageNode{}
	designNodes := map[string][]core.LineageNode{}
	decisionNodes := map[string][]core.LineageNode{}
	sessionNodes := map[string][]core.LineageNode{}
	orderNodes := map[string][]core.LineageNode{}
	bundleNodes := map[string][]core.LineageNode{}
	for _, node := range nodes {
		baseID := lineageRecordBaseID(node)
		switch node.Type {
		case core.LineageTask, core.LineageBlueprint, core.LineageBlueprintVersion:
			taskNodes[baseID] = append(taskNodes[baseID], node)
		case core.LineageRequirement, core.LineageRequirementVersion:
			requirementNodes[baseID] = append(requirementNodes[baseID], node)
		case core.LineageReferenceDocument, core.LineageReferenceDocumentVersion:
			referenceNodes[baseID] = append(referenceNodes[baseID], node)
		case core.LineageSystemDesign, core.LineageSystemDesignVersion:
			designNodes[baseID] = append(designNodes[baseID], node)
		case core.LineageDecision:
			decisionNodes[baseID] = append(decisionNodes[baseID], node)
		case core.LineageRepositoryPath:
			recordsPlaceholder := node
			_ = recordsPlaceholder
		case core.LineagePlanningSession:
			sessionNodes[baseID] = append(sessionNodes[baseID], node)
		case core.LineageWorkOrder:
			orderNodes[baseID] = append(orderNodes[baseID], node)
		case core.LineagePlanningBundle:
			bundleNodes[baseID] = append(bundleNodes[baseID], node)
		}
	}
	records := make(map[core.LineageNode]store.LineageNodeRecord, len(nodes))
	for _, node := range nodes {
		if node.Type == core.LineageRepositoryPath {
			records[node] = store.LineageNodeRecord{Title: node.ID}
		}
	}
	if err := s.queryLineageTaskRecords(ctx, taskNodes, records); err != nil {
		return nil, err
	}
	if err := s.queryLineageRequirementRecords(ctx, requirementNodes, records); err != nil {
		return nil, err
	}
	if len(referenceNodes) > 0 {
		rows, queryErr := s.boundary.Query(ctx, `SELECT id,name FROM reference_documents WHERE workspace_id=$1 AND id=ANY($2)`, workspace(ctx), lineageRecordIDs(referenceNodes))
		if queryErr != nil {
			return nil, queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var id, name string
			if scanErr := rows.Scan(&id, &name); scanErr != nil {
				return nil, scanErr
			}
			for _, node := range referenceNodes[id] {
				records[node] = store.LineageNodeRecord{Title: name}
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return nil, rowsErr
		}
	}
	if len(designNodes) > 0 {
		rows, queryErr := s.boundary.Query(ctx, `SELECT id,title,slug FROM system_designs WHERE workspace_id=$1 AND id=ANY($2)`, workspace(ctx), lineageRecordIDs(designNodes))
		if queryErr != nil {
			return nil, queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var id, title, slug string
			if scanErr := rows.Scan(&id, &title, &slug); scanErr != nil {
				return nil, scanErr
			}
			for _, node := range designNodes[id] {
				records[node] = store.LineageNodeRecord{Title: title, Slug: slug}
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return nil, rowsErr
		}
	}
	if len(decisionNodes) > 0 {
		rows, queryErr := s.boundary.Query(ctx, `SELECT id,statement FROM decisions WHERE workspace_id=$1 AND id=ANY($2)`, workspace(ctx), lineageRecordIDs(decisionNodes))
		if queryErr != nil {
			return nil, queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var id, title string
			if scanErr := rows.Scan(&id, &title); scanErr != nil {
				return nil, scanErr
			}
			for _, node := range decisionNodes[id] {
				records[node] = store.LineageNodeRecord{Title: title}
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return nil, rowsErr
		}
	}
	if err := s.queryLineageSessionRecords(ctx, sessionNodes, records); err != nil {
		return nil, err
	}
	if err := s.queryLineageOrderRecords(ctx, orderNodes, records); err != nil {
		return nil, err
	}
	if len(bundleNodes) > 0 {
		err := s.lineageStatusQuery(ctx, `SELECT id,title FROM planning_bundles WHERE workspace_id=$1 AND id=ANY($2)`, lineageRecordIDs(bundleNodes), func(rows pgx.Rows) error {
			var id, title string
			if err := rows.Scan(&id, &title); err != nil {
				return err
			}
			for _, node := range bundleNodes[id] {
				records[node] = store.LineageNodeRecord{Title: title}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return records, nil
}

// lineageStatusQuery runs one workspace-scoped batch read over ids.
func (s *Store) lineageStatusQuery(ctx context.Context, query string, ids []string, scan func(pgx.Rows) error) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.boundary.Query(ctx, query, workspace(ctx), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err = scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ListLineageNodeStatuses reads one batch per node class, only for the
// supplied nodes (component-lineage).
func (s *Store) ListLineageNodeStatuses(ctx context.Context, nodes []core.LineageNode) (map[core.LineageNode]core.LineageNodeStatus, error) {
	request := store.LineageStatusIDs(nodes)
	ids := request.IDs
	rows := store.LineageStatusRows{
		TaskStates: map[string]string{}, OrderStates: map[string]string{}, DecisionState: map[string]string{},
		SessionState: map[string]string{}, BundleState: map[string]string{},
		Documents: map[core.LineageNodeType]map[string]store.LineageStatusDocument{},
		Versions:  map[core.LineageNodeType]map[string]map[int]store.LineageStatusVersion{},
		Verdicts:  map[string]core.LineageNodeStatus{},
	}
	states := []struct {
		class core.LineageNodeType
		query string
		into  map[string]string
	}{
		{core.LineageTask, `SELECT id,state FROM tasks WHERE workspace_id=$1 AND id=ANY($2)`, rows.TaskStates},
		{core.LineageWorkOrder, `SELECT id,state FROM work_orders WHERE workspace_id=$1 AND id=ANY($2)`, rows.OrderStates},
		{core.LineageDecision, `SELECT id,status FROM decisions WHERE workspace_id=$1 AND id=ANY($2)`, rows.DecisionState},
		{core.LineagePlanningSession, `SELECT id,status FROM planning_sessions WHERE workspace_id=$1 AND id=ANY($2)`, rows.SessionState},
		{core.LineagePlanningBundle, `SELECT id,status FROM planning_bundles WHERE workspace_id=$1 AND id=ANY($2)`, rows.BundleState},
	}
	for _, state := range states {
		into := state.into
		if err := s.lineageStatusQuery(ctx, state.query, ids[state.class], func(r pgx.Rows) error {
			var id, value string
			if err := r.Scan(&id, &value); err != nil {
				return err
			}
			into[id] = value
			return nil
		}); err != nil {
			return nil, err
		}
	}
	documents := []struct {
		class    core.LineageNodeType
		document string
		versions string
	}{
		{core.LineageRequirement, `SELECT id,COALESCE(current_version,0),archived_at IS NOT NULL FROM requirements WHERE workspace_id=$1 AND id=ANY($2)`,
			`SELECT v.requirement_id,v.version,v.confirmed,v.retired FROM unnest($2::text[],$3::int[]) AS p(id,version)
				JOIN requirement_versions v ON v.workspace_id=$1 AND v.requirement_id=p.id AND v.version=p.version`},
		{core.LineageSystemDesign, `SELECT id,COALESCE(current_version,0),archived_at IS NOT NULL FROM system_designs WHERE workspace_id=$1 AND id=ANY($2)`,
			`SELECT v.document_id,v.version,v.confirmed,v.dismissed FROM unnest($2::text[],$3::int[]) AS p(id,version)
				JOIN system_design_versions v ON v.workspace_id=$1 AND v.document_id=p.id AND v.version=p.version`},
		{core.LineageReferenceDocument, `SELECT id,COALESCE(current_version,0),deleted_at IS NOT NULL FROM reference_documents WHERE workspace_id=$1 AND id=ANY($2)`, ""},
	}
	for _, document := range documents {
		byID := map[string]store.LineageStatusDocument{}
		rows.Documents[document.class] = byID
		if err := s.lineageStatusQuery(ctx, document.document, ids[document.class], func(r pgx.Rows) error {
			var id string
			var recorded store.LineageStatusDocument
			if err := r.Scan(&id, &recorded.CurrentVersion, &recorded.Archived); err != nil {
				return err
			}
			byID[id] = recorded
			return nil
		}); err != nil {
			return nil, err
		}
		pairs := request.Versions[document.class]
		if document.versions == "" || len(pairs) == 0 {
			continue
		}
		// Only the version rows the nodes name are read, never a document's
		// whole history (component-lineage).
		documentIDs, numbers := make([]string, len(pairs)), make([]int32, len(pairs))
		for index, pair := range pairs {
			documentIDs[index], numbers[index] = pair.DocumentID, int32(pair.Version)
		}
		versions := map[string]map[int]store.LineageStatusVersion{}
		rows.Versions[document.class] = versions
		versionRows, err := s.boundary.Query(ctx, document.versions, workspace(ctx), documentIDs, numbers)
		if err != nil {
			return nil, err
		}
		for versionRows.Next() {
			var id string
			var version int
			var recorded store.LineageStatusVersion
			if err = versionRows.Scan(&id, &version, &recorded.Confirmed, &recorded.Dismissed); err != nil {
				versionRows.Close()
				return nil, err
			}
			if observe := observeLineageStatusVersionRow.Load(); observe != nil {
				(*observe)(id, version)
			}
			if versions[id] == nil {
				versions[id] = map[int]store.LineageStatusVersion{}
			}
			versions[id][version] = recorded
		}
		versionRows.Close()
		if err = versionRows.Err(); err != nil {
			return nil, err
		}
	}
	// A verdict's outcome lives on its task's review.completed record; the
	// join reads only the named review orders' task ledgers.
	if err := s.lineageStatusQuery(ctx, `SELECT wo.id,e.payload_json::text FROM work_orders wo
		JOIN events e ON e.workspace_id=wo.workspace_id AND e.task_id=wo.task_id AND e.kind='review.completed'
		WHERE wo.workspace_id=$1 AND wo.id=ANY($2) ORDER BY e.id`, ids[core.LineageVerdict], func(r pgx.Rows) error {
		var orderID, payload string
		if err := r.Scan(&orderID, &payload); err != nil {
			return err
		}
		if status, ok := store.LineageVerdictStatus([]byte(payload), orderID); ok {
			rows.Verdicts[orderID] = status
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return rows.Statuses(nodes), nil
}

// observeLineageStatusVersionRow, when set by a test, sees each version
// row a status read returns, so fetch bounds are checked against rows.
var observeLineageStatusVersionRow atomic.Pointer[func(documentID string, version int)]

// lineageKindLinksSQL reads one kind at the named nodes through the links
// primary key (outbound) or links_destination_idx (inbound), in
// core.SortLineageKindLinks order (component-lineage).
// With newest it orders descending under the limit, and the caller restores
// ascending order.
func lineageKindLinksSQL(inbound, newest bool) string {
	near, far := "src", "dst"
	if inbound {
		near, far = "dst", "src"
	}
	order := ""
	if newest {
		order = " DESC"
	}
	return `SELECT l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,l.legacy_created_by_event,l.created_at,l.created_by_event_id
	FROM (SELECT DISTINCT node_type,node_id FROM unnest($2::text[],$3::text[]) AS f(node_type,node_id)) f
	JOIN links l ON l.workspace_id=$1 AND l.` + near + `_type=f.node_type AND l.` + near + `_id=f.node_id
	WHERE l.kind=$4 AND l.` + far + `_type=$5 AND btrim(l.` + far + `_id,E' \t\n\r\v\f')<>''
		AND NOT (l.src_type=l.dst_type AND l.src_id=l.dst_id)
		AND (COALESCE(l.created_by_event_id,0)>0 OR (l.kind='historical_feature_assignment' AND btrim(COALESCE(l.legacy_created_by_event,''),E' \t\n\r\v\f')<>''))
	ORDER BY COALESCE(l.created_by_event_id,0)` + order + `,l.src_type COLLATE "C"` + order + `,l.src_id COLLATE "C"` + order + `,l.dst_type COLLATE "C"` + order + `,l.dst_id COLLATE "C"` + order + `,l.kind COLLATE "C"` + order + `
	LIMIT $6`
}

func (s *Store) ListLineageKindLinks(ctx context.Context, query core.LineageKindLinkQuery) ([]core.LineageLink, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	types, ids := make([]string, len(query.Nodes)), make([]string, len(query.Nodes))
	for index, node := range query.Nodes {
		types[index], ids[index] = string(node.Type), node.ID
	}
	rows, err := s.boundary.Query(ctx, lineageKindLinksSQL(query.Inbound, query.Newest), workspace(ctx), types, ids, query.Kind, string(query.FarType), query.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.LineageLink{}
	for rows.Next() {
		var link core.LineageLink
		var legacy pgtype.Text
		var createdAt pgtype.Timestamptz
		var eventID pgtype.Int8
		if err = rows.Scan(&link.Workspace, &link.SrcType, &link.SrcID, &link.DstType, &link.DstID, &link.Kind, &legacy, &createdAt, &eventID); err != nil {
			return nil, err
		}
		link.LegacyCreatedByEvent, link.CreatedByEventID, link.CreatedAt = legacy.String, eventID.Int64, createdAt.Time
		if link.Validate() == nil {
			out = append(out, link)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	core.SortLineageKindLinks(out)
	return out, nil
}

func (s *Store) ListLineageContextRecords(ctx context.Context, nodes []core.LineageNode) (store.LineageContextRecords, error) {
	taskIDs, requirementIDs := map[string]bool{}, map[string]bool{}
	for _, node := range nodes {
		switch node.Type {
		case core.LineageTask:
			taskIDs[node.ID] = true
		case core.LineageRequirement:
			requirementIDs[node.ID] = true
		}
	}
	result := store.LineageContextRecords{
		Tasks:        map[string]store.LineageContextTaskRecord{},
		Requirements: map[string]store.LineageContextRequirementRecord{},
	}
	if len(taskIDs) > 0 {
		ids := make([]string, 0, len(taskIDs))
		for id := range taskIDs {
			ids = append(ids, id)
		}
		rows, err := s.boundary.Query(ctx, `SELECT t.id,t.title,t.state,COALESCE(t.parent_task_id,''),review.payload_json
			FROM tasks t
			LEFT JOIN LATERAL (
				SELECT e.payload_json FROM events e
				WHERE e.workspace_id=t.workspace_id AND e.task_id=t.id
					AND t.state IN ('merged','closed')
					AND e.kind IN ('review.completed','review.round_completed')
					AND (jsonb_typeof(e.payload_json)='object' OR e.payload_json='null'::jsonb)
				ORDER BY e.at DESC, e.id DESC LIMIT 1
			) review ON TRUE
			WHERE t.workspace_id=$1 AND t.id=ANY($2)`, workspace(ctx), ids)
		if err != nil {
			return store.LineageContextRecords{}, err
		}
		for rows.Next() {
			var id, title, state, parentTaskID string
			var reviewPayload []byte
			if err = rows.Scan(&id, &title, &state, &parentTaskID, &reviewPayload); err != nil {
				rows.Close()
				return store.LineageContextRecords{}, err
			}
			result.Tasks[id] = store.LineageContextTaskRecord{
				Title: title, State: core.TaskState(state), ParentTaskID: parentTaskID,
				ReviewPayload: append(json.RawMessage(nil), reviewPayload...),
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return store.LineageContextRecords{}, err
		}
		rows.Close()
	}
	if len(requirementIDs) > 0 {
		ids := make([]string, 0, len(requirementIDs))
		for id := range requirementIDs {
			ids = append(ids, id)
		}
		rows, err := s.boundary.Query(ctx, `SELECT r.id,r.title,v.version,v.content,v.statements_json,r.archived_at IS NOT NULL
			FROM requirements r
			JOIN requirement_versions v ON v.workspace_id=r.workspace_id
				AND v.requirement_id=r.id AND v.version=r.current_version AND v.confirmed
			WHERE r.workspace_id=$1 AND r.id=ANY($2) AND r.current_version IS NOT NULL`, workspace(ctx), ids)
		if err != nil {
			return store.LineageContextRecords{}, err
		}
		for rows.Next() {
			var id, title, content string
			var version int
			var statementsJSON []byte
			var archived bool
			if err = rows.Scan(&id, &title, &version, &content, &statementsJSON, &archived); err != nil {
				rows.Close()
				return store.LineageContextRecords{}, err
			}
			var statements []core.RequirementStatement
			if err = json.Unmarshal(statementsJSON, &statements); err != nil {
				rows.Close()
				return store.LineageContextRecords{}, fmt.Errorf("decode requirement %s v%d statements: %w", id, version, err)
			}
			result.Requirements[id] = store.LineageContextRequirementRecord{
				Title: title, Version: version, Content: content, Statements: statements, Archived: archived,
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return store.LineageContextRecords{}, err
		}
		rows.Close()
	}
	return result, nil
}

func lineageRecordBaseID(node core.LineageNode) string {
	if node.Type != core.LineageBlueprintVersion && node.Type != core.LineageRequirementVersion && node.Type != core.LineageReferenceDocumentVersion && node.Type != core.LineageSystemDesignVersion {
		return node.ID
	}
	index := strings.LastIndex(node.ID, ":v")
	if index <= 0 {
		return node.ID
	}
	return node.ID[:index]
}

func lineageRecordIDs(nodes map[string][]core.LineageNode) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	return ids
}

func (s *Store) queryLineageTaskRecords(ctx context.Context, nodes map[string][]core.LineageNode, records map[core.LineageNode]store.LineageNodeRecord) error {
	if len(nodes) == 0 {
		return nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT id,title FROM tasks WHERE workspace_id=$1 AND id=ANY($2)`, workspace(ctx), lineageRecordIDs(nodes))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, title string
		if err = rows.Scan(&id, &title); err != nil {
			return err
		}
		for _, node := range nodes[id] {
			records[node] = store.LineageNodeRecord{Title: title}
		}
	}
	return rows.Err()
}

func (s *Store) queryLineageRequirementRecords(ctx context.Context, nodes map[string][]core.LineageNode, records map[core.LineageNode]store.LineageNodeRecord) error {
	if len(nodes) == 0 {
		return nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT id,title,slug FROM requirements WHERE workspace_id=$1 AND id=ANY($2)`, workspace(ctx), lineageRecordIDs(nodes))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, slug string
		if err = rows.Scan(&id, &title, &slug); err != nil {
			return err
		}
		for _, node := range nodes[id] {
			records[node] = store.LineageNodeRecord{Title: title, Slug: slug}
		}
	}
	return rows.Err()
}

func (s *Store) queryLineageSessionRecords(ctx context.Context, nodes map[string][]core.LineageNode, records map[core.LineageNode]store.LineageNodeRecord) error {
	if len(nodes) == 0 {
		return nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT id,title FROM planning_sessions WHERE workspace_id=$1 AND id=ANY($2)`, workspace(ctx), lineageRecordIDs(nodes))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, title string
		if err = rows.Scan(&id, &title); err != nil {
			return err
		}
		for _, node := range nodes[id] {
			records[node] = store.LineageNodeRecord{Title: title}
		}
	}
	return rows.Err()
}

func (s *Store) queryLineageOrderRecords(ctx context.Context, nodes map[string][]core.LineageNode, records map[core.LineageNode]store.LineageNodeRecord) error {
	if len(nodes) == 0 {
		return nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT id,task_id,stage FROM work_orders WHERE workspace_id=$1 AND id=ANY($2)`, workspace(ctx), lineageRecordIDs(nodes))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, taskID string
		var stage core.Stage
		if err = rows.Scan(&id, &taskID, &stage); err != nil {
			return err
		}
		for _, node := range nodes[id] {
			records[node] = store.LineageNodeRecord{TaskID: taskID, Stage: stage}
		}
	}
	return rows.Err()
}

func (s *Store) ListRequirementDeliveryEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	result := make(map[string][]core.Event, len(taskIDs))
	if len(taskIDs) == 0 {
		return result, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT e.id,e.task_id,e.job_id,e.kind,e.actor_id,e.actor_role,e.payload_json,e.at,e.workspace_id
		FROM events e JOIN tasks t ON t.id=e.task_id
		WHERE t.workspace_id=$1 AND e.task_id=ANY($2::text[]) AND e.kind IN (
			'merge.confirmed','merge.reconciled','review.round_completed',
			'task.context_requirement_added','task.context_requirement_activated','task.context_requirement_removed'
		)
		ORDER BY e.task_id,e.at,e.id`, workspace(ctx), taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var event db.Event
		if err := rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID,
			&event.ActorRole, &event.PayloadJson, &event.At, &event.WorkspaceID); err != nil {
			return nil, err
		}
		converted := eventFromDB(event)
		result[converted.TaskID] = append(result[converted.TaskID], converted)
	}
	return result, rows.Err()
}

func (s *Store) ListLineageLinks(ctx context.Context) ([]core.LineageLink, error) {
	rows, err := s.queries.ListLineageLinks(ctx, workspace(ctx))
	if err != nil {
		return nil, err
	}
	links := make([]core.LineageLink, 0, len(rows))
	for _, row := range rows {
		links = append(links, core.LineageLink{
			Workspace: row.WorkspaceID, SrcType: core.LineageNodeType(row.SrcType), SrcID: row.SrcID,
			DstType: core.LineageNodeType(row.DstType), DstID: row.DstID, Kind: row.Kind,
			CreatedByEventID: row.CreatedByEventID.Int64, LegacyCreatedByEvent: row.LegacyCreatedByEvent.String, CreatedAt: row.CreatedAt.Time,
		})
	}
	return links, nil
}

func (s *Store) LineageNodeExists(ctx context.Context, node core.LineageNode) (bool, error) {
	if !node.Valid() {
		return false, nil
	}
	var exists bool
	err := s.boundary.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM links WHERE workspace_id=$1 AND src_type=$2 AND src_id=$3
		UNION ALL
		SELECT 1 FROM links WHERE workspace_id=$1 AND dst_type=$2 AND dst_id=$3
		LIMIT 1
	)`, workspace(ctx), string(node.Type), node.ID).Scan(&exists)
	return exists, err
}

// lineageRelationRankSQL is core.LineageRelationPriority rendered for the
// neighbor query, which cannot call Go. TestLineageRelationRankSQLMatchesCore
// fails when the two disagree (component-lineage).
var lineageRelationRankSQL = core.LineageRelationRankSQL("n.kind")

// lineageNeighborPrefixSQL returns, for every requested node, its first $4
// valid incident links in the walk's ranking: relation, link time, event ID,
// then the far endpoint and kind in byte order. The source side probes the
// primary key and the destination side links_destination_idx, so a hub costs
// at most the limit in returned rows (component-lineage).
var lineageNeighborPrefixSQL = `SELECT f.ord::int,l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,l.legacy_created_by_event,l.created_at,l.created_by_event_id
	FROM unnest($2::text[],$3::text[]) WITH ORDINALITY AS f(node_type,node_id,ord)
	CROSS JOIN LATERAL (
		SELECT n.workspace_id,n.src_type,n.src_id,n.dst_type,n.dst_id,n.kind,n.legacy_created_by_event,n.created_at,n.created_by_event_id
		FROM (
			SELECT s.workspace_id,s.src_type,s.src_id,s.dst_type,s.dst_id,s.kind,s.legacy_created_by_event,s.created_at,s.created_by_event_id,s.dst_type AS far_type,s.dst_id AS far_id
			FROM links s WHERE s.workspace_id=$1 AND s.src_type=f.node_type AND s.src_id=f.node_id
			UNION ALL
			SELECT d.workspace_id,d.src_type,d.src_id,d.dst_type,d.dst_id,d.kind,d.legacy_created_by_event,d.created_at,d.created_by_event_id,d.src_type,d.src_id
			FROM links d WHERE d.workspace_id=$1 AND d.dst_type=f.node_type AND d.dst_id=f.node_id
		) n
		WHERE btrim(n.src_id,E' \t\n\r\v\f')<>'' AND btrim(n.dst_id,E' \t\n\r\v\f')<>'' AND btrim(n.kind,E' \t\n\r\v\f')<>''
			AND n.src_type=ANY($5::text[]) AND n.dst_type=ANY($5::text[])
			AND NOT (n.src_type=n.dst_type AND n.src_id=n.dst_id)
			AND (COALESCE(n.created_by_event_id,0)>0 OR (n.kind='historical_feature_assignment' AND btrim(COALESCE(n.legacy_created_by_event,''),E' \t\n\r\v\f')<>''))
		ORDER BY ` + lineageRelationRankSQL + `,n.created_at,COALESCE(n.created_by_event_id,0),n.far_type COLLATE "C",n.far_id COLLATE "C",n.kind COLLATE "C"
		LIMIT $4
	) l`

// ListLineageNeighborhood runs the shared bounded walk and reads each depth's
// ranked link prefixes with one query, so returned rows follow the budget
// rather than the workspace graph (component-lineage).
func (s *Store) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	if budget.MaxDepth < 0 || budget.MaxNodes <= 0 {
		return nil, fmt.Errorf("lineage neighborhood requires bounded depth and nodes")
	}
	budget.Workspace = workspace(ctx)
	nodeTypes := make([]string, 0, len(core.LineageNodeTypes()))
	for _, nodeType := range core.LineageNodeTypes() {
		nodeTypes = append(nodeTypes, string(nodeType))
	}
	return core.FetchLineageNeighborhood(ctx, roots, budget, func(ctx context.Context, nodes []core.LineageNode, limit int) (map[core.LineageNode][]core.LineageLink, error) {
		types, ids := make([]string, len(nodes)), make([]string, len(nodes))
		for index, node := range nodes {
			types[index], ids[index] = string(node.Type), node.ID
		}
		rows, err := s.boundary.Query(ctx, lineageNeighborPrefixSQL, workspace(ctx), types, ids, limit, nodeTypes)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := make(map[core.LineageNode][]core.LineageLink, len(nodes))
		for rows.Next() {
			var ordinal int
			var link core.LineageLink
			var legacy pgtype.Text
			var createdAt pgtype.Timestamptz
			var eventID pgtype.Int8
			if err = rows.Scan(&ordinal, &link.Workspace, &link.SrcType, &link.SrcID, &link.DstType, &link.DstID, &link.Kind, &legacy, &createdAt, &eventID); err != nil {
				return nil, err
			}
			if ordinal < 1 || ordinal > len(nodes) {
				return nil, fmt.Errorf("lineage neighbor row names request %d of %d", ordinal, len(nodes))
			}
			link.LegacyCreatedByEvent, link.CreatedByEventID, link.CreatedAt = legacy.String, eventID.Int64, createdAt.Time
			node := nodes[ordinal-1]
			out[node] = append(out[node], link)
		}
		return out, rows.Err()
	})
}

func (s *Store) ListRequirementDeliveryLineage(ctx context.Context, requirementID string, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	if strings.TrimSpace(requirementID) == "" || budget.MaxDepth < 0 || budget.MaxNodes <= 0 || budget.MaxLinks <= 0 {
		return nil, fmt.Errorf("requirement delivery lineage requires a requirement id and bounded depth, nodes, and links")
	}
	rows, err := s.boundary.Query(ctx, `WITH RECURSIVE walk(node_type,node_id,depth) AS (
		SELECT 'requirement'::text,$2::text,0
		UNION
		SELECT l.dst_type,l.dst_id,w.depth+1
		FROM walk w JOIN links l ON l.workspace_id=$1 AND l.src_type=w.node_type AND l.src_id=w.node_id
		WHERE w.depth < $3 AND (
			(l.kind='serves' AND l.src_type='requirement' AND l.dst_type IN ('task','blueprint')) OR
			(l.kind='versions' AND l.src_type='blueprint' AND l.dst_type='blueprint_version') OR
			(l.kind='materializes' AND l.src_type='blueprint_version' AND l.dst_type='task')
		)
	), chosen AS (
		SELECT min(w.depth) AS src_depth,l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,l.legacy_created_by_event,l.created_at,l.created_by_event_id
		FROM walk w JOIN links l ON l.workspace_id=$1 AND l.src_type=w.node_type AND l.src_id=w.node_id
		WHERE w.depth < $3 AND (
			(l.kind='serves' AND l.src_type='requirement' AND l.dst_type IN ('task','blueprint')) OR
			(l.kind='versions' AND l.src_type='blueprint' AND l.dst_type='blueprint_version') OR
			(l.kind='materializes' AND l.src_type='blueprint_version' AND l.dst_type='task')
		)
		GROUP BY l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,l.legacy_created_by_event,l.created_at,l.created_by_event_id
	)
	SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at,created_by_event_id
	FROM chosen ORDER BY src_depth,src_type,src_id,dst_type,dst_id,kind LIMIT $4`, workspace(ctx), requirementID, budget.MaxDepth, budget.MaxLinks+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]core.LineageLink, 0, budget.MaxLinks+1)
	for rows.Next() {
		var link core.LineageLink
		var legacy pgtype.Text
		var createdAt pgtype.Timestamptz
		var eventID pgtype.Int8
		if err = rows.Scan(&link.Workspace, &link.SrcType, &link.SrcID, &link.DstType, &link.DstID, &link.Kind, &legacy, &createdAt, &eventID); err != nil {
			return nil, err
		}
		link.LegacyCreatedByEvent, link.CreatedByEventID, link.CreatedAt = legacy.String, eventID.Int64, createdAt.Time
		result = append(result, link)
	}
	return result, rows.Err()
}

func (s *Store) ListRequirementDeliveryLineageByRequirement(ctx context.Context, requirementIDs []string, budget core.LineageTraversalBudget) (map[string][]core.LineageLink, error) {
	if len(requirementIDs) == 0 {
		return map[string][]core.LineageLink{}, nil
	}
	if budget.MaxDepth < 0 || budget.MaxNodes <= 0 || budget.MaxLinks <= 0 {
		return nil, fmt.Errorf("requirement delivery lineage requires bounded depth, nodes, and links")
	}
	rows, err := s.boundary.Query(ctx, `WITH RECURSIVE walk(requirement_id,node_type,node_id,depth) AS (
		SELECT requirement_id,'requirement'::text,requirement_id,0
		FROM unnest($2::text[]) AS seed(requirement_id)
		UNION
		SELECT w.requirement_id,l.dst_type,l.dst_id,w.depth+1
		FROM walk w JOIN links l ON l.workspace_id=$1 AND l.src_type=w.node_type AND l.src_id=w.node_id
		WHERE w.depth < $3 AND (
			(l.kind='serves' AND l.src_type='requirement' AND l.dst_type IN ('task','blueprint')) OR
			(l.kind='versions' AND l.src_type='blueprint' AND l.dst_type='blueprint_version') OR
			(l.kind='materializes' AND l.src_type='blueprint_version' AND l.dst_type='task')
		)
	), chosen AS (
		SELECT w.requirement_id,min(w.depth) AS src_depth,l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,l.legacy_created_by_event,l.created_at,l.created_by_event_id
		FROM walk w JOIN links l ON l.workspace_id=$1 AND l.src_type=w.node_type AND l.src_id=w.node_id
		WHERE w.depth < $3 AND (
			(l.kind='serves' AND l.src_type='requirement' AND l.dst_type IN ('task','blueprint')) OR
			(l.kind='versions' AND l.src_type='blueprint' AND l.dst_type='blueprint_version') OR
			(l.kind='materializes' AND l.src_type='blueprint_version' AND l.dst_type='task')
		)
		GROUP BY w.requirement_id,l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,l.legacy_created_by_event,l.created_at,l.created_by_event_id
	), ranked AS (
		SELECT *,row_number() OVER (PARTITION BY requirement_id ORDER BY src_depth,src_type,src_id,dst_type,dst_id,kind) AS position
		FROM chosen
	)
	SELECT requirement_id,workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at,created_by_event_id
	FROM ranked WHERE position <= $4 ORDER BY requirement_id,position`, workspace(ctx), requirementIDs, budget.MaxDepth, budget.MaxLinks+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]core.LineageLink, len(requirementIDs))
	for rows.Next() {
		var requirementID string
		var link core.LineageLink
		var legacy pgtype.Text
		var createdAt pgtype.Timestamptz
		var eventID pgtype.Int8
		if err = rows.Scan(&requirementID, &link.Workspace, &link.SrcType, &link.SrcID, &link.DstType, &link.DstID, &link.Kind, &legacy, &createdAt, &eventID); err != nil {
			return nil, err
		}
		link.LegacyCreatedByEvent, link.CreatedByEventID, link.CreatedAt = legacy.String, eventID.Int64, createdAt.Time
		out[requirementID] = append(out[requirementID], link)
	}
	return out, rows.Err()
}

func (s *Store) RebuildLineage(ctx context.Context, request core.LineageRebuildRequest) (core.LineageRebuildResult, error) {
	var result core.LineageRebuildResult
	if strings.TrimSpace(request.Reason) == "" || strings.TrimSpace(request.RequestID) == "" {
		return result, fmt.Errorf("%w: reason and request_id are required", store.ErrLineageRebuildValidation)
	}
	err := s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		var prior []byte
		err := q.QueryRow(ctx, `SELECT payload_json FROM events WHERE workspace_id=$1 AND kind='lineage.rebuilt'
			AND payload_json->>'request_id'=$2 ORDER BY id DESC LIMIT 1`, workspace(ctx), request.RequestID).Scan(&prior)
		if err == nil {
			var payload struct {
				Reason string                    `json:"reason"`
				Result core.LineageRebuildResult `json:"result"`
			}
			if json.Unmarshal(prior, &payload) == nil {
				if payload.Reason != request.Reason {
					return fmt.Errorf("%w: request_id %s was already used for a different reason", store.ErrLineageRebuildConflict, request.RequestID)
				}
				result = payload.Result
				return nil
			}
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		events, err := q.ListWorkspaceEvents(ctx, workspace(ctx))
		if err != nil {
			return err
		}
		key := func(link core.LineageLink) string {
			return strings.Join([]string{link.Workspace, string(link.SrcType), link.SrcID, string(link.DstType), link.DstID, link.Kind}, "\x00")
		}
		replayable := map[string]core.LineageLink{}
		suppressed := map[string]struct{}{}
		candidateEvent := map[string]int64{}
		ambiguous := map[string]struct{}{}
		replay := store.NewLineageReplay()
		for _, row := range events {
			event := eventFromDB(row)
			projection := replay.Project(workspace(ctx), event)
			links := projection.Links
			if requirementID, version, historical := store.HistoricalRequirementConfirmation(event); historical {
				var predecessor pgtype.Int4
				if err = q.QueryRow(ctx, `SELECT max(version) FROM requirement_versions
					WHERE workspace_id=$1 AND requirement_id=$2 AND confirmed=true AND version<$3`,
					workspace(ctx), requirementID, version).Scan(&predecessor); err != nil {
					return err
				}
				predecessorVersion := 0
				if predecessor.Valid {
					predecessorVersion = int(predecessor.Int32)
				}
				links = store.LineageLinksForHistoricalConfirmation(workspace(ctx), event, predecessorVersion)
			}
			result.Unsupported += projection.Unsupported
			for _, link := range projection.Suppresses {
				linkKey := key(link)
				delete(replayable, linkKey)
				delete(candidateEvent, linkKey)
				delete(ambiguous, linkKey)
				suppressed[linkKey] = struct{}{}
			}
			for _, link := range links {
				linkKey := key(link)
				delete(suppressed, linkKey)
				if first, ok := candidateEvent[linkKey]; ok && first != link.CreatedByEventID {
					ambiguous[linkKey] = struct{}{}
				} else if !ok {
					candidateEvent[linkKey] = link.CreatedByEventID
				}
				if prior, ok := replayable[linkKey]; !ok || link.CreatedByEventID < prior.CreatedByEventID {
					replayable[linkKey] = link
				}
			}
		}
		stored, err := q.ListLineageLinks(ctx, workspace(ctx))
		if err != nil {
			return err
		}
		canonical := store.CanonicalLineageKinds()
		preserved := map[string]core.LineageLink{}
		for _, row := range stored {
			link := core.LineageLink{
				Workspace: row.WorkspaceID, SrcType: core.LineageNodeType(row.SrcType), SrcID: row.SrcID,
				DstType: core.LineageNodeType(row.DstType), DstID: row.DstID, Kind: row.Kind,
				CreatedByEventID: row.CreatedByEventID.Int64, LegacyCreatedByEvent: row.LegacyCreatedByEvent.String,
				CreatedAt: row.CreatedAt.Time,
			}
			_, owned := canonical[link.Kind]
			if !row.CreatedByEventID.Valid || !owned {
				if _, regenerated := replayable[key(link)]; !regenerated {
					result.Existing++
				}
				continue
			}
			linkKey := key(link)
			_, isSuppressed := suppressed[linkKey]
			if _, regenerable := replayable[linkKey]; !regenerable && !isSuppressed {
				preserved[linkKey] = link
			}
		}
		if _, err = q.DeleteLineageLinks(ctx, workspace(ctx)); err != nil {
			return err
		}
		insert := func(link core.LineageLink) error {
			return q.InsertLineageLink(ctx, db.InsertLineageLinkParams{
				WorkspaceID: link.Workspace, SrcType: string(link.SrcType), SrcID: link.SrcID,
				DstType: string(link.DstType), DstID: link.DstID, Kind: link.Kind,
				CreatedByEventID: link.CreatedByEventID, CreatedAt: timestamp(link.CreatedAt),
			})
		}
		for _, link := range replayable {
			if err = insert(link); err != nil {
				return err
			}
		}
		for _, link := range preserved {
			if err = insert(link); err != nil {
				return err
			}
		}
		result.Projected = len(replayable)
		result.PreservedUnregenerable = len(preserved)
		result.Ambiguous = len(ambiguous)
		actor := store.ActorFromContext(ctx)
		_, err = insertWorkspaceEventRow(ctx, q, db.InsertWorkspaceEventParams{
			WorkspaceID: workspace(ctx), Kind: "lineage.rebuilt", ActorID: actor.ID, ActorRole: string(actor.Role),
			PayloadJson: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "reason": request.Reason, "request_id": request.RequestID, "result": result}),
			At:          timestamp(time.Now().UTC()),
		})
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}
