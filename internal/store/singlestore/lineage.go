package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) LineageNodeExists(ctx context.Context, node core.LineageNode) (bool, error) {
	if !node.Valid() {
		return false, nil
	}
	var exists bool
	err := documentRow(ctx, s.db, `SELECT EXISTS (
		SELECT 1 FROM links WHERE workspace_id=? AND
		((src_type=? AND src_id=?) OR (dst_type=? AND dst_id=?))
	)`, documentWorkspace(ctx), string(node.Type), node.ID, string(node.Type), node.ID).Scan(&exists)
	return exists, err
}

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
		rows, queryErr := documentBatchRows(ctx, s.db, `SELECT id,name FROM reference_documents WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), lineageRecordIDs(referenceNodes))
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
		rows, queryErr := documentBatchRows(ctx, s.db, `SELECT id,title,slug FROM system_designs WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), lineageRecordIDs(designNodes))
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
		rows, queryErr := documentBatchRows(ctx, s.db, `SELECT id,statement FROM decisions WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), lineageRecordIDs(decisionNodes))
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
	if err := s.lineageStatusQuery(ctx, `SELECT id,title FROM planning_bundles WHERE workspace_id=? AND id IN (%s)`, lineageRecordIDs(bundleNodes), func(rows *documentResultSet) error {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return err
		}
		for _, node := range bundleNodes[id] {
			records[node] = store.LineageNodeRecord{Title: title}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return records, nil
}

// lineageStatusQuery runs one workspace-scoped batch read over ids.
func (s *Store) lineageStatusQuery(ctx context.Context, query string, ids []string, scan func(*documentResultSet) error) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := documentBatchRows(ctx, s.db, query, documentWorkspace(ctx), ids)
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
		{core.LineageTask, `SELECT id,state FROM tasks WHERE workspace_id=? AND id IN (%s)`, rows.TaskStates},
		{core.LineageWorkOrder, `SELECT id,state FROM work_orders WHERE workspace_id=? AND id IN (%s)`, rows.OrderStates},
		{core.LineageDecision, `SELECT id,status FROM decisions WHERE workspace_id=? AND id IN (%s)`, rows.DecisionState},
		{core.LineagePlanningSession, `SELECT id,status FROM planning_sessions WHERE workspace_id=? AND id IN (%s)`, rows.SessionState},
		{core.LineagePlanningBundle, `SELECT id,status FROM planning_bundles WHERE workspace_id=? AND id IN (%s)`, rows.BundleState},
	}
	for _, state := range states {
		into := state.into
		if err := s.lineageStatusQuery(ctx, state.query, ids[state.class], func(r *documentResultSet) error {
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
		{core.LineageRequirement, `SELECT id,COALESCE(current_version,0),archived_at IS NOT NULL FROM requirements WHERE workspace_id=? AND id IN (%s)`,
			`SELECT v.requirement_id,v.version,v.confirmed,v.retired FROM (%s) p JOIN requirement_versions v ON v.workspace_id=? AND v.requirement_id=p.id AND v.version=p.version`},
		{core.LineageSystemDesign, `SELECT id,COALESCE(current_version,0),archived_at IS NOT NULL FROM system_designs WHERE workspace_id=? AND id IN (%s)`,
			`SELECT v.document_id,v.version,v.confirmed,v.dismissed FROM (%s) p JOIN system_design_versions v ON v.workspace_id=? AND v.document_id=p.id AND v.version=p.version`},
		{core.LineageReferenceDocument, `SELECT id,COALESCE(current_version,0),deleted_at IS NOT NULL FROM reference_documents WHERE workspace_id=? AND id IN (%s)`, ""},
	}
	for _, document := range documents {
		byID := map[string]store.LineageStatusDocument{}
		rows.Documents[document.class] = byID
		if err := s.lineageStatusQuery(ctx, document.document, ids[document.class], func(r *documentResultSet) error {
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
		requested := make([]string, len(pairs))
		args := make([]any, 0, 2*len(pairs)+1)
		for index, pair := range pairs {
			requested[index] = "SELECT ? AS id, ? AS version"
			args = append(args, pair.DocumentID, pair.Version)
		}
		args = append(args, documentWorkspace(ctx))
		versions := map[string]map[int]store.LineageStatusVersion{}
		rows.Versions[document.class] = versions
		versionRows, err := documentRows(ctx, s.db, strings.Replace(document.versions, "%s", strings.Join(requested, " UNION ALL "), 1), args...)
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
	if err := s.lineageStatusQuery(ctx, `SELECT wo.id,e.payload_json FROM work_orders wo
		JOIN events e ON e.workspace_id=wo.workspace_id AND e.task_id=wo.task_id AND e.kind='review.completed'
		WHERE wo.workspace_id=? AND wo.id IN (%s) ORDER BY e.id`, ids[core.LineageVerdict], func(r *documentResultSet) error {
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

// ListLineageKindLinks reads one kind at the named nodes through the links
// primary key (outbound) or links_destination_idx (inbound), in
// core.SortLineageKindLinks order under the table's binary collation
// (component-lineage).
func (s *Store) ListLineageKindLinks(ctx context.Context, query core.LineageKindLinkQuery) ([]core.LineageLink, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	near, far := "src", "dst"
	if query.Inbound {
		near, far = "dst", "src"
	}
	// Newest orders descending under the limit; ascending order is restored
	// below.
	order := ""
	if query.Newest {
		order = " DESC"
	}
	seen := map[core.LineageNode]bool{}
	requested := []string{}
	args := []any{}
	for _, node := range query.Nodes {
		node = core.LineageNode{Type: node.Type, ID: node.ID}
		if seen[node] {
			continue
		}
		seen[node] = true
		requested = append(requested, "SELECT ? AS node_type, ? AS node_id")
		args = append(args, string(node.Type), node.ID)
	}
	args = append(args, documentWorkspace(ctx), query.Kind, string(query.FarType), query.Limit)
	rows, err := documentRows(ctx, s.db, `SELECT l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,COALESCE(l.legacy_created_by_event,''),l.created_at,COALESCE(l.created_by_event_id,0)
		FROM (`+strings.Join(requested, " UNION ALL ")+`) q
		JOIN links l ON l.workspace_id=? AND l.`+near+`_type=q.node_type AND l.`+near+`_id=q.node_id
		WHERE l.kind=? AND l.`+far+`_type=? AND TRIM(l.`+far+`_id)<>''
			AND NOT (l.src_type=l.dst_type AND l.src_id=l.dst_id)
			AND (COALESCE(l.created_by_event_id,0)>0 OR (l.kind='historical_feature_assignment' AND TRIM(COALESCE(l.legacy_created_by_event,''))<>''))
		ORDER BY COALESCE(l.created_by_event_id,0)`+order+`,l.src_type`+order+`,l.src_id`+order+`,l.dst_type`+order+`,l.dst_id`+order+`,l.kind`+order+`
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.LineageLink{}
	for rows.Next() {
		var link core.LineageLink
		if err = rows.Scan(&link.Workspace, &link.SrcType, &link.SrcID, &link.DstType, &link.DstID, &link.Kind, &link.LegacyCreatedByEvent, &link.CreatedAt, &link.CreatedByEventID); err != nil {
			return nil, err
		}
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
	rows, err := documentBatchRows(ctx, s.db, `SELECT id,title FROM tasks WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), lineageRecordIDs(nodes))
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
	rows, err := documentBatchRows(ctx, s.db, `SELECT id,title,slug FROM requirements WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), lineageRecordIDs(nodes))
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
	rows, err := documentBatchRows(ctx, s.db, `SELECT id,title FROM planning_sessions WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), lineageRecordIDs(nodes))
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
	rows, err := documentBatchRows(ctx, s.db, `SELECT id,task_id,stage FROM work_orders WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), lineageRecordIDs(nodes))
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

// lineageRelationRankSQL is core.LineageRelationPriority rendered for the
// neighbor query (component-lineage).
var lineageRelationRankSQL = core.LineageRelationRankSQL("x.kind")

// lineageNeighborPrefixQuery selects, for every requested node, its first
// limit valid incident links in the walk's ranking (component-lineage).
func lineageNeighborPrefixQuery(ws string, nodes []core.LineageNode, limit int) (string, []any) {
	typeMarks := make([]string, 0, len(core.LineageNodeTypes()))
	var typeArgs []any
	for _, nodeType := range core.LineageNodeTypes() {
		typeMarks = append(typeMarks, "?")
		typeArgs = append(typeArgs, string(nodeType))
	}
	typeList := strings.Join(typeMarks, ",")
	requested := make([]string, len(nodes))
	var requestArgs []any
	for index, node := range nodes {
		requested[index] = "SELECT ? AS ord, ? AS node_type, ? AS node_id"
		requestArgs = append(requestArgs, index+1, string(node.Type), node.ID)
	}
	request := strings.Join(requested, " UNION ALL ")
	args := append([]any{}, requestArgs...)
	args = append(args, ws)
	args = append(args, requestArgs...)
	args = append(args, ws)
	args = append(args, typeArgs...)
	args = append(args, typeArgs...)
	args = append(args, limit)
	return `SELECT y.ord,y.workspace_id,y.src_type,y.src_id,y.dst_type,y.dst_id,y.kind,y.legacy,y.created_at,y.event_id FROM (
		SELECT x.*,ROW_NUMBER() OVER (PARTITION BY x.ord ORDER BY ` + lineageRelationRankSQL + `,x.created_at,x.event_id,x.far_type,x.far_id,x.kind) AS rn FROM (
			SELECT q.ord,l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,COALESCE(l.legacy_created_by_event,'') AS legacy,l.created_at,COALESCE(l.created_by_event_id,0) AS event_id,l.dst_type AS far_type,l.dst_id AS far_id
			FROM (` + request + `) q JOIN links l ON l.workspace_id=? AND l.src_type=q.node_type AND l.src_id=q.node_id
			UNION ALL
			SELECT q.ord,l.workspace_id,l.src_type,l.src_id,l.dst_type,l.dst_id,l.kind,COALESCE(l.legacy_created_by_event,''),l.created_at,COALESCE(l.created_by_event_id,0),l.src_type,l.src_id
			FROM (` + request + `) q JOIN links l ON l.workspace_id=? AND l.dst_type=q.node_type AND l.dst_id=q.node_id
		) x
		WHERE TRIM(x.src_id)<>'' AND TRIM(x.dst_id)<>'' AND TRIM(x.kind)<>''
			AND x.src_type IN (` + typeList + `) AND x.dst_type IN (` + typeList + `)
			AND NOT (x.src_type=x.dst_type AND x.src_id=x.dst_id)
			AND (x.event_id>0 OR (x.kind='historical_feature_assignment' AND TRIM(x.legacy)<>''))
	) y WHERE y.rn<=?`, args
}

// ListLineageNeighborhood runs the shared bounded walk and reads each depth's
// ranked link prefixes with one query. The source side probes the primary key
// and the destination side links_destination_idx; ROW_NUMBER keeps each
// node's first limit links in the walk's ranking under the table's binary
// collation, so returned rows follow the budget (component-lineage).
func (s *Store) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	if budget.MaxDepth < 0 || budget.MaxNodes <= 0 {
		return nil, fmt.Errorf("lineage neighborhood requires bounded depth and nodes")
	}
	budget.Workspace = documentWorkspace(ctx)
	return core.FetchLineageNeighborhood(ctx, roots, budget, func(ctx context.Context, nodes []core.LineageNode, limit int) (map[core.LineageNode][]core.LineageLink, error) {
		query, args := lineageNeighborPrefixQuery(documentWorkspace(ctx), nodes, limit)
		rows, err := documentRows(ctx, s.db, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := make(map[core.LineageNode][]core.LineageLink, len(nodes))
		for rows.Next() {
			var ordinal int
			var link core.LineageLink
			if err = rows.Scan(&ordinal, &link.Workspace, &link.SrcType, &link.SrcID, &link.DstType, &link.DstID, &link.Kind, &link.LegacyCreatedByEvent, &link.CreatedAt, &link.CreatedByEventID); err != nil {
				return nil, err
			}
			if ordinal < 1 || ordinal > len(nodes) {
				return nil, fmt.Errorf("lineage neighbor row names request %d of %d", ordinal, len(nodes))
			}
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
	links, err := s.ListLineageLinks(ctx)
	if err != nil {
		return nil, err
	}
	return boundedRequirementDeliveryLinks(links, requirementID, budget), nil
}

func (s *Store) ListRequirementDeliveryLineageByRequirement(ctx context.Context, requirementIDs []string, budget core.LineageTraversalBudget) (map[string][]core.LineageLink, error) {
	out := make(map[string][]core.LineageLink, len(requirementIDs))
	if len(requirementIDs) == 0 {
		return out, nil
	}
	if budget.MaxDepth < 0 || budget.MaxNodes <= 0 || budget.MaxLinks <= 0 {
		return nil, fmt.Errorf("requirement delivery lineage requires bounded depth, nodes, and links")
	}
	links, err := s.ListLineageLinks(ctx)
	if err != nil {
		return nil, err
	}
	budget.Workspace = documentWorkspace(ctx)
	for _, id := range requirementIDs {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("requirement id required")
		}
		selected := boundedRequirementDeliveryLinks(links, id, budget)
		if len(selected) > 0 {
			out[id] = selected
		}
	}
	return out, nil
}

func boundedRequirementDeliveryLinks(links []core.LineageLink, requirementID string, budget core.LineageTraversalBudget) []core.LineageLink {
	workspace := budget.Workspace
	eligible := make(map[core.LineageNode][]core.LineageLink)
	for _, link := range links {
		if workspace != "" && link.Workspace != workspace || !requirementDeliveryLink(link) {
			continue
		}
		src := core.LineageNode{Type: link.SrcType, ID: link.SrcID}
		eligible[src] = append(eligible[src], link)
	}
	for src := range eligible {
		sort.Slice(eligible[src], func(i, j int) bool { return lineageLinkKey(eligible[src][i]) < lineageLinkKey(eligible[src][j]) })
	}

	type step struct {
		node  core.LineageNode
		depth int
	}
	queue := []step{{node: core.LineageNode{Type: core.LineageRequirement, ID: requirementID}}}
	seen := map[core.LineageNode]bool{queue[0].node: true}
	result := make([]core.LineageLink, 0, min(budget.MaxLinks+1, len(links)))
	for len(queue) > 0 && len(result) <= budget.MaxLinks {
		current := queue[0]
		queue = queue[1:]
		if current.depth >= budget.MaxDepth {
			continue
		}
		for _, link := range eligible[current.node] {
			result = append(result, link)
			if len(result) > budget.MaxLinks {
				break
			}
			dst := core.LineageNode{Type: link.DstType, ID: link.DstID}
			if !seen[dst] {
				seen[dst] = true
				queue = append(queue, step{node: dst, depth: current.depth + 1})
			}
		}
	}
	return result
}

func requirementDeliveryLink(link core.LineageLink) bool {
	return link.Kind == "serves" && link.SrcType == core.LineageRequirement &&
		(link.DstType == core.LineageTask || link.DstType == core.LineageBlueprint) ||
		link.Kind == "versions" && link.SrcType == core.LineageBlueprint && link.DstType == core.LineageBlueprintVersion ||
		link.Kind == "materializes" && link.SrcType == core.LineageBlueprintVersion && link.DstType == core.LineageTask
}

func (s *Store) RebuildLineage(ctx context.Context, request core.LineageRebuildRequest) (core.LineageRebuildResult, error) {
	var result core.LineageRebuildResult
	if strings.TrimSpace(request.Reason) == "" || strings.TrimSpace(request.RequestID) == "" {
		return result, fmt.Errorf("%w: reason and request_id are required", store.ErrLineageRebuildValidation)
	}
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		var prior []byte
		err := documentRow(ctx, tx, `SELECT payload_json FROM events WHERE workspace_id=? AND kind='lineage.rebuilt'
			AND JSON_EXTRACT_STRING(payload_json,'request_id')=? ORDER BY id DESC LIMIT 1`, documentWorkspace(ctx), request.RequestID).Scan(&prior)
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
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		events, err := documentAllEvents(ctx, tx)
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
			event := row
			projection := replay.Project(documentWorkspace(ctx), event)
			links := projection.Links
			if requirementID, version, historical := store.HistoricalRequirementConfirmation(event); historical {
				var predecessor sql.NullInt32
				if err = documentRow(ctx, tx, `SELECT max(version) FROM requirement_versions
					WHERE workspace_id=? AND requirement_id=? AND confirmed=true AND version<?`, documentWorkspace(ctx), requirementID, version).Scan(&predecessor); err != nil {
					return err
				}
				predecessorVersion := 0
				if predecessor.Valid {
					predecessorVersion = int(predecessor.Int32)
				}
				links = store.LineageLinksForHistoricalConfirmation(documentWorkspace(ctx), event, predecessorVersion)
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
		stored, err := documentLineageLinks(ctx, tx)
		if err != nil {
			return err
		}
		canonical := store.CanonicalLineageKinds()
		preserved := map[string]core.LineageLink{}
		for _, row := range stored {
			link := row
			_, owned := canonical[link.Kind]
			if row.CreatedByEventID == 0 || !owned {
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
		if _, err = deleteDocumentLineage(ctx, tx); err != nil {
			return err
		}
		insert := func(link core.LineageLink) error { return insertLineageLink(ctx, tx, link) }
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
		err = insertWorkspaceEvent(ctx, tx, core.Event{Kind: "lineage.rebuilt", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "reason": request.Reason, "request_id": request.RequestID, "result": result})})
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}
func lineageLinkKey(l core.LineageLink) string {
	return strings.Join([]string{l.Workspace, string(l.SrcType), l.SrcID, string(l.DstType), l.DstID, l.Kind}, "\x00")
}
func documentBatchRows(ctx context.Context, db s2log.Executor, query, ws string, ids []string) (*documentResultSet, error) {
	marks := make([]string, len(ids))
	args := []any{ws}
	for i, id := range ids {
		marks[i] = "?"
		args = append(args, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("batch query requires ids")
	}
	return documentRows(ctx, db, strings.Replace(query, "%s", strings.Join(marks, ","), 1), args...)
}
func documentLineageLinks(ctx context.Context, db s2log.Executor) ([]core.LineageLink, error) {
	rows, err := documentRows(ctx, db, `SELECT workspace_id,src_type,src_id,dst_type,dst_id,kind,COALESCE(legacy_created_by_event,''),created_at,COALESCE(created_by_event_id,0) FROM links WHERE workspace_id=? ORDER BY created_by_event_id,src_type,src_id,dst_type,dst_id,kind`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.LineageLink{}
	for rows.Next() {
		var l core.LineageLink
		if err = rows.Scan(&l.Workspace, &l.SrcType, &l.SrcID, &l.DstType, &l.DstID, &l.Kind, &l.LegacyCreatedByEvent, &l.CreatedAt, &l.CreatedByEventID); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (s *Store) ListLineageLinks(ctx context.Context) ([]core.LineageLink, error) {
	return documentLineageLinks(ctx, s.db)
}
func deleteDocumentLineage(ctx context.Context, tx *sql.Tx) (sql.Result, error) {
	kinds := store.CanonicalLineageKinds()
	marks := []string{}
	args := []any{documentWorkspace(ctx)}
	for k := range kinds {
		marks = append(marks, "?")
		args = append(args, k)
	}
	return tx.ExecContext(ctx, `DELETE FROM links WHERE workspace_id=? AND created_by_event_id IS NOT NULL AND kind IN (`+strings.Join(marks, ",")+`)`, args...)
}
func documentAllEvents(ctx context.Context, db s2log.Executor) ([]core.Event, error) {
	rows, err := documentRows(ctx, db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events WHERE workspace_id=? ORDER BY id`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Event{}
	for rows.Next() {
		var e core.Event
		if err = rows.Scan(&e.ID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &e.Payload, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
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
		rows, err := documentBatchRows(ctx, s.db, `SELECT id,title,state,COALESCE(parent_task_id,''),NULL FROM tasks WHERE workspace_id=? AND id IN (%s)`, documentWorkspace(ctx), ids)
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
	if len(taskIDs) > 0 {
		ids := []string{}
		for id := range taskIDs {
			ids = append(ids, id)
		}
		rows, err := documentBatchRows(ctx, s.db, `SELECT task_id,payload_json FROM events WHERE workspace_id=? AND task_id IN (%s) AND kind IN ('review.completed','review.round_completed') ORDER BY task_id,at,id`, documentWorkspace(ctx), ids)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var id string
			var payload []byte
			if err = rows.Scan(&id, &payload); err != nil {
				rows.Close()
				return result, err
			}
			item := result.Tasks[id]
			if !core.TaskTerminal(item.State) {
				continue
			}
			var object map[string]any
			if json.Unmarshal(payload, &object) == nil {
				item.ReviewPayload = append(json.RawMessage(nil), payload...)
				result.Tasks[id] = item
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
	}
	if len(requirementIDs) > 0 {
		ids := make([]string, 0, len(requirementIDs))
		for id := range requirementIDs {
			ids = append(ids, id)
		}
		rows, err := documentBatchRows(ctx, s.db, `SELECT r.id,r.title,v.version,v.content,v.statements_json,r.archived_at IS NOT NULL
			FROM requirements r
			JOIN requirement_versions v ON v.workspace_id=r.workspace_id
				AND v.requirement_id=r.id AND v.version=r.current_version AND v.confirmed
			WHERE r.workspace_id=? AND r.id IN (%s) AND r.current_version IS NOT NULL`, documentWorkspace(ctx), ids)
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

func (s *Store) ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	var seeds []string
	var args []any
	for i, n := range nodes {
		if n.Valid() {
			seeds = append(seeds, "SELECT ? AS node_type,? AS node_id,? AS ord")
			args = append(args, string(n.Type), n.ID, i)
		}
	}
	if len(seeds) == 0 {
		return []core.Artifact{}, nil
	}
	args = append(args, ws, ws, ws, ws)
	rows, err := documentRows(ctx, s.db, "WITH wanted AS ("+strings.Join(seeds, " UNION ALL ")+`), matched AS (
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='task' AND l.workspace_id=? AND l.task_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='requirement' AND l.workspace_id=? AND l.requirement_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='planning_session' AND l.workspace_id=? AND l.planning_session_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifacts a ON w.node_type='evidence' AND a.workspace_id=? AND a.id=w.node_id JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id
		AND l.role='verification_evidence' AND l.task_id IS NOT NULL
		AND ((a.content_type IN ('image/png','image/jpeg','image/webp') AND a.size_bytes BETWEEN 1 AND 10485760)
			OR (a.content_type IN ('video/mp4','video/webm') AND a.size_bytes BETWEEN 1 AND 26214400))
), dedup AS (
	SELECT id,workspace_id,name,content_type,size_bytes,created_at,role,task_id,requirement_id,planning_session_id,min(ord) AS ord
	FROM matched GROUP BY id,workspace_id,name,content_type,size_bytes,created_at,role,task_id,requirement_id,planning_session_id
)
SELECT id,workspace_id,name,content_type,size_bytes,created_at,role,
	COALESCE(task_id,''),COALESCE(requirement_id,''),COALESCE(planning_session_id,'')
FROM dedup ORDER BY ord,created_at,id,role`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Artifact{}
	for rows.Next() {
		var a core.Artifact
		if err = rows.Scan(&a.ID, &a.Workspace, &a.Name, &a.ContentType, &a.SizeBytes, &a.CreatedAt, &a.Role, &a.TaskID, &a.RequirementID, &a.PlanningSessionID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
