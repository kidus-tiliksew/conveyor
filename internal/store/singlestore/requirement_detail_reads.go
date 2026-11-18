package singlestore

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// Requirement-detail batch reads (component-persistence;
// component-document-corpus): one workspace-scoped set query per selection
// with one parameterized IN list, whatever the number of requested tasks.
// Neither read transfers a task body or plan content.

// ListRequirementDetailTasks reads the requested tasks' ID, title, state,
// and child presence in one query.
func (s *Store) ListRequirementDetailTasks(ctx context.Context, taskIDs []string) (map[string]store.RequirementDetailTask, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementDetailTasks")
	out := map[string]store.RequirementDetailTask{}
	ids := store.UniqueTaskIDs(taskIDs)
	if len(ids) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	// The child predicate joins the distinct parents among the requested
	// IDs instead of a correlated subquery in the select list.
	marks := taskMapMarks(len(ids))
	args := append([]any{ws}, taskMapArgs(ids)...)
	args = append(args, ws)
	args = append(args, taskMapArgs(ids)...)
	rows, err := documentRows(ctx, s.db, `SELECT t.id,t.title,t.state,parents.parent_task_id IS NOT NULL
FROM tasks t
LEFT JOIN (SELECT DISTINCT parent_task_id FROM tasks
	WHERE workspace_id=? AND parent_task_id IN (`+marks+`)) parents ON parents.parent_task_id=t.id
WHERE t.workspace_id=? AND t.id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var task store.RequirementDetailTask
		var state string
		if err = rows.Scan(&task.ID, &task.Title, &state, &task.HasChildren); err != nil {
			return nil, fmt.Errorf("scan requirement detail task: %w", err)
		}
		task.State = core.TaskState(state)
		out[task.ID] = task
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListLatestPlanSummaries reads the version and approval flag of each
// requested task's latest plan version in one query.
func (s *Store) ListLatestPlanSummaries(ctx context.Context, taskIDs []string) (map[string]store.PlanSummary, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListLatestPlanSummaries")
	out := map[string]store.PlanSummary{}
	ids := store.UniqueTaskIDs(taskIDs)
	if len(ids) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	args := append([]any{ws}, taskMapArgs(ids)...)
	args = append(args, ws)
	rows, err := documentRows(ctx, s.db, `SELECT s.task_id,s.version,s.approved
FROM (SELECT task_id,MAX(version) AS version FROM task_specs
	WHERE workspace_id=? AND task_id IN (`+taskMapMarks(len(ids))+`) GROUP BY task_id) latest
JOIN task_specs s ON s.workspace_id=? AND s.task_id=latest.task_id AND s.version=latest.version`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var plan store.PlanSummary
		if err = rows.Scan(&taskID, &plan.Version, &plan.Approved); err != nil {
			return nil, fmt.Errorf("scan latest plan summary: %w", err)
		}
		out[taskID] = plan
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// requirementVersionSummaryQuery reads every version of the requested
// requirements in version order. The %[1]s predicate pairs each requested ID
// with the current version its listed row named; SingleStore does not accept
// a parametrized multi-column IN in the select list. A historical row
// projects an empty content, so only that listed current row and the pending
// rows transfer one.
const requirementVersionSummaryQuery = `SELECT v.workspace_id,v.requirement_id,v.version,
CASE WHEN %[1]s OR (NOT v.confirmed AND NOT v.retired) THEN v.content ELSE '' END,
v.statements_json,v.origin,v.origin_session_id,v.origin_task_id,v.origin_drift_id,v.confirmed,v.confirmed_by,v.confirmed_at,v.retired,v.retired_by,v.retired_at,v.retired_by_version,v.created_at,v.derived_from,COALESCE(v.dismissal_note,'')
FROM requirement_versions v JOIN requirements r
  ON r.workspace_id=v.workspace_id AND r.id=v.requirement_id
WHERE v.workspace_id=? AND v.requirement_id IN (%[2]s)
ORDER BY v.requirement_id,v.version`

// eachDocumentVersionChunk calls fn with each chunk's (id,version) pair
// marks and arguments, and its ID marks and arguments. Each pair mark is
// "(?,?)" and binds the ID, then the version.
func eachDocumentVersionChunk(selections []store.DocumentVersionSelection, fn func(pairMarks string, pairArgs []any, idMarks string, idArgs []any) error) error {
	for start := 0; start < len(selections); start += hydrationChunk {
		chunk := selections[start:min(start+hydrationChunk, len(selections))]
		pairArgs, idArgs := make([]any, 0, 2*len(chunk)), make([]any, 0, len(chunk))
		for _, selection := range chunk {
			pairArgs = append(pairArgs, selection.ID, selection.CurrentVersion)
			idArgs = append(idArgs, selection.ID)
		}
		pairMarks := strings.TrimSuffix(strings.Repeat("(?,?),", len(chunk)), ",")
		idMarks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		if err := fn(pairMarks, pairArgs, idMarks, idArgs); err != nil {
			return err
		}
	}
	return nil
}

// ListRequirementVersionSummaries reads the requested requirements' versions
// in one query per selection chunk. The selected current version and pending
// rows keep their content and statements; a historical row keeps its
// metadata and statements, and its content is empty. The current version is
// the one each listed row named, so a confirmation after the row read cannot
// empty the listed content (component-persistence; component-document-corpus).
func (s *Store) ListRequirementVersionSummaries(ctx context.Context, selections []store.DocumentVersionSelection) (map[string][]core.RequirementVersion, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementVersionSummaries")
	out := map[string][]core.RequirementVersion{}
	selected := store.UniqueDocumentVersionSelections(selections)
	if len(selected) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	err = eachDocumentVersionChunk(selected, func(_ string, pairArgs []any, idMarks string, idArgs []any) error {
		args := append(append(append([]any{}, pairArgs...), ws), idArgs...)
		listed := strings.Repeat("(v.requirement_id=? AND v.version=?) OR ", len(idArgs))
		listed = "(" + strings.TrimSuffix(listed, " OR ") + ")"
		rows, queryErr := documentRows(ctx, s.db, fmt.Sprintf(requirementVersionSummaryQuery, listed, idMarks), args...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			version, scanErr := scanRequirementVersionRow(rows)
			if scanErr != nil {
				return scanErr
			}
			out[version.RequirementID] = append(out[version.RequirementID], version)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
