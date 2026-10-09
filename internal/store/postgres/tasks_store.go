package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) CreateTask(ctx context.Context, task core.Task) error {
	return s.CreateTaskWithDependencies(ctx, task, nil)
}

func (s *Store) CreateTaskWithDependencies(ctx context.Context, task core.Task, dependencyIDs []string) error {
	return s.CreateTaskWithDependenciesAndContext(ctx, task, dependencyIDs, store.TaskContextInput{})
}

func (s *Store) CreateTaskWithDependenciesAndContext(ctx context.Context, task core.Task, dependencyIDs []string, attached store.TaskContextInput) error {
	if task.Workspace != workspace(ctx) {
		return fmt.Errorf("task workspace %q does not match store workspace %q", task.Workspace, workspace(ctx))
	}
	var err error
	attached, err = store.NormalizeTaskContextInput(attached)
	if err != nil {
		return err
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	if task.NextStage == "" && (task.State == core.TaskQueued || task.State == core.TaskClaiming) {
		task.NextStage = core.InitialStage(task.Level)
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		return s.createTaskTx(ctx, tx, q, task, dependencyIDs, attached, nil)
	})
}

func (s *Store) createTaskTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task, dependencyIDs []string, attached store.TaskContextInput, pinned map[string]int) error {
	if len(dependencyIDs) > 0 {
		if err := lockDependencyEdgesTx(ctx, tx, task.Workspace); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, dependencyID := range dependencyIDs {
		dependencyID = strings.TrimSpace(dependencyID)
		if dependencyID == "" || seen[dependencyID] {
			return fmt.Errorf("depends_on contains an empty or duplicate task id")
		}
		seen[dependencyID] = true
		var dependencyWorkspace, dependencyState string
		if err := tx.QueryRow(ctx, `SELECT workspace_id, state FROM tasks WHERE id=$1`, dependencyID).
			Scan(&dependencyWorkspace, &dependencyState); err != nil {
			return notFound(err, "dependency task %s", dependencyID)
		}
		if dependencyWorkspace != task.Workspace {
			return fmt.Errorf("dependency task %s belongs to another workspace", dependencyID)
		}
		if core.TaskTerminal(core.TaskState(dependencyState)) {
			return fmt.Errorf("dependency task %s is not open", dependencyID)
		}
	}
	designVersions, err := validateTaskContextTx(ctx, tx, task.Workspace, attached)
	if err != nil {
		return err
	}
	if pinned != nil {
		designVersions = pinned
	}
	if occupant, err := occupyingOpenTaskID(ctx, tx, task.Repo, task.Branch, task.ID); err != nil {
		return err
	} else if occupant != "" {
		return store.TaskBranchInUseError(task.Branch, occupant)
	}
	if _, err := q.InsertTask(ctx, taskInsertParams(task)); err != nil {
		if occupant, lookErr := occupyingOpenTaskID(ctx, tx, task.Repo, task.Branch, task.ID); lookErr == nil && occupant != "" {
			return store.TaskBranchInUseError(task.Branch, occupant)
		}
		return err
	}
	if task.Supersedes != "" || task.SupersededBy != "" || task.IntakeOperatorDirection != "" {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET supersedes=NULLIF($3,''),superseded_by=NULLIF($4,''),intake_operator_direction=$5 WHERE workspace_id=$1 AND id=$2`, task.Workspace, task.ID, task.Supersedes, task.SupersededBy, task.IntakeOperatorDirection); err != nil {
			return err
		}
	}
	if err := store.ValidateRepositoryInstallTask(task); err != nil {
		return err
	}
	if task.RepositoryInstallAttempt > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO repository_install_tasks(workspace_id,repository_name,attempt,task_id) VALUES($1,$2,$3,$4)`, task.Workspace, task.Repo, task.RepositoryInstallAttempt, task.ID); err != nil {
			return err
		}
	}
	_, err = insertEventWithID(ctx, q, core.Event{
		TaskID:  task.ID,
		Kind:    "task.created",
		Payload: store.TaskCreatedPayload(ctx, task),
		At:      task.CreatedAt,
	})
	if err != nil {
		return err
	}
	for dependencyID := range seen {
		if _, err := tx.Exec(ctx, `INSERT INTO task_dependencies (workspace_id, task_id, depends_on_task_id)
				VALUES ($1,$2,$3)`, task.Workspace, task.ID, dependencyID); err != nil {
			return fmt.Errorf("depends_on %s: %w", dependencyID, err)
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: task.ID, Kind: "task.dependency_added", At: task.CreatedAt,
			Payload: core.JSONPayload(map[string]string{"task_id": task.ID, "depends_on_task_id": dependencyID})}); err != nil {
			return err
		}
	}
	for _, id := range attached.RequirementIDs {
		if err := insertEvent(ctx, q, core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, At: task.CreatedAt,
			Payload: core.JSONPayload(map[string]any{"id": id})}); err != nil {
			return err
		}
	}
	for _, id := range attached.DesignIDs {
		if err := insertEvent(ctx, q, core.Event{TaskID: task.ID, Kind: store.TaskContextDesignAdded, At: task.CreatedAt,
			Payload: core.JSONPayload(map[string]any{"id": id, "version": designVersions[id]})}); err != nil {
			return err
		}
	}
	if task.State == core.TaskQueued {
		_, err := s.enqueueTaskTx(ctx, tx, task.ID, task.Workspace)
		return err
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, id string) (core.Task, error) {
	task, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
	if err != nil {
		return core.Task{}, notFound(err, "task %s", id)
	}
	result := taskFromDB(task)
	if err = s.hydrateTaskRelations(ctx, &result); err != nil {
		return core.Task{}, err
	}
	if err = s.hydrateGitHubLifecycle(ctx, &result); err != nil {
		return core.Task{}, err
	}
	return result, nil
}

func (s *Store) GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error) {
	task, err := s.queries.GetTaskByIntakeKey(ctx, db.GetTaskByIntakeKeyParams{WorkspaceID: workspace(ctx), IntakeKey: nullableText(key)})
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Task{}, false, nil
	}
	if err != nil {
		return core.Task{}, false, err
	}
	result := taskFromDB(task)
	if err = s.hydrateTaskRelations(ctx, &result); err != nil {
		return core.Task{}, false, err
	}
	if err = s.hydrateGitHubLifecycle(ctx, &result); err != nil {
		return core.Task{}, false, err
	}
	return result, true, nil
}

func (s *Store) ListTasks(ctx context.Context) ([]core.Task, error) {
	rows, err := s.queries.ListTasks(ctx, workspace(ctx))
	if err != nil {
		return nil, err
	}
	result := make([]core.Task, len(rows))
	dependencyTaskIDs := make([]string, 0, len(rows))
	parentTaskIDs := make([]string, 0, len(rows))
	for i := range rows {
		result[i] = taskFromDB(rows[i].Task)
		if rows[i].HasDependencies {
			dependencyTaskIDs = append(dependencyTaskIDs, result[i].ID)
		}
		if rows[i].HasChildren {
			parentTaskIDs = append(parentTaskIDs, result[i].ID)
		}
	}
	if err = s.hydrateTaskRelationsBatch(ctx, result, dependencyTaskIDs, parentTaskIDs); err != nil {
		return nil, err
	}
	if err = s.hydrateGitHubLifecyclesBatch(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListTasksFiltered narrows the workspace with the shared Tasks/Board predicate
// (AC-2.4). An inactive filter routes to ListTasks so the many callers that
// never filter keep their existing plan.
func (s *Store) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	if !filter.Active() {
		return s.ListTasks(ctx)
	}
	// A zero page limit is the query's own "no LIMIT" spelling, so the paged
	// Tasks read and this unpaged one stay one SQL predicate.
	rows, err := s.queries.ListTaskOperationsTasks(ctx, taskOperationsListParams(ctx, filter, 0, 0))
	if err != nil {
		return nil, err
	}
	return s.hydrateTaskRows(ctx, rows)
}

// taskFilterParams binds the shared predicate. Every member is a store-side
// argument: nothing about the filter is decided after the rows come back.
func taskFilterParams(ctx context.Context, filter store.TaskFilter) db.CountTaskOperationsTasksParams {
	// Empty arrays rather than nil: every list member binds as text[], and the
	// query's cardinality guard is what turns an empty list into "inactive".
	states := make([]string, len(filter.States))
	for i, state := range filter.States {
		states[i] = string(state)
	}
	return db.CountTaskOperationsTasksParams{
		WorkspaceID: workspace(ctx), TaskStates: states,
		Repositories: emptyIfNil(filter.Repositories),
		Search:       filter.Query, CreatedFrom: nullableTimestamp(filter.CreatedFrom),
		CreatedTo: nullableTimestamp(filter.CreatedTo), ServesRequirements: emptyIfNil(filter.ServesRequirementIDs),
		GoverningDesigns: emptyIfNil(filter.GoverningDesignIDs), Assignee: filter.Assignee,
		UpdatedFrom: nullableTimestamp(filter.UpdatedFrom), UpdatedTo: nullableTimestamp(filter.UpdatedTo),
	}
}

func taskOperationsListParams(ctx context.Context, filter store.TaskFilter, limit, offset int) db.ListTaskOperationsTasksParams {
	bound := taskFilterParams(ctx, filter)
	return db.ListTaskOperationsTasksParams{
		WorkspaceID: bound.WorkspaceID, TaskStates: bound.TaskStates, Repositories: bound.Repositories,
		Search: bound.Search, CreatedFrom: bound.CreatedFrom, CreatedTo: bound.CreatedTo,
		ServesRequirements: bound.ServesRequirements, GoverningDesigns: bound.GoverningDesigns,
		Assignee:    bound.Assignee,
		UpdatedFrom: bound.UpdatedFrom, UpdatedTo: bound.UpdatedTo,
		PageLimit: int32(limit), PageOffset: int32(offset),
	}
}

// hydrateTaskRows fills the relations and GitHub lifecycle every task list
// projection carries, so the paged and unpaged reads return identical rows.
func (s *Store) hydrateTaskRows(ctx context.Context, rows []db.ListTaskOperationsTasksRow) ([]core.Task, error) {
	result := make([]core.Task, len(rows))
	dependencyTaskIDs := make([]string, 0, len(rows))
	parentTaskIDs := make([]string, 0, len(rows))
	for i := range rows {
		result[i] = taskFromDB(rows[i].Task)
		if rows[i].HasDependencies {
			dependencyTaskIDs = append(dependencyTaskIDs, result[i].ID)
		}
		if rows[i].HasChildren {
			parentTaskIDs = append(parentTaskIDs, result[i].ID)
		}
	}
	if err := s.hydrateTaskRelationsBatch(ctx, result, dependencyTaskIDs, parentTaskIDs); err != nil {
		return nil, err
	}
	if err := s.hydrateGitHubLifecyclesBatch(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ListTaskOperations(ctx context.Context, query store.TaskOperationsQuery) (store.TaskOperationsPage, error) {
	if err := query.Validate(); err != nil {
		return store.TaskOperationsPage{}, err
	}
	filter := taskFilterParams(ctx, query.TaskFilter)
	total, err := s.queries.CountTaskOperationsTasks(ctx, filter)
	if err != nil {
		return store.TaskOperationsPage{}, err
	}
	rows, err := s.queries.ListTaskOperationsTasks(ctx, taskOperationsListParams(ctx, query.TaskFilter, query.Limit, query.Offset))
	if err != nil {
		return store.TaskOperationsPage{}, err
	}
	page := store.TaskOperationsPage{
		Tasks: make([]core.Task, len(rows)), Events: map[string][]core.Event{},
		Plans: map[string]core.SpecVersion{}, Total: int(total),
	}
	dependencyTaskIDs := make([]string, 0, len(rows))
	parentTaskIDs := make([]string, 0, len(rows))
	taskIDs := make([]string, 0, len(rows))
	for i := range rows {
		page.Tasks[i] = taskFromDB(rows[i].Task)
		taskIDs = append(taskIDs, page.Tasks[i].ID)
		if rows[i].HasDependencies {
			dependencyTaskIDs = append(dependencyTaskIDs, page.Tasks[i].ID)
		}
		if rows[i].HasChildren {
			parentTaskIDs = append(parentTaskIDs, page.Tasks[i].ID)
		}
	}
	if err = s.hydrateTaskRelationsBatch(ctx, page.Tasks, dependencyTaskIDs, parentTaskIDs); err != nil {
		return store.TaskOperationsPage{}, err
	}
	if len(taskIDs) == 0 {
		return page, nil
	}
	if err = s.hydrateGitHubLifecyclesBatch(ctx, page.Tasks); err != nil {
		return store.TaskOperationsPage{}, err
	}
	events, err := s.queries.ListTaskOperationsEvents(ctx, db.ListTaskOperationsEventsParams{WorkspaceID: filter.WorkspaceID, TaskIds: taskIDs})
	if err != nil {
		return store.TaskOperationsPage{}, err
	}
	for _, event := range events {
		converted := eventFromDB(event)
		page.Events[converted.TaskID] = append(page.Events[converted.TaskID], converted)
	}
	plans, err := s.queries.ListTaskOperationsLatestPlans(ctx, db.ListTaskOperationsLatestPlansParams{WorkspaceID: filter.WorkspaceID, TaskIds: taskIDs})
	if err != nil {
		return store.TaskOperationsPage{}, err
	}
	for _, plan := range plans {
		page.Plans[plan.TaskID] = specFromDB(plan)
	}
	legacyRows, err := s.boundary.Query(ctx, `SELECT task_id,spec_version FROM legacy_spec_gate_versions
		WHERE workspace_id=$1 AND task_id=ANY($2::text[])`, filter.WorkspaceID, taskIDs)
	if err != nil {
		return store.TaskOperationsPage{}, err
	}
	defer legacyRows.Close()
	for legacyRows.Next() {
		var taskID string
		var version int
		if err = legacyRows.Scan(&taskID, &version); err != nil {
			return store.TaskOperationsPage{}, err
		}
		plan, ok := page.Plans[taskID]
		if ok && plan.Version == version {
			plan.LegacyGate = true
			page.Plans[taskID] = plan
		}
	}
	if err = legacyRows.Err(); err != nil {
		return store.TaskOperationsPage{}, err
	}
	return page, nil
}

func (s *Store) ListTaskPage(ctx context.Context, query store.TaskOperationsQuery) (store.TaskPage, error) {
	if err := query.Validate(); err != nil {
		return store.TaskPage{}, err
	}
	filter := taskFilterParams(ctx, query.TaskFilter)
	total, err := s.queries.CountTaskOperationsTasks(ctx, filter)
	if err != nil {
		return store.TaskPage{}, err
	}
	rows, err := s.queries.ListTaskOperationsTasks(ctx, taskOperationsListParams(ctx, query.TaskFilter, query.Limit, query.Offset))
	if err != nil {
		return store.TaskPage{}, err
	}
	tasks, err := s.hydrateTaskRows(ctx, rows)
	if err != nil {
		return store.TaskPage{}, err
	}
	return store.TaskPage{Tasks: tasks, Total: int(total)}, nil
}

func (s *Store) ListCallerAttentionTaskPage(ctx context.Context, query store.CallerAttentionQuery) (store.TaskPage, error) {
	if err := query.Validate(); err != nil {
		return store.TaskPage{}, err
	}
	cte := attentionTasksCTE()
	filter := `
FROM attention_tasks attention
JOIN tasks t ON t.workspace_id=$1 AND t.id=attention.task_id
WHERE t.assignee_user_id=$2
  AND t.state NOT IN ('merged','closed')
  AND NOT EXISTS (
      SELECT 1 FROM tasks child
      WHERE child.workspace_id=t.workspace_id AND child.parent_task_id=t.id
  )`
	var total int64
	if err := s.boundary.QueryRow(ctx, cte+"\nSELECT count(*)::bigint "+filter, workspace(ctx), query.UserID).Scan(&total); err != nil {
		return store.TaskPage{}, err
	}
	rows, err := s.boundary.Query(ctx, cte+`
SELECT t.*
`+filter+`
ORDER BY t.created_at DESC,t.id
LIMIT $3 OFFSET $4`, workspace(ctx), query.UserID, query.Limit, query.Offset)
	if err != nil {
		return store.TaskPage{}, err
	}
	defer rows.Close()
	persisted, err := pgx.CollectRows(rows, pgx.RowToStructByName[db.Task])
	if err != nil {
		return store.TaskPage{}, err
	}
	tasks := make([]core.Task, len(persisted))
	taskIDs := make([]string, len(persisted))
	for index := range persisted {
		tasks[index] = taskFromDB(persisted[index])
		taskIDs[index] = tasks[index].ID
	}
	if err = s.hydrateTaskRelationsBatch(ctx, tasks, taskIDs, taskIDs); err != nil {
		return store.TaskPage{}, err
	}
	if err = s.hydrateGitHubLifecyclesBatch(ctx, tasks); err != nil {
		return store.TaskPage{}, err
	}
	return store.TaskPage{Tasks: tasks, Total: int(total)}, nil
}

func (s *Store) hydrateTaskRelations(ctx context.Context, task *core.Task) error {
	rows, err := s.boundary.Query(ctx, `SELECT dependency.id, dependency.title, dependency.state,
		dependency.origin_spec_version, dependency.origin_sub_id
		FROM task_dependencies edge
		JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
			AND dependency.id=edge.depends_on_task_id
		WHERE edge.workspace_id=$1 AND edge.task_id=$2
		ORDER BY dependency.id`, workspace(ctx), task.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var relation core.TaskRelation
		if err = rows.Scan(&relation.ID, &relation.Title, &relation.State, &relation.OriginSpecVersion, &relation.OriginSubID); err != nil {
			return err
		}
		task.Dependencies = append(task.Dependencies, relation)
		if relation.State != core.TaskMerged {
			task.BlockingTaskIDs = append(task.BlockingTaskIDs, relation.ID)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	childRows, err := s.boundary.Query(ctx, `SELECT id,title,state,origin_spec_version,origin_sub_id
		FROM tasks WHERE workspace_id=$1 AND parent_task_id=$2
		ORDER BY origin_spec_version,origin_sub_id,id`, workspace(ctx), task.ID)
	if err != nil {
		return err
	}
	defer childRows.Close()
	for childRows.Next() {
		var relation core.TaskRelation
		if err = childRows.Scan(&relation.ID, &relation.Title, &relation.State, &relation.OriginSpecVersion, &relation.OriginSubID); err != nil {
			return err
		}
		task.Children = append(task.Children, relation)
	}
	return childRows.Err()
}

func (s *Store) hydrateTaskRelationsBatch(ctx context.Context, tasks []core.Task, dependencyTaskIDs, parentTaskIDs []string) error {
	byID := make(map[string]*core.Task, len(tasks))
	for index := range tasks {
		byID[tasks[index].ID] = &tasks[index]
	}
	if len(dependencyTaskIDs) > 0 {
		rows, err := s.boundary.Query(ctx, `SELECT edge.task_id,dependency.id,dependency.title,dependency.state,
			dependency.origin_spec_version,dependency.origin_sub_id
			FROM task_dependencies edge
			JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
				AND dependency.id=edge.depends_on_task_id
			WHERE edge.workspace_id=$1 AND edge.task_id=ANY($2::text[])
			ORDER BY edge.task_id,dependency.id`, workspace(ctx), dependencyTaskIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var taskID string
			var relation core.TaskRelation
			if err = rows.Scan(&taskID, &relation.ID, &relation.Title, &relation.State,
				&relation.OriginSpecVersion, &relation.OriginSubID); err != nil {
				rows.Close()
				return err
			}
			task := byID[taskID]
			task.Dependencies = append(task.Dependencies, relation)
			if relation.State != core.TaskMerged {
				task.BlockingTaskIDs = append(task.BlockingTaskIDs, relation.ID)
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	if len(parentTaskIDs) == 0 {
		return nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT parent_task_id,id,title,state,origin_spec_version,origin_sub_id
		FROM tasks
		WHERE workspace_id=$1 AND parent_task_id=ANY($2::text[])
		ORDER BY parent_task_id,origin_spec_version,origin_sub_id,id`, workspace(ctx), parentTaskIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var parentTaskID string
		var relation core.TaskRelation
		if err = rows.Scan(&parentTaskID, &relation.ID, &relation.Title, &relation.State,
			&relation.OriginSpecVersion, &relation.OriginSubID); err != nil {
			return err
		}
		byID[parentTaskID].Children = append(byID[parentTaskID].Children, relation)
	}
	return rows.Err()
}

func (s *Store) hydrateTaskAssignee(ctx context.Context, task *core.Task) error {
	var assignee core.TaskAssignee
	err := s.boundary.QueryRow(ctx, `SELECT u.id,u.email,u.display_name
		FROM tasks t JOIN workspace_role_bindings b ON b.workspace_id=t.workspace_id AND b.user_id=t.assignee_user_id
		JOIN users u ON u.id=b.user_id
		WHERE t.workspace_id=$1 AND t.id=$2`, workspace(ctx), task.ID).
		Scan(&assignee.UserID, &assignee.Email, &assignee.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		task.Assignee = nil
		return nil
	}
	if err != nil {
		return err
	}
	task.Assignee = &assignee
	return nil
}

func (s *Store) hydrateTaskAssignees(ctx context.Context, tasks []core.Task) error {
	ids := make([]string, len(tasks))
	byID := make(map[string]*core.Task, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
		byID[tasks[i].ID] = &tasks[i]
	}
	rows, err := s.boundary.Query(ctx, `SELECT t.id,u.id,u.email,u.display_name
		FROM tasks t JOIN workspace_role_bindings b ON b.workspace_id=t.workspace_id AND b.user_id=t.assignee_user_id
		JOIN users u ON u.id=b.user_id WHERE t.workspace_id=$1 AND t.id=ANY($2::text[])`, workspace(ctx), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var assignee core.TaskAssignee
		if err = rows.Scan(&taskID, &assignee.UserID, &assignee.Email, &assignee.DisplayName); err != nil {
			return err
		}
		if task := byID[taskID]; task != nil {
			task.Assignee = &assignee
		}
	}
	return rows.Err()
}

func (s *Store) SetTaskHold(ctx context.Context, id string, hold bool) (core.Task, error) {
	var result core.Task
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if before.Hold == hold {
			result = taskFromDB(before)
			return nil
		}
		updated, err := q.UpdateTaskHold(ctx, db.UpdateTaskHoldParams{ID: id, WorkspaceID: workspace(ctx), Hold: hold})
		if err != nil {
			return err
		}
		result = taskFromDB(updated)
		kind := "task.hold.set"
		if !hold {
			kind = "task.hold.cleared"
		}
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: kind, Payload: core.JSONPayload(map[string]any{"hold": hold})})
	})
	return result, err
}

func (s *Store) SetTaskAssigneeCommand(ctx context.Context, lease taskops.TaskLease, id, assigneeUserID string) (core.Task, error) {
	if !lease.ValidForCommand(id, taskops.SetAssigneeCommand) {
		return core.Task{}, fmt.Errorf("task assignment requires a valid taskops lease")
	}
	assigneeUserID = strings.TrimSpace(assigneeUserID)
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var current pgtype.Text
		if err := tx.QueryRow(ctx, `SELECT assignee_user_id FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), id).Scan(&current); err != nil {
			return notFound(err, "task %s", id)
		}
		if assigneeUserID != "" {
			var active bool
			var role core.WorkspaceRole
			err := tx.QueryRow(ctx, `SELECT u.status='active',b.role FROM workspace_role_bindings b JOIN users u ON u.id=b.user_id
				WHERE b.workspace_id=$1 AND b.user_id=$2`, workspace(ctx), assigneeUserID).Scan(&active, &role)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
				return fmt.Errorf("assignee %s is not an active member of workspace %s", assigneeUserID, workspace(ctx))
			}
			if err != nil {
				return err
			}
			if !core.RoleAllows(role, core.CapabilityClaimWork) {
				return fmt.Errorf("assignee %s role %s lacks %s capability in workspace %s", assigneeUserID, role, core.CapabilityClaimWork, workspace(ctx))
			}
		}
		if current.String == assigneeUserID && current.Valid == (assigneeUserID != "") {
			return nil
		}
		if _, err := q.UpdateTaskAssignee(ctx, db.UpdateTaskAssigneeParams{ID: id, WorkspaceID: workspace(ctx), AssigneeUserID: nullableText(assigneeUserID)}); err != nil {
			return err
		}
		kind := "task.assignee.set"
		if assigneeUserID == "" {
			kind = "task.assignee.cleared"
		}
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: kind, Payload: core.JSONPayload(map[string]any{"assignee_user_id": assigneeUserID})})
	})
	if err != nil {
		return core.Task{}, err
	}
	return s.GetTask(ctx, id)
}

func (s *Store) RequestChangesCommand(ctx context.Context, lease taskops.TaskLease, request taskops.RequestChanges) (core.Task, error) {
	if !lease.ValidForCommand(request.TaskID, taskops.RequestChangesCommand) {
		return core.Task{}, fmt.Errorf("request changes requires a valid taskops lease")
	}
	if strings.TrimSpace(request.Feedback) == "" {
		return core.Task{}, fmt.Errorf("feedback is required")
	}
	actor := store.ActorFromContext(ctx)
	if actor.Role != core.ActorUser {
		return core.Task{}, fmt.Errorf("request changes requires a user actor")
	}
	var result core.Task
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		key := "conveyor:task-operation:" + workspace(ctx) + ":" + request.TaskID
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
			return err
		}
		row, err := q.GetTask(ctx, db.GetTaskParams{ID: request.TaskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", request.TaskID)
		}
		before := taskFromDB(row)
		var latestPayload []byte
		events := []core.Event{}
		if err = tx.QueryRow(ctx, `SELECT e.payload_json FROM events e JOIN tasks t ON t.id=e.task_id WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='task.state_changed' ORDER BY e.at DESC, e.id DESC LIMIT 1`, workspace(ctx), request.TaskID).Scan(&latestPayload); err != nil {
			return err
		}
		events = append(events, core.Event{Kind: "task.state_changed", Payload: latestPayload})
		if !store.AtMergeGate(before, events) {
			return fmt.Errorf("task %s is not at the merge gate", request.TaskID)
		}
		if request.JobID != "" {
			job, jobErr := q.GetJob(ctx, db.GetJobParams{ID: request.JobID, WorkspaceID: workspace(ctx)})
			if jobErr != nil || job.TaskID != request.TaskID || core.Stage(job.Stage) != core.StageReview {
				return fmt.Errorf("review job %s does not belong to task %s", request.JobID, request.TaskID)
			}
		}
		count, err := q.CountEvents(ctx, db.CountEventsParams{TaskID: nullableText(request.TaskID), Kind: "pipeline.bounced", WorkspaceID: workspace(ctx)})
		if err != nil {
			return err
		}
		plan := store.PlanChangesRequested(store.ChangesRequestedInput{
			TaskID: request.TaskID, JobID: request.JobID, ActorID: actor.ID, ActorRole: actor.Role,
			ReasonCode: store.UserRequestChangesReason, Feedback: request.Feedback, Source: store.UserRequestChangesSource,
			Count: int(count) + 1, Window: 1, MaxBounces: request.MaxBounces, Requeue: core.TaskInterventionRedirect,
		})
		state, err := core.TransitionTask(before.State, plan.Command)
		if err != nil {
			return err
		}
		if _, err = q.InsertIntervention(ctx, interventionInsertParams(plan.Intervention)); err != nil {
			return err
		}
		for _, event := range plan.Events {
			if err = insertEvent(ctx, q, event); err != nil {
				return err
			}
		}
		updated, err := q.UpdateTaskTransition(ctx, db.UpdateTaskTransitionParams{ID: request.TaskID, WorkspaceID: workspace(ctx), State: string(state), NextStage: string(plan.NextStage), RecoveryStage: string(plan.Recovery)})
		if err != nil {
			return err
		}
		result = taskFromDB(updated)
		if request.Hold && !before.Hold {
			if _, err = tx.Exec(ctx, `UPDATE tasks SET hold=true,updated_at=now() WHERE workspace_id=$1 AND id=$2`, workspace(ctx), request.TaskID); err != nil {
				return err
			}
			result.Hold = true
			if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "task.hold_changed", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"hold": true, "reason_code": store.UserRequestChangesReason})}); err != nil {
				return err
			}
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "task.state_changed", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state, "command": plan.Command})}); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "pipeline.transition_decided", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"from_stage": before.NextStage, "next_stage": plan.NextStage, "recovery_stage": plan.Recovery, "state": state})}); err != nil {
			return err
		}
		_, err = s.enqueueTaskTx(ctx, tx, request.TaskID, row.WorkspaceID)
		return err
	})
	return result, err
}

func (s *Store) UpdateTaskClassification(ctx context.Context, id, class string) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := q.UpdateTaskClassification(ctx, db.UpdateTaskClassificationParams{ID: id, WorkspaceID: workspace(ctx), Class: class}); err != nil {
			return notFound(err, "task %s", id)
		}
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: "task.classified", Payload: core.JSONPayload(map[string]any{"class": class})})
	})
}

func (s *Store) EnsureTaskEnqueued(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		task, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if core.TaskState(task.State) != core.TaskQueued {
			return fmt.Errorf("task %s is not queued", id)
		}
		inserted, err := s.enqueueTaskTx(ctx, tx, id, task.WorkspaceID)
		if err != nil || !inserted {
			return err
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: id, Kind: "dispatch.reconciled",
			Payload: core.JSONPayload(map[string]string{"reason": "missing durable queue job"}),
		})
	})
}

// ReconcileBlueprintClosures repairs missed close edges for blueprint parents.
// Candidate discovery is a pure read; each close revalidates under the
// parent's lifecycle and row locks so concurrent ticks and commands are
// exactly-once.
func (s *Store) ReconcileBlueprintClosures(ctx context.Context) (int, error) {
	rows, err := s.boundary.Query(ctx, `
SELECT parent.id
FROM tasks parent
WHERE parent.workspace_id=$1
  AND parent.state='queued'
  AND EXISTS (
      SELECT 1 FROM tasks child
      WHERE child.workspace_id=parent.workspace_id
        AND child.parent_task_id=parent.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM tasks child
      WHERE child.workspace_id=parent.workspace_id
        AND child.parent_task_id=parent.id
        AND child.state NOT IN ('merged','closed')
  )
ORDER BY parent.created_at,parent.id`, workspace(ctx))
	if err != nil {
		return 0, err
	}
	var parentIDs []string
	for rows.Next() {
		var parentID string
		if err = rows.Scan(&parentID); err != nil {
			rows.Close()
			return 0, err
		}
		parentIDs = append(parentIDs, parentID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	closed := 0
	for _, parentID := range parentIDs {
		didClose := false
		err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			var closeErr error
			didClose, closeErr = s.closeBlueprintParentTx(ctx, tx, q, parentID)
			return closeErr
		})
		if err != nil {
			return closed, err
		}
		if didClose {
			closed++
		}
	}
	return closed, nil
}

func (s *Store) CreateJob(ctx context.Context, job core.Job) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := q.GetTask(ctx, db.GetTaskParams{ID: job.TaskID, WorkspaceID: workspace(ctx)}); err != nil {
			return notFound(err, "task %s", job.TaskID)
		}
		if _, err := q.InsertJob(ctx, jobInsertParams(job)); err != nil {
			return err
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: job.TaskID, JobID: job.ID, Kind: "job.created",
			Payload: core.JSONPayload(job),
		})
	})
}

func (s *Store) UpdateJob(ctx context.Context, job core.Job) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var current core.JobState
		if err := tx.QueryRow(ctx, `SELECT j.state
			FROM jobs j
			JOIN tasks t ON t.id=j.task_id
			WHERE t.workspace_id=$1 AND j.id=$2
			FOR UPDATE OF j`, workspace(ctx), job.ID).Scan(&current); err != nil {
			return notFound(err, "job %s", job.ID)
		}
		if err := store.ValidateJobTransition(current, job.State); err != nil {
			return err
		}
		row, err := q.UpdateJob(ctx, jobUpdateParams(job, workspace(ctx)))
		if err != nil {
			return notFound(err, "job %s", job.ID)
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: row.TaskID, JobID: row.ID, Kind: "job.updated", Payload: core.JSONPayload(job),
		})
	})
}

func (s *Store) ListJobs(ctx context.Context, taskID string) ([]core.Job, error) {
	rows, err := s.queries.ListJobs(ctx, db.ListJobsParams{TaskID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return nil, err
	}
	result := make([]core.Job, len(rows))
	for i := range rows {
		result[i] = jobFromDB(rows[i])
	}
	return result, nil
}

func (s *Store) GetLatestJob(ctx context.Context, taskID string) (core.Job, bool, error) {
	row, err := s.queries.GetLatestJob(ctx, db.GetLatestJobParams{TaskID: taskID, WorkspaceID: workspace(ctx)})
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Job{}, false, nil
	}
	if err != nil {
		return core.Job{}, false, err
	}
	return jobFromDB(row), true, nil
}

func (s *Store) ApproveSpecVersionAndMaterialize(ctx context.Context, taskID string, version int) ([]core.Task, error) {
	var children []core.Task
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockDependencyEdgesTx(ctx, tx, workspace(ctx)); err != nil {
			return err
		}
		key := "conveyor:blueprint-approval:" + workspace(ctx) + ":" + taskID
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
			return err
		}
		parentRow, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", taskID)
		}
		parent := taskFromDB(parentRow)
		var decompositionJSON []byte
		if err = tx.QueryRow(ctx, `SELECT decomposition FROM task_specs
			WHERE task_id=$1 AND version=$2
			  AND version=(SELECT MAX(version) FROM task_specs WHERE task_id=$1)
			FOR UPDATE`, taskID, version).Scan(&decompositionJSON); err != nil {
			return fmt.Errorf("spec version %d for task %s not found or superseded", version, taskID)
		}
		var decomposition []core.BlueprintDecompositionItem
		if len(decompositionJSON) > 0 {
			if err = json.Unmarshal(decompositionJSON, &decomposition); err != nil {
				return fmt.Errorf("decode blueprint decomposition: %w", err)
			}
		}
		if err = core.ValidateBlueprintDecomposition(decomposition); err != nil {
			return err
		}
		baseBranches := make(map[string]string, len(decomposition))
		for _, item := range decomposition {
			var baseBranch string
			if err = tx.QueryRow(ctx, `SELECT default_base FROM repos WHERE workspace_id=$1 AND name=$2`,
				workspace(ctx), strings.TrimSpace(item.Repo)).Scan(&baseBranch); err != nil {
				return fmt.Errorf("blueprint %s repository %q is not configured in workspace %s", item.ID, item.Repo, workspace(ctx))
			}
			baseBranches[item.ID] = baseBranch
		}
		childrenBySub := map[string]core.Task{}
		rows, err := tx.Query(ctx, `SELECT id,origin_sub_id FROM tasks
			WHERE workspace_id=$1 AND parent_task_id=$2 AND origin_sub_id<>''
			ORDER BY origin_spec_version,created_at,id`, workspace(ctx), taskID)
		if err != nil {
			return err
		}
		existingIDs := map[string]string{}
		for rows.Next() {
			var id, originSubID string
			if scanErr := rows.Scan(&id, &originSubID); scanErr != nil {
				rows.Close()
				return scanErr
			}
			if _, exists := existingIDs[originSubID]; !exists {
				existingIDs[originSubID] = id
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for originSubID, id := range existingIDs {
			childRow, getErr := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
			if getErr != nil {
				return getErr
			}
			childrenBySub[originSubID] = taskFromDB(childRow)
		}
		createdAt := time.Now().UTC()
		childrenToCreate := 0
		for _, item := range decomposition {
			if _, exists := childrenBySub[item.ID]; !exists {
				childrenToCreate++
			}
		}
		if childrenToCreate > 0 {
			_, err = insertEventWithID(ctx, q, core.Event{
				TaskID: taskID, Kind: "blueprint.materialized", At: createdAt,
				Payload: core.JSONPayload(map[string]any{
					"version": version, "children_created": childrenToCreate, "children_total": len(decomposition),
				}),
			})
			if err != nil {
				return err
			}
		}
		createdSubs := make(map[string]struct{}, len(decomposition))
		for _, item := range decomposition {
			if _, exists := childrenBySub[item.ID]; exists {
				continue
			}
			id := core.NewTaskID()
			title := strings.TrimSpace(item.Summary)
			title = core.TruncateUTF8Bytes(title, 200)
			child := core.Task{
				ID: id, Workspace: parent.Workspace,
				Source: fmt.Sprintf("blueprint:%s@v%d#%s", parent.ID, version, item.ID),
				Title:  title,
				Body:   fmt.Sprintf("%s\n\nDefined by blueprint task %s, spec version %d (%s).", strings.TrimSpace(item.Summary), parent.ID, version, item.ID),
				Class:  parent.Class, Level: parent.Level, Hold: parent.Hold,
				SpecApproval: parent.SpecApproval, MergeApproval: parent.MergeApproval,
				PolicyVersion: parent.PolicyVersion,
				SetupContract: parent.SetupContract, Repo: strings.TrimSpace(item.Repo),
				BaseBranch: baseBranches[item.ID], Branch: gitx.BranchName(id),
				State: core.TaskQueued, NextStage: core.StageImplement,
				ParentTaskID: parent.ID, OriginSpecVersion: version, OriginSubID: item.ID,
				CreatedAt: createdAt,
			}
			if _, err = q.InsertTask(ctx, taskInsertParams(child)); err != nil {
				return fmt.Errorf("materialize %s: %w", item.ID, err)
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: child.ID, Kind: "task.created", Payload: core.JSONPayload(child), At: createdAt}); err != nil {
				return err
			}
			if _, err = s.enqueueTaskTx(ctx, tx, child.ID, child.Workspace); err != nil {
				return err
			}
			childrenBySub[item.ID] = child
			createdSubs[item.ID] = struct{}{}
		}
		for _, item := range decomposition {
			if _, created := createdSubs[item.ID]; !created {
				continue
			}
			child := childrenBySub[item.ID]
			for _, dependencySubID := range item.DependsOn {
				dependency := childrenBySub[dependencySubID]
				if _, err = tx.Exec(ctx, `INSERT INTO task_dependencies
					(workspace_id,task_id,depends_on_task_id) VALUES ($1,$2,$3)
					ON CONFLICT DO NOTHING`, workspace(ctx), child.ID, dependency.ID); err != nil {
					return fmt.Errorf("materialize dependency %s -> %s: %w", item.ID, dependencySubID, err)
				}
				if err = insertEvent(ctx, q, core.Event{TaskID: child.ID, Kind: "task.dependency_added", At: createdAt,
					Payload: core.JSONPayload(map[string]string{"task_id": child.ID, "depends_on_task_id": dependency.ID})}); err != nil {
					return err
				}
			}
		}
		if _, err = q.ApproveLatestSpecVersion(ctx, db.ApproveLatestSpecVersionParams{TaskID: taskID, Version: int32(version), WorkspaceID: workspace(ctx)}); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})}); err != nil {
			return err
		}
		children = children[:0]
		for _, item := range decomposition {
			children = append(children, childrenBySub[item.ID])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for index := range children {
		id := children[index].ID
		child, getErr := s.GetTask(ctx, id)
		if getErr != nil {
			return nil, fmt.Errorf("hydrate materialized child %s: %w", id, getErr)
		}
		children[index] = child
	}
	return children, nil
}

func (s *Store) ListBlockingTaskIDs(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.boundary.Query(ctx, `SELECT dependency.id
		FROM task_dependencies edge
		JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
			AND dependency.id=edge.depends_on_task_id
		WHERE edge.workspace_id=$1 AND edge.task_id=$2 AND dependency.state<>'merged'
		ORDER BY dependency.id`, workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Store) ValidateTaskDependencies(ctx context.Context, dependencyIDs []string) error {
	seen := map[string]bool{}
	for _, dependencyID := range dependencyIDs {
		dependencyID = strings.TrimSpace(dependencyID)
		if dependencyID == "" || seen[dependencyID] {
			return fmt.Errorf("depends_on contains an empty or duplicate task id")
		}
		seen[dependencyID] = true
		var dependencyWorkspace, dependencyState string
		if err := s.boundary.QueryRow(ctx, `SELECT workspace_id, state FROM tasks WHERE id=$1`, dependencyID).
			Scan(&dependencyWorkspace, &dependencyState); err != nil {
			return notFound(err, "dependency task %s", dependencyID)
		}
		if dependencyWorkspace != workspace(ctx) {
			return fmt.Errorf("dependency task %s belongs to another workspace", dependencyID)
		}
		if core.TaskTerminal(core.TaskState(dependencyState)) {
			return fmt.Errorf("dependency task %s is not open", dependencyID)
		}
	}
	return nil
}

func (s *Store) ListDependentTaskIDs(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.boundary.Query(ctx, `SELECT task_id FROM task_dependencies
		WHERE workspace_id=$1 AND depends_on_task_id=$2 ORDER BY task_id`, workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Store) ListDependencyBlockers(ctx context.Context, taskIDs []string) (map[string]store.DependencyBlockers, error) {
	if len(taskIDs) == 0 {
		return map[string]store.DependencyBlockers{}, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT edge.task_id, dependency.id, dependency.state
		FROM task_dependencies edge
		JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
			AND dependency.id=edge.depends_on_task_id
		WHERE edge.workspace_id=$1 AND edge.task_id=ANY($2::text[])
			AND dependency.state<>'merged'
		ORDER BY edge.task_id, dependency.id`, workspace(ctx), taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]store.DependencyBlockers{}
	for rows.Next() {
		var taskID, dependencyID, state string
		if err = rows.Scan(&taskID, &dependencyID, &state); err != nil {
			return nil, err
		}
		blockers := result[taskID]
		blockers.BlockingTaskIDs = append(blockers.BlockingTaskIDs, dependencyID)
		if core.TaskTerminal(core.TaskState(state)) {
			blockers.UnsatisfiableTaskIDs = append(blockers.UnsatisfiableTaskIDs, dependencyID)
		}
		result[taskID] = blockers
	}
	return result, rows.Err()
}

func (s *Store) AddTaskDependency(ctx context.Context, request store.DependencyAdditionRequest) (store.DependencyAdditionResult, error) {
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.DependsOnTaskID = strings.TrimSpace(request.DependsOnTaskID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.TaskID == "" || request.DependsOnTaskID == "" || request.Reason == "" || request.RequestID == "" {
		return store.DependencyAdditionResult{}, fmt.Errorf("task_id, depends_on_task_id, reason, and request_id are required")
	}
	if request.TaskID == request.DependsOnTaskID {
		return store.DependencyAdditionResult{}, fmt.Errorf("%w: task cannot depend on itself", store.ErrTaskDependencyConflict)
	}
	actor := store.ActorFromContext(ctx)
	added := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockDependencyEdgesTx(ctx, tx, workspace(ctx)); err != nil {
			return err
		}
		var prior store.DependencyAdditionRequest
		var actorID, actorRole string
		var priorAdded bool
		err := tx.QueryRow(ctx, `SELECT task_id,depends_on_task_id,reason,request_id,actor_id,actor_role,added
			FROM task_dependency_additions WHERE workspace_id=$1 AND request_id=$2`,
			workspace(ctx), request.RequestID).
			Scan(&prior.TaskID, &prior.DependsOnTaskID, &prior.Reason, &prior.RequestID, &actorID, &actorRole, &priorAdded)
		if err == nil {
			if prior != request || actorID != actor.ID || actorRole != string(actor.Role) {
				return fmt.Errorf("%w: request_id %s was already used for different dependency addition inputs", store.ErrTaskDependencyConflict, request.RequestID)
			}
			added = priorAdded
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		states := map[string]core.TaskState{}
		rows, err := tx.Query(ctx, `SELECT id,state FROM tasks
			WHERE workspace_id=$1 AND id=ANY($2::text[]) ORDER BY id FOR UPDATE`,
			workspace(ctx), []string{request.TaskID, request.DependsOnTaskID})
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, state string
			if err = rows.Scan(&id, &state); err != nil {
				rows.Close()
				return err
			}
			states[id] = core.TaskState(state)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if _, exists := states[request.TaskID]; !exists {
			return fmt.Errorf("task %s: %w", request.TaskID, store.ErrNotFound)
		}
		if _, exists := states[request.DependsOnTaskID]; !exists {
			return fmt.Errorf("dependency task %s: %w", request.DependsOnTaskID, store.ErrNotFound)
		}
		if core.TaskTerminal(states[request.TaskID]) {
			return fmt.Errorf("task %s is not open: %w", request.TaskID, store.ErrTaskTerminal)
		}
		if core.TaskTerminal(states[request.DependsOnTaskID]) {
			return fmt.Errorf("dependency task %s is not open: %w", request.DependsOnTaskID, store.ErrTaskTerminal)
		}

		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_dependencies
			WHERE workspace_id=$1 AND task_id=$2 AND depends_on_task_id=$3)`,
			workspace(ctx), request.TaskID, request.DependsOnTaskID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			var cycle bool
			if err = tx.QueryRow(ctx, `WITH RECURSIVE reachable(id) AS (
				SELECT depends_on_task_id FROM task_dependencies WHERE workspace_id=$1 AND task_id=$2
				UNION
				SELECT edge.depends_on_task_id FROM task_dependencies edge
				JOIN reachable ON reachable.id=edge.task_id WHERE edge.workspace_id=$1
			) SELECT EXISTS (SELECT 1 FROM reachable WHERE id=$3)`,
				workspace(ctx), request.DependsOnTaskID, request.TaskID).Scan(&cycle); err != nil {
				return err
			}
			if cycle {
				return fmt.Errorf("%w: %s already reaches %s", store.ErrTaskDependencyCycle, request.DependsOnTaskID, request.TaskID)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO task_dependencies (workspace_id,task_id,depends_on_task_id)
				VALUES ($1,$2,$3)`, workspace(ctx), request.TaskID, request.DependsOnTaskID); err != nil {
				return err
			}
			added = true
		}
		now := time.Now().UTC()
		if _, err = tx.Exec(ctx, `INSERT INTO task_dependency_additions
			(workspace_id,request_id,task_id,depends_on_task_id,reason,actor_id,actor_role,added,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			workspace(ctx), request.RequestID, request.TaskID, request.DependsOnTaskID,
			request.Reason, actor.ID, actor.Role, added, now); err != nil {
			return err
		}
		if !added {
			return nil
		}
		if err = insertEvent(ctx, q, core.Event{
			TaskID: request.TaskID, Kind: "task.dependency_added", ActorID: actor.ID, ActorRole: actor.Role, At: now,
			Payload: core.JSONPayload(map[string]any{
				"task_id": request.TaskID, "depends_on_task_id": request.DependsOnTaskID,
				"reason": request.Reason, "request_id": request.RequestID,
			}),
		}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE work_orders SET queue_blocked_at=$1,updated_at=$1
			WHERE workspace_id=$2 AND task_id=$3 AND stage IN ('implement','verify')
				AND state='queued' AND queue_blocked_at IS NULL`, now, workspace(ctx), request.TaskID)
		return err
	})
	if err != nil {
		return store.DependencyAdditionResult{}, err
	}
	task, err := s.GetTask(ctx, request.TaskID)
	if err != nil {
		return store.DependencyAdditionResult{}, err
	}
	return store.DependencyAdditionResult{Task: task, RequestID: request.RequestID, Added: added}, nil
}

func (s *Store) RemoveTaskDependency(ctx context.Context, request store.DependencyRemovalRequest) (store.DependencyRemovalResult, error) {
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.DependsOnTaskID = strings.TrimSpace(request.DependsOnTaskID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.TaskID == "" || request.DependsOnTaskID == "" || request.Reason == "" || request.RequestID == "" {
		return store.DependencyRemovalResult{}, fmt.Errorf("task_id, depends_on_task_id, reason, and request_id are required")
	}
	actor := store.ActorFromContext(ctx)
	removed := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		key := "conveyor:dependency-remove:" + workspace(ctx) + ":" + request.TaskID
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
			return err
		}
		var prior store.DependencyRemovalRequest
		var actorID, actorRole string
		err := tx.QueryRow(ctx, `SELECT task_id,depends_on_task_id,reason,request_id,actor_id,actor_role
			FROM task_dependency_removals WHERE workspace_id=$1 AND request_id=$2`,
			workspace(ctx), request.RequestID).
			Scan(&prior.TaskID, &prior.DependsOnTaskID, &prior.Reason, &prior.RequestID, &actorID, &actorRole)
		if err == nil {
			if prior != request || actorID != actor.ID || actorRole != string(actor.Role) {
				return fmt.Errorf("request_id %s was already used for different dependency removal inputs", request.RequestID)
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		command, err := tx.Exec(ctx, `DELETE FROM task_dependencies
			WHERE workspace_id=$1 AND task_id=$2 AND depends_on_task_id=$3`,
			workspace(ctx), request.TaskID, request.DependsOnTaskID)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("dependency edge %s -> %s not found", request.TaskID, request.DependsOnTaskID)
		}
		now := time.Now().UTC()
		if _, err = tx.Exec(ctx, `INSERT INTO task_dependency_removals
			(workspace_id,request_id,task_id,depends_on_task_id,reason,actor_id,actor_role,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			workspace(ctx), request.RequestID, request.TaskID, request.DependsOnTaskID,
			request.Reason, actor.ID, actor.Role, now); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{
			TaskID: request.TaskID, Kind: "task.dependency_removed", ActorID: actor.ID, ActorRole: actor.Role, At: now,
			Payload: core.JSONPayload(map[string]any{
				"task_id": request.TaskID, "depends_on_task_id": request.DependsOnTaskID,
				"actor": actor.ID, "reason": request.Reason, "request_id": request.RequestID,
			}),
		}); err != nil {
			return err
		}
		if err = s.resumeDependencyQueueClocksTx(ctx, tx, request.TaskID, now); err != nil {
			return err
		}
		removed = true
		return nil
	})
	if err != nil {
		return store.DependencyRemovalResult{}, err
	}
	task, err := s.GetTask(ctx, request.TaskID)
	if err != nil {
		return store.DependencyRemovalResult{}, err
	}
	return store.DependencyRemovalResult{Task: task, RequestID: request.RequestID, Removed: removed}, nil
}

func (s *Store) CreateIntervention(ctx context.Context, intervention core.Intervention) error {
	if !intervention.Action.Valid() {
		return fmt.Errorf("invalid intervention action %q", intervention.Action)
	}
	actor := store.ActorFromContext(ctx)
	if intervention.ActorID == "" {
		intervention.ActorID = actor.ID
	}
	if intervention.ActorRole == "" {
		intervention.ActorRole = actor.Role
	}
	if intervention.At.IsZero() {
		intervention.At = time.Now().UTC()
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		_, err := q.GetTask(ctx, db.GetTaskParams{ID: intervention.TaskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", intervention.TaskID)
		}
		if intervention.JobID != "" {
			job, err := q.GetJob(ctx, db.GetJobParams{ID: intervention.JobID, WorkspaceID: workspace(ctx)})
			if err != nil || job.TaskID != intervention.TaskID {
				return fmt.Errorf("job %s does not belong to task %s in workspace %s", intervention.JobID, intervention.TaskID, workspace(ctx))
			}
		}
		if _, err := q.InsertIntervention(ctx, interventionInsertParams(intervention)); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{
			TaskID:    intervention.TaskID,
			JobID:     intervention.JobID,
			Kind:      "intervention." + string(intervention.Action),
			ActorID:   intervention.ActorID,
			ActorRole: intervention.ActorRole,
			Payload: core.JSONPayload(map[string]any{
				"reason_code": intervention.ReasonCode,
				"comment":     intervention.Comment,
			}),
			At: intervention.At,
		}); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) CancelTaskCommand(ctx context.Context, lease taskops.TaskLease, intervention core.Intervention) (core.Task, error) {
	if !lease.ValidFor(intervention.TaskID) {
		return core.Task{}, fmt.Errorf("task cancellation requires a valid taskops lease")
	}
	if intervention.Action != core.InterventionCancel || strings.TrimSpace(intervention.ReasonCode) == "" {
		return core.Task{}, fmt.Errorf("cancel intervention requires a reason")
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error { return s.cancelTaskTx(ctx, tx, q, intervention) })
	if err != nil {
		return core.Task{}, err
	}
	return s.GetTask(ctx, intervention.TaskID)
}

func (s *Store) cancelTaskTx(ctx context.Context, tx pgx.Tx, q *db.Queries, intervention core.Intervention) error {
	var priorState string
	err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), intervention.TaskID).Scan(&priorState)
	if err != nil {
		return notFound(err, "task %s", intervention.TaskID)
	}
	if core.TaskTerminal(core.TaskState(priorState)) {
		return store.ErrTaskTerminal
	}
	taskState, transitionErr := core.TransitionTask(core.TaskState(priorState), core.TaskCancel)
	if transitionErr != nil {
		return transitionErr
	}
	actor := store.ActorFromContext(ctx)
	if intervention.ActorID == "" {
		intervention.ActorID = actor.ID
	}
	if intervention.ActorRole == "" {
		intervention.ActorRole = actor.Role
	}
	if intervention.At.IsZero() {
		intervention.At = time.Now().UTC()
	}
	if intervention.JobID != "" {
		job, getErr := q.GetJob(ctx, db.GetJobParams{ID: intervention.JobID, WorkspaceID: workspace(ctx)})
		if getErr != nil || job.TaskID != intervention.TaskID {
			return fmt.Errorf("job %s does not belong to task %s", intervention.JobID, intervention.TaskID)
		}
	}
	if _, err = q.InsertIntervention(ctx, interventionInsertParams(intervention)); err != nil {
		return err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: intervention.TaskID, JobID: intervention.JobID, Kind: "intervention.cancel", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"reason_code": intervention.ReasonCode, "comment": intervention.Comment}), At: intervention.At}); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id,job_id,state,attempt_id,session_id FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND state NOT IN ('completed','cancelled') FOR UPDATE`, workspace(ctx), intervention.TaskID)
	if err != nil {
		return err
	}
	type cancelledOrder struct {
		id, jobID, attemptID, sessionID string
		state                           core.WorkOrderState
	}
	var orders []cancelledOrder
	for rows.Next() {
		var order cancelledOrder
		if err = rows.Scan(&order.id, &order.jobID, &order.state, &order.attemptID, &order.sessionID); err != nil {
			rows.Close()
			return err
		}
		if _, transitionErr = core.TransitionWorkOrder(order.state, core.WorkOrderCmdCancel); transitionErr != nil {
			rows.Close()
			return transitionErr
		}
		orders = append(orders, order)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var cancelled, jobIDs []string
	for _, order := range orders {
		if _, err = tx.Exec(ctx, `UPDATE work_orders SET state='cancelled',
			last_attempt_outcome=CASE WHEN state='claimed' THEN 'cancelled' ELSE last_attempt_outcome END,
			last_attempt_id=CASE WHEN attempt_id<>'' THEN attempt_id ELSE last_attempt_id END,
			attempt_id='',
			claimant_id=CASE WHEN attempt_id<>'' THEN '' ELSE claimant_id END,
			session_id=CASE WHEN attempt_id<>'' THEN '' ELSE session_id END,
			client_token_hash=CASE WHEN attempt_id<>'' THEN '' ELSE client_token_hash END,
			agent=CASE WHEN attempt_id<>'' THEN '' ELSE agent END,
			model=CASE WHEN attempt_id<>'' THEN '' ELSE model END,
			worker_id=CASE WHEN attempt_id<>'' THEN '' ELSE worker_id END,
			model_enforcement=CASE WHEN attempt_id<>'' THEN '' ELSE model_enforcement END,
			lease_expires_at=CASE WHEN attempt_id<>'' THEN NULL ELSE lease_expires_at END,
			execution_started_at=CASE WHEN attempt_id<>'' THEN NULL ELSE execution_started_at END,
			execution_deadline=CASE WHEN attempt_id<>'' THEN NULL ELSE execution_deadline END,
			updated_at=$1 WHERE workspace_id=$2 AND id=$3 AND state=$4`, intervention.At, workspace(ctx), order.id, order.state); err != nil {
			return err
		}
		cancelled, jobIDs = append(cancelled, order.id), append(jobIDs, order.jobID)
		if err = insertEvent(ctx, q, core.Event{TaskID: intervention.TaskID, JobID: order.jobID, Kind: "work_order.cancelled", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"id": order.id, "attempt_id": order.attemptID, "session_id": order.sessionID, "state": core.WorkOrderCancelled, "from": order.state, "command": core.WorkOrderCmdCancel}), At: intervention.At}); err != nil {
			return err
		}
	}
	if len(jobIDs) != 0 {
		if _, err = tx.Exec(ctx, `UPDATE jobs SET state='failed',ended_at=$1,updated_at=$1 WHERE task_id=$2 AND id=ANY($3) AND state<>'done'`, intervention.At, intervention.TaskID, jobIDs); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE tasks SET state=$1,next_stage='',recovery_stage='',updated_at=$2 WHERE workspace_id=$3 AND id=$4 AND state=$5`, taskState, intervention.At, workspace(ctx), intervention.TaskID, priorState); err != nil {
		return err
	}
	if err = deleteProposedTaskContextTx(ctx, tx, intervention.TaskID); err != nil {
		return err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: intervention.TaskID, Kind: "task.state_changed", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"from": priorState, "to": taskState, "command": core.TaskCancel}), At: intervention.At}); err != nil {
		return err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: intervention.TaskID, Kind: "task.cancelled", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"actor": intervention.ActorID, "reason": intervention.ReasonCode, "comment": intervention.Comment, "from": priorState, "cancelled_work_orders": cancelled}), At: intervention.At}); err != nil {
		return err
	}
	if err = s.recordDependencyOutcomeTx(ctx, tx, intervention.TaskID, taskState, intervention.At); err != nil {
		return err
	}
	return nil
}

func (s *Store) ListInterventions(ctx context.Context, taskID string) ([]core.Intervention, error) {
	rows, err := s.queries.ListInterventions(ctx, db.ListInterventionsParams{TaskID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return nil, err
	}
	result := make([]core.Intervention, len(rows))
	for i := range rows {
		result[i] = interventionFromDB(rows[i])
	}
	return result, nil
}

func lockDependencyEdgesTx(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	key := "conveyor:dependency-edges:" + workspaceID
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
		return fmt.Errorf("lock dependency edges in workspace %s: %w", workspaceID, err)
	}
	return nil
}

func taskInsertParams(task core.Task) db.InsertTaskParams {
	return db.InsertTaskParams{
		ID: task.ID, WorkspaceID: task.Workspace, Source: task.Source,
		Title: task.Title, Body: task.Body, Class: task.Class,
		EscalationLevel: string(task.Level), RepoName: task.Repo,
		Mode: string(task.Mode), Hold: task.Hold, SpecApproval: task.SpecApproval, MergeApproval: task.MergeApproval,
		PolicyVersion: int32(task.PolicyVersion),
		SetupName:     task.SetupName, SetupContract: setupContractJSON(task.SetupContract),
		ReviewedHeadSha: task.ReviewedHeadSHA, ApprovedHeadSha: task.ApprovedHeadSHA, ApprovalStale: task.ApprovalStale,
		RefreshBaselineSha: task.RefreshBaselineSHA, RefreshHeadSha: task.RefreshHeadSHA, RefreshReviewScope: task.RefreshReviewScope,
		BaseBranch: task.BaseBranch, Branch: task.Branch, State: string(task.State),
		NextStage: string(task.NextStage), RecoveryStage: string(task.RecoveryStage),
		ParentTaskID: nullableText(task.ParentTaskID), OriginSpecVersion: int32(task.OriginSpecVersion), OriginSubID: task.OriginSubID,
		IntakeKey: nullableText(task.IntakeKey), CreatedAt: timestamp(task.CreatedAt),
	}
}

func taskFromDB(task db.Task) core.Task {
	var setup config.ExecutionSetup
	_ = json.Unmarshal(task.SetupContract, &setup)
	return core.Task{
		ID: task.ID, Workspace: task.WorkspaceID, Source: task.Source, IntakeKey: task.IntakeKey.String,
		Supersedes: task.Supersedes.String, SupersededBy: task.SupersededBy.String, IntakeOperatorDirection: task.IntakeOperatorDirection,
		Title: task.Title, Body: task.Body, Class: task.Class,
		Level: core.EscalationLevel(task.EscalationLevel), Repo: task.RepoName,
		Mode: core.TaskMode(task.Mode), Hold: task.Hold, SpecApproval: task.SpecApproval, MergeApproval: task.MergeApproval,
		PolicyVersion: int(task.PolicyVersion),
		SetupName:     task.SetupName, SetupContract: setup,
		ReviewedHeadSHA: task.ReviewedHeadSha, ApprovedHeadSHA: task.ApprovedHeadSha, ApprovalStale: task.ApprovalStale,
		RefreshBaselineSHA: task.RefreshBaselineSha, RefreshHeadSHA: task.RefreshHeadSha, RefreshReviewScope: task.RefreshReviewScope,
		BaseBranch: task.BaseBranch, Branch: task.Branch,
		State: core.TaskState(task.State), NextStage: core.Stage(task.NextStage), RecoveryStage: core.Stage(task.RecoveryStage),
		ParentTaskID: task.ParentTaskID.String, OriginSpecVersion: int(task.OriginSpecVersion), OriginSubID: task.OriginSubID,
		CreatedAt: task.CreatedAt.Time,
	}
}

func setupContractJSON(setup config.ExecutionSetup) []byte {
	data, _ := json.Marshal(setup)
	return data
}

func jobInsertParams(job core.Job) db.InsertJobParams {
	return db.InsertJobParams{
		ID: job.ID, TaskID: job.TaskID, Stage: string(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, AuthMode: job.AuthMode, Runner: job.Runner,
		PackVersion: job.PackVersion, ConfinementTier: job.Confinement,
		CostUsd: nullableFloat(job.CostUSD), TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: string(job.State),
		StartedAt: nullableTimestamp(job.StartedAt), EndedAt: nullableTimestamp(job.EndedAt),
	}
}

func jobUpdateParams(job core.Job, workspace string) db.UpdateJobParams {
	return db.UpdateJobParams{
		ID: job.ID, Stage: string(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, AuthMode: job.AuthMode, Runner: job.Runner,
		PackVersion: job.PackVersion, ConfinementTier: job.Confinement,
		CostUsd: nullableFloat(job.CostUSD), TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: string(job.State),
		StartedAt: nullableTimestamp(job.StartedAt), EndedAt: nullableTimestamp(job.EndedAt),
		WorkspaceID: workspace,
	}
}

func jobFromDB(job db.Job) core.Job {
	return core.Job{
		ID: job.ID, TaskID: job.TaskID, Stage: core.Stage(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, AuthMode: job.AuthMode, Runner: job.Runner,
		PackVersion: job.PackVersion, Confinement: job.ConfinementTier,
		CostUSD: floatPointer(job.CostUsd), TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: core.JobState(job.State),
		StartedAt: job.StartedAt.Time, EndedAt: nullableTime(job.EndedAt),
	}
}

func interventionInsertParams(intervention core.Intervention) db.InsertInterventionParams {
	return db.InsertInterventionParams{
		TaskID: intervention.TaskID, JobID: nullableText(intervention.JobID),
		ActorID: intervention.ActorID, ActorRole: string(intervention.ActorRole),
		Action: string(intervention.Action), ReasonCode: intervention.ReasonCode,
		Comment: intervention.Comment, At: timestamp(intervention.At),
	}
}

func interventionFromDB(intervention db.Intervention) core.Intervention {
	return core.Intervention{
		ID: intervention.ID, TaskID: intervention.TaskID, JobID: intervention.JobID.String,
		ActorID: intervention.ActorID, ActorRole: core.ActorRole(intervention.ActorRole),
		Action: core.InterventionAction(intervention.Action), ReasonCode: intervention.ReasonCode,
		Comment: intervention.Comment, At: intervention.At.Time,
	}
}
