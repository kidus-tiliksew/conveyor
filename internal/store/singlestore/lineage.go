package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	return records, nil
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

func (s *Store) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	links, err := s.ListLineageLinks(ctx)
	if err != nil {
		return nil, err
	}
	selected := map[string]core.LineageLink{}
	for _, root := range roots {
		walk, walkErr := core.TraverseLineage(links, []core.LineageNode{root}, core.LineageTraversalBudget{MaxDepth: budget.MaxDepth, MaxNodes: budget.MaxNodes, Workspace: documentWorkspace(ctx)})
		if walkErr != nil {
			return nil, walkErr
		}
		nodes := make(map[core.LineageNode]bool, len(walk.Nodes))
		for _, node := range walk.Nodes {
			nodes[node] = true
		}
		for _, link := range links {
			if nodes[core.LineageNode{Type: link.SrcType, ID: link.SrcID}] || nodes[core.LineageNode{Type: link.DstType, ID: link.DstID}] {
				selected[lineageLinkKey(link)] = link
			}
		}
	}
	result := make([]core.LineageLink, 0, len(selected))
	for _, link := range selected {
		result = append(result, link)
	}
	sort.Slice(result, func(i, j int) bool { return lineageLinkKey(result[i]) < lineageLinkKey(result[j]) })
	return result, nil
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
		for _, row := range events {
			event := row
			projection := store.ProjectLineageEvent(documentWorkspace(ctx), event)
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
		rows, err := documentBatchRows(ctx, s.db, `SELECT task_id,payload_json FROM events WHERE workspace_id=? AND task_id IN (%s) AND kind IN ('review.completed','review.round_completed') ORDER BY id`, documentWorkspace(ctx), ids)
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
		rows, err := documentBatchRows(ctx, s.db, `SELECT r.id,r.title,v.version,v.content,v.statements_json
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
			if err = rows.Scan(&id, &title, &version, &content, &statementsJSON); err != nil {
				rows.Close()
				return store.LineageContextRecords{}, err
			}
			var statements []core.RequirementStatement
			if err = json.Unmarshal(statementsJSON, &statements); err != nil {
				rows.Close()
				return store.LineageContextRecords{}, fmt.Errorf("decode requirement %s v%d statements: %w", id, version, err)
			}
			result.Requirements[id] = store.LineageContextRequirementRecord{
				Title: title, Version: version, Content: content, Statements: statements,
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
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.feature_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='task' AND l.workspace_id=? AND l.task_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.feature_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='requirement' AND l.workspace_id=? AND l.requirement_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.feature_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifact_links l ON w.node_type='planning_session' AND l.workspace_id=? AND l.planning_session_id=w.node_id JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id
	UNION ALL
	SELECT w.ord,a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,l.role,l.task_id,l.feature_id,l.requirement_id,l.planning_session_id
	FROM wanted w JOIN artifacts a ON w.node_type='evidence' AND a.workspace_id=? AND a.id=w.node_id JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id
		AND l.role='verification_evidence' AND l.task_id IS NOT NULL AND l.feature_id IS NULL
		AND ((a.content_type IN ('image/png','image/jpeg','image/webp') AND a.size_bytes BETWEEN 1 AND 10485760)
			OR (a.content_type IN ('video/mp4','video/webm') AND a.size_bytes BETWEEN 1 AND 26214400))
), dedup AS (
	SELECT id,workspace_id,name,content_type,size_bytes,created_at,role,task_id,feature_id,requirement_id,planning_session_id,min(ord) AS ord
	FROM matched GROUP BY id,workspace_id,name,content_type,size_bytes,created_at,role,task_id,feature_id,requirement_id,planning_session_id
)
SELECT id,workspace_id,name,content_type,size_bytes,created_at,role,
	COALESCE(task_id,''),COALESCE(feature_id,''),COALESCE(requirement_id,''),COALESCE(planning_session_id,'')
FROM dedup ORDER BY ord,created_at,id,role`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Artifact{}
	for rows.Next() {
		var a core.Artifact
		if err = rows.Scan(&a.ID, &a.Workspace, &a.Name, &a.ContentType, &a.SizeBytes, &a.CreatedAt, &a.Role, &a.TaskID, &a.FeatureID, &a.RequirementID, &a.PlanningSessionID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
