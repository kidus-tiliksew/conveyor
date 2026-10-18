package singlestore

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskMapColumns is taskColumns with the body replaced by an empty string,
// so the map read never transfers task bodies (req-board-map AC-3.1).
var taskMapColumns = strings.Replace(taskColumns, ",body,", ",'' AS body,", 1)

func taskMapMarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func taskMapArgs(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func (s *Store) scanTaskMapRows(ctx context.Context, query string, args ...any) ([]core.Task, error) {
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

// ListTaskMap reads the workspace task map in at most five set-based
// queries without correlated subqueries: open and scope-selected task rows,
// the workspace's dependency edges, any finished dependency rows of selected
// open tasks, the requirement add/remove events with their task states, and
// the workspace's requirement titles (component-persistence).
func (s *Store) ListTaskMap(ctx context.Context, query store.TaskMapQuery) (store.TaskMap, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return store.TaskMap{}, err
	}
	finishedClause := "FALSE"
	args := []any{ws}
	if query.IncludeFinished {
		finishedClause = "TRUE"
		if len(query.Repositories) > 0 {
			finishedClause = "t.repo_name IN (" + taskMapMarks(len(query.Repositories)) + ")"
			args = append(args, taskMapArgs(query.Repositories)...)
		}
	}
	loaded, err := s.scanTaskMapRows(ctx, `SELECT `+taskMapColumns+` FROM tasks t
WHERE t.workspace_id=? AND (t.state NOT IN ('merged','closed') OR `+finishedClause+`)
ORDER BY t.id`, args...)
	if err != nil {
		return store.TaskMap{}, err
	}
	loadedByID := make(map[string]bool, len(loaded))
	for _, task := range loaded {
		loadedByID[task.ID] = true
	}
	edgeRows, err := documentRows(ctx, s.db, `SELECT task_id, depends_on_task_id FROM task_dependencies
WHERE workspace_id=? ORDER BY task_id, depends_on_task_id`, ws)
	if err != nil {
		return store.TaskMap{}, err
	}
	allEdges := map[string][]string{}
	for edgeRows.Next() {
		var taskID, dependencyID string
		if err = edgeRows.Scan(&taskID, &dependencyID); err != nil {
			edgeRows.Close()
			return store.TaskMap{}, err
		}
		allEdges[taskID] = append(allEdges[taskID], dependencyID)
	}
	err = edgeRows.Err()
	edgeRows.Close()
	if err != nil {
		return store.TaskMap{}, err
	}
	var missing []string
	for _, task := range loaded {
		if core.TaskTerminal(task.State) || !store.TaskMapSelects(store.TaskMapQuery{Repositories: query.Repositories}, task, false) {
			continue
		}
		for _, dependencyID := range allEdges[task.ID] {
			if !loadedByID[dependencyID] {
				loadedByID[dependencyID] = true
				missing = append(missing, dependencyID)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		finished, err := s.scanTaskMapRows(ctx, `SELECT `+taskMapColumns+` FROM tasks t
WHERE t.workspace_id=? AND t.id IN (`+taskMapMarks(len(missing))+`) ORDER BY t.id`, append([]any{ws}, taskMapArgs(missing)...)...)
		if err != nil {
			return store.TaskMap{}, err
		}
		loaded = append(loaded, finished...)
	}
	edges := map[string][]string{}
	for _, task := range loaded {
		if dependencies := allEdges[task.ID]; len(dependencies) > 0 {
			edges[task.ID] = dependencies
		}
	}
	attachments, err := s.listTaskMapAttachments(ctx, ws)
	if err != nil {
		return store.TaskMap{}, err
	}
	return store.AssembleTaskMap(query, loaded, edges, attachments), nil
}

// listTaskMapAttachments reads every requirement add/remove event of the
// workspace's tasks in (at, id) order, folds each task's events with
// store.ActiveTaskContextReferences, and keeps requirements with a confirmed
// version, the documents store.TaskContextFromEvents resolves.
func (s *Store) listTaskMapAttachments(ctx context.Context, ws string) ([]store.TaskMapAttachment, error) {
	rows, err := documentRows(ctx, s.db, `SELECT e.id, e.task_id, e.kind, e.payload_json, e.at, t.state
FROM events e JOIN tasks t ON t.workspace_id=e.workspace_id AND t.id=e.task_id
WHERE e.workspace_id=? AND e.task_id IS NOT NULL AND e.kind IN (?,?)
ORDER BY e.task_id, e.at, e.id`, ws, store.TaskContextRequirementAdded, store.TaskContextRequirementRemoved)
	if err != nil {
		return nil, err
	}
	eventsByTask := map[string][]core.Event{}
	states := map[string]core.TaskState{}
	var taskOrder []string
	for rows.Next() {
		var event core.Event
		var state string
		if err = rows.Scan(&event.ID, &event.TaskID, &event.Kind, &event.Payload, &event.At, &state); err != nil {
			rows.Close()
			return nil, err
		}
		if _, seen := eventsByTask[event.TaskID]; !seen {
			taskOrder = append(taskOrder, event.TaskID)
		}
		eventsByTask[event.TaskID] = append(eventsByTask[event.TaskID], event)
		states[event.TaskID] = core.TaskState(state)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(taskOrder) == 0 {
		return nil, nil
	}
	requirementRows, err := documentRows(ctx, s.db, `SELECT id, title, COALESCE(current_version,0) FROM requirements WHERE workspace_id=?`, ws)
	if err != nil {
		return nil, err
	}
	type requirementHead struct {
		title   string
		current int
	}
	requirements := map[string]requirementHead{}
	for requirementRows.Next() {
		var id string
		var head requirementHead
		if err = requirementRows.Scan(&id, &head.title, &head.current); err != nil {
			requirementRows.Close()
			return nil, err
		}
		requirements[id] = head
	}
	err = requirementRows.Err()
	requirementRows.Close()
	if err != nil {
		return nil, err
	}
	var out []store.TaskMapAttachment
	for _, taskID := range taskOrder {
		active, _ := store.ActiveTaskContextReferences(eventsByTask[taskID])
		ids := make([]string, 0, len(active))
		for id := range active {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			head, ok := requirements[id]
			if !ok || head.current <= 0 {
				continue
			}
			out = append(out, store.TaskMapAttachment{TaskID: taskID, TaskState: states[taskID], RequirementID: id, RequirementTitle: head.title})
		}
	}
	return out, nil
}

// ListNextActionEvents reads only the assembler's event kinds for the
// requested tasks in one query, in the (at, id) order ListEvents returns.
func (s *Store) ListNextActionEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	out := map[string][]core.Event{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	args := append([]any{ws}, taskMapArgs(taskIDs)...)
	args = append(args, taskMapArgs(store.NextActionEventKinds)...)
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events
WHERE workspace_id=? AND task_id IN (`+taskMapMarks(len(taskIDs))+`) AND kind IN (`+taskMapMarks(len(store.NextActionEventKinds))+`)
ORDER BY task_id,at,id`, args...)
	if err != nil {
		return nil, err
	}
	events, err := scanDocumentEvents(rows)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		out[event.TaskID] = append(out[event.TaskID], event)
	}
	return out, nil
}

// ListLatestSpecVersionNumbers reads each requested task's latest plan
// version in one query, the version GetLatestSpecVersion returns.
func (s *Store) ListLatestSpecVersionNumbers(ctx context.Context, taskIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT task_id, MAX(version) FROM task_specs
WHERE workspace_id=? AND task_id IN (`+taskMapMarks(len(taskIDs))+`) GROUP BY task_id`, append([]any{ws}, taskMapArgs(taskIDs)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var version int
		if err = rows.Scan(&taskID, &version); err != nil {
			return nil, err
		}
		out[taskID] = version
	}
	return out, rows.Err()
}

// ListClaimBlockingProposalsForTasks applies claimBlockingProposalsTx's
// predicate to every requested task in one query.
func (s *Store) ListClaimBlockingProposalsForTasks(ctx context.Context, taskIDs []string) (map[string][]store.ClaimBlockingProposal, error) {
	out := map[string][]store.ClaimBlockingProposal{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	marks := taskMapMarks(len(taskIDs))
	args := append([]any{ws, string(core.SystemDesignOriginImplementation)}, taskMapArgs(taskIDs)...)
	args = append(args, ws, string(core.RequirementOriginImplementation))
	args = append(args, taskMapArgs(taskIDs)...)
	rows, err := documentRows(ctx, s.db, `SELECT origin_task_id,'system_design' AS tier,document_id AS id,version FROM system_design_versions
WHERE workspace_id=? AND origin=? AND origin_task_id IN (`+marks+`) AND NOT confirmed AND NOT dismissed
UNION ALL SELECT origin_task_id,'requirement',requirement_id,version FROM requirement_versions
WHERE workspace_id=? AND origin=? AND origin_task_id IN (`+marks+`) AND NOT confirmed AND NOT retired`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var item store.ClaimBlockingProposal
		if err = rows.Scan(&taskID, &item.Tier, &item.ID, &item.Version); err != nil {
			return nil, err
		}
		out[taskID] = append(out[taskID], item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return store.GroupClaimBlockingProposals(out), nil
}

// ListTaskHeads reads the requested tasks' identity, title, and state in one
// query.
func (s *Store) ListTaskHeads(ctx context.Context, taskIDs []string) (map[string]store.TaskHead, error) {
	out := map[string]store.TaskHead{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,title,state FROM tasks WHERE workspace_id=? AND id IN (`+taskMapMarks(len(taskIDs))+`)`, append([]any{ws}, taskMapArgs(taskIDs)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var head store.TaskHead
		var state string
		if err = rows.Scan(&head.ID, &head.Title, &state); err != nil {
			return nil, fmt.Errorf("scan task head: %w", err)
		}
		head.State = core.TaskState(state)
		out[head.ID] = head
	}
	return out, rows.Err()
}
