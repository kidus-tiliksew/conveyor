package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// All task command writers use taskTx and writeRow. The command lock shares
// the callback identity (DEC-38, component-persistence).
func (s *Store) taskTx(ctx context.Context, id string, fn func(*sql.Tx) error) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockTaskOperation(ctx, tx, ws, id); err != nil {
			return err
		}
		return fn(tx)
	})
}
func taskWrite(ctx context.Context, tx *sql.Tx, id string, values map[string]any) error {
	values["updated_at"] = time.Now().UTC()
	_, err := writeRow(ctx, tx, rowWrite{table: "tasks", operation: "UPDATE", values: values, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": id}})
	return err
}
func taskEvent(ctx context.Context, tx *sql.Tx, e core.Event) error {
	actor := store.ActorFromContext(ctx)
	// PostgreSQL rejects NUL in text. Enforce the reference input domain here,
	// after the projection write, so a rejected audit insert rolls back the tx.
	if strings.ContainsRune(actor.ID, 0) || strings.ContainsRune(e.ActorID, 0) {
		return fmt.Errorf("audit actor contains NUL")
	}
	return insertEvent(ctx, tx, e)
}

const taskColumns = `id,workspace_id,source,COALESCE(intake_key,''),title,body,class,escalation_level,mode,hold,spec_approval,merge_approval,policy_version,setup_name,setup_contract,reviewed_head_sha,approved_head_sha,approval_stale,refresh_baseline_sha,refresh_head_sha,refresh_review_scope,repo_name,base_branch,branch,state,next_stage,recovery_stage,COALESCE(parent_task_id,''),origin_spec_version,origin_sub_id,COALESCE(feature_id,''),created_at,COALESCE(assignee_user_id,'')`

func scanTask(row interface{ Scan(...any) error }) (core.Task, error) {
	var t core.Task
	var setup []byte
	var assignee string
	err := row.Scan(&t.ID, &t.Workspace, &t.Source, &t.IntakeKey, &t.Title, &t.Body, &t.Class, &t.Level, &t.Mode, &t.Hold, &t.SpecApproval, &t.MergeApproval, &t.PolicyVersion, &t.SetupName, &setup, &t.ReviewedHeadSHA, &t.ApprovedHeadSHA, &t.ApprovalStale, &t.RefreshBaselineSHA, &t.RefreshHeadSHA, &t.RefreshReviewScope, &t.Repo, &t.BaseBranch, &t.Branch, &t.State, &t.NextStage, &t.RecoveryStage, &t.ParentTaskID, &t.OriginSpecVersion, &t.OriginSubID, &t.FeatureID, &t.CreatedAt, &assignee)
	if err != nil {
		return core.Task{}, err
	}
	if len(setup) > 0 {
		if err = json.Unmarshal(setup, &t.SetupContract); err != nil {
			return core.Task{}, err
		}
	}
	if assignee != "" {
		t.Assignee = &core.TaskAssignee{UserID: assignee}
	}
	return t, nil
}
func getTaskRow(ctx context.Context, db s2log.Executor, id string) (core.Task, error) {
	t, err := scanTask(documentRow(ctx, db, `SELECT `+taskColumns+` FROM tasks WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), id))
	return t, notFound(err, "task %s", id)
}
func (s *Store) CreateTask(ctx context.Context, t core.Task) error {
	return s.CreateTaskWithDependencies(ctx, t, nil)
}
func (s *Store) CreateTaskWithDependencies(ctx context.Context, t core.Task, ids []string) error {
	return s.CreateTaskWithDependenciesAndContext(ctx, t, ids, store.TaskContextInput{})
}
func (s *Store) CreateTaskWithDependenciesAndContext(ctx context.Context, t core.Task, ids []string, attached store.TaskContextInput) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	if t.Workspace != ws {
		return fmt.Errorf("task workspace %q does not match store workspace %q", t.Workspace, ws)
	}
	attached, err = store.NormalizeTaskContextInput(attached)
	if err != nil {
		return err
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.NextStage == "" && (t.State == core.TaskQueued || t.State == core.TaskClaiming) {
		t.NextStage = core.InitialStage(t.Level)
	}
	return s.taskTx(ctx, t.ID, func(tx *sql.Tx) error {
		if err := workerWorkspaceExists(ctx, tx, ws); err != nil {
			return err
		}
		if t.ParentTaskID != "" {
			if err := documentParent(ctx, tx, "tasks", t.ParentTaskID); err != nil {
				return err
			}
		}
		if t.FeatureID != "" {
			var found int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM features WHERE workspace_id=? AND id=?`, ws, t.FeatureID).Scan(&found); err != nil {
				return notFound(err, "feature %s", t.FeatureID)
			}
		}
		if len(ids) > 0 {
			if err := lockDependencyEdgesTx(ctx, tx, ws); err != nil {
				return err
			}
		}
		seen := map[string]bool{}
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				return fmt.Errorf("depends_on contains an empty or duplicate task id")
			}
			seen[id] = true
			d, err := getTaskRow(ctx, tx, id)
			if err != nil {
				return err
			}
			if core.TaskTerminal(d.State) {
				return fmt.Errorf("dependency task %s is not open", id)
			}
		}
		versions, err := validateTaskContextTx(ctx, tx, ws, attached)
		if err != nil {
			return err
		}
		if err = insertTaskRow(ctx, tx, t); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, Kind: "task.created", Payload: core.JSONPayload(t), At: t.CreatedAt}); err != nil {
			return err
		}
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if _, err = tx.ExecContext(ctx, `INSERT INTO task_dependencies(workspace_id,task_id,depends_on_task_id) VALUES (?,?,?)`, ws, t.ID, id); err != nil {
				return err
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, Kind: "task.dependency_added", At: t.CreatedAt, Payload: core.JSONPayload(map[string]string{"task_id": t.ID, "depends_on_task_id": id})}); err != nil {
				return err
			}
		}
		for _, id := range attached.RequirementIDs {
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, Kind: store.TaskContextRequirementAdded, At: t.CreatedAt, Payload: core.JSONPayload(map[string]any{"id": id})}); err != nil {
				return err
			}
		}
		for _, id := range attached.DesignIDs {
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, Kind: store.TaskContextDesignAdded, At: t.CreatedAt, Payload: core.JSONPayload(map[string]any{"id": id, "version": versions[id]})}); err != nil {
				return err
			}
		}
		if t.State == core.TaskQueued {
			_, err = s.enqueueTaskTx(ctx, tx, t.ID, ws)
		}
		return err
	})
}
func insertTaskRow(ctx context.Context, tx *sql.Tx, t core.Task) error {
	for _, key := range []string{"task-id:" + t.ID, "task-branch:" + t.Branch, "task-intake:" + t.Workspace + ":" + t.IntakeKey} {
		if err := lockKey(ctx, tx, key); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id=? OR branch=? OR (workspace_id=? AND intake_key=? AND ?<>'')`, t.ID, t.Branch, t.Workspace, t.IntakeKey, t.IntakeKey).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("task identifier, branch or intake key already exists")
	}
	setup, err := json.Marshal(t.SetupContract)
	if err != nil {
		return err
	}
	_, err = writeRow(ctx, tx, rowWrite{table: "tasks", operation: "INSERT", values: map[string]any{
		"id":                   t.ID,
		"workspace_id":         t.Workspace,
		"source":               t.Source,
		"intake_key":           nullString(t.IntakeKey),
		"title":                t.Title,
		"body":                 t.Body,
		"class":                t.Class,
		"escalation_level":     t.Level,
		"mode":                 t.Mode,
		"hold":                 t.Hold,
		"spec_approval":        t.SpecApproval,
		"merge_approval":       t.MergeApproval,
		"policy_version":       t.PolicyVersion,
		"setup_name":           t.SetupName,
		"setup_contract":       setup,
		"reviewed_head_sha":    t.ReviewedHeadSHA,
		"approved_head_sha":    t.ApprovedHeadSHA,
		"approval_stale":       t.ApprovalStale,
		"refresh_baseline_sha": t.RefreshBaselineSHA,
		"refresh_head_sha":     t.RefreshHeadSHA,
		"refresh_review_scope": t.RefreshReviewScope,
		"repo_name":            t.Repo,
		"base_branch":          t.BaseBranch,
		"branch":               t.Branch,
		"state":                t.State,
		"next_stage":           t.NextStage,
		"recovery_stage":       t.RecoveryStage,
		"parent_task_id":       nullString(t.ParentTaskID),
		"origin_spec_version":  t.OriginSpecVersion,
		"origin_sub_id":        t.OriginSubID,
		"feature_id":           nullString(t.FeatureID),
		"created_at":           t.CreatedAt,
	}})
	return err
}
func (s *Store) GetTask(ctx context.Context, id string) (core.Task, error) {
	t, err := getTaskRow(ctx, s.db, id)
	if err != nil {
		return core.Task{}, err
	}
	if err = s.hydrateTaskRelations(ctx, &t); err != nil {
		return core.Task{}, err
	}
	return t, nil
}
func (s *Store) GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error) {
	t, err := scanTask(documentRow(ctx, s.db, `SELECT `+taskColumns+` FROM tasks WHERE workspace_id=? AND intake_key=?`, documentWorkspace(ctx), key))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Task{}, false, nil
	}
	if err != nil {
		return core.Task{}, false, err
	}
	err = s.hydrateTaskRelations(ctx, &t)
	return t, err == nil, err
}
func (s *Store) hydrateTaskRelations(ctx context.Context, t *core.Task) error {
	rows, err := documentRows(ctx, s.db, `SELECT d.id,d.title,d.state,d.origin_spec_version,d.origin_sub_id FROM task_dependencies e JOIN tasks d ON d.workspace_id=e.workspace_id AND d.id=e.depends_on_task_id WHERE e.workspace_id=? AND e.task_id=? ORDER BY d.id`, documentWorkspace(ctx), t.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r core.TaskRelation
		if err = rows.Scan(&r.ID, &r.Title, &r.State, &r.OriginSpecVersion, &r.OriginSubID); err != nil {
			rows.Close()
			return err
		}
		t.Dependencies = append(t.Dependencies, r)
		if r.State != core.TaskMerged {
			t.BlockingTaskIDs = append(t.BlockingTaskIDs, r.ID)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = documentRows(ctx, s.db, `SELECT id,title,state,origin_spec_version,origin_sub_id FROM tasks WHERE workspace_id=? AND parent_task_id=? ORDER BY origin_spec_version,origin_sub_id,id`, documentWorkspace(ctx), t.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r core.TaskRelation
		if err = rows.Scan(&r.ID, &r.Title, &r.State, &r.OriginSpecVersion, &r.OriginSubID); err != nil {
			rows.Close()
			return err
		}
		t.Children = append(t.Children, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if t.Assignee != nil {
		err = documentRow(ctx, s.db, `SELECT u.email,u.display_name FROM users u JOIN workspace_role_bindings b ON b.user_id=u.id WHERE b.workspace_id=? AND u.id=?`, documentWorkspace(ctx), t.Assignee.UserID).Scan(&t.Assignee.Email, &t.Assignee.DisplayName)
		if errors.Is(err, sql.ErrNoRows) {
			t.Assignee = nil
			err = nil
		}
		if err != nil {
			return err
		}
	}
	// Task reads include the persisted forge projection; forge writers remain sibling-owned.
	var lifecycle core.GitHubLifecycle
	err = documentRow(ctx, s.db, `SELECT task_id,repository,spec_version,source,source_issue_number,issue_number,issue_url,outcome,state,create_state,create_attempts,reconcile_misses,attempts,forge_error_category,last_error,forge_author_class,forge_author_user_id,created_at,updated_at FROM github_lifecycles WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), t.ID).Scan(&lifecycle.TaskID, &lifecycle.Repository, &lifecycle.SpecVersion, &lifecycle.Source, &lifecycle.SourceIssueNumber, &lifecycle.IssueNumber, &lifecycle.IssueURL, &lifecycle.Outcome, &lifecycle.State, &lifecycle.CreateState, &lifecycle.CreateAttempts, &lifecycle.ReconcileMisses, &lifecycle.Attempts, &lifecycle.ForgeErrorCategory, &lifecycle.LastError, &lifecycle.ForgeAuthorClass, &lifecycle.ForgeAuthorUserID, &lifecycle.CreatedAt, &lifecycle.UpdatedAt)
	if err == nil {
		t.GitHub = &lifecycle
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}
func (s *Store) SetTaskHold(ctx context.Context, id string, hold bool) (core.Task, error) {
	var result core.Task
	err := s.taskTx(ctx, id, func(tx *sql.Tx) error {
		var err error
		result, err = getTaskRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if result.Hold == hold {
			return nil
		}
		if err = taskWrite(ctx, tx, id, map[string]any{"hold": hold}); err != nil {
			return err
		}
		result.Hold = hold
		kind := "task.hold.set"
		if !hold {
			kind = "task.hold.cleared"
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: kind, Payload: core.JSONPayload(map[string]any{"hold": hold})})
	})
	return result, err
}
func (s *Store) UpdateTaskClassification(ctx context.Context, id, class string) error {
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		if _, err := getTaskRow(ctx, tx, id); err != nil {
			return err
		}
		if err := taskWrite(ctx, tx, id, map[string]any{"class": class}); err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "task.classified", Payload: core.JSONPayload(map[string]any{"class": class})})
	})
}

func approvalValues(head string) map[string]any {
	return map[string]any{"reviewed_head_sha": head, "approved_head_sha": head, "approval_stale": false, "refresh_baseline_sha": "", "refresh_head_sha": "", "refresh_review_scope": ""}
}
func (s *Store) BindTaskApproval(ctx context.Context, id, head string) error {
	head = strings.TrimSpace(head)
	if head == "" {
		return fmt.Errorf("approved head SHA is required")
	}
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		t, err := getTaskRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = taskWrite(ctx, tx, id, approvalValues(head)); err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "approval.bound", Payload: core.JSONPayload(map[string]any{"workspace": t.Workspace, "task_id": id, "approved_head": head})})
	})
}
func (s *Store) MarkTaskApprovalStale(ctx context.Context, id, approved, newHead, scope, reason string) (bool, error) {
	if approved == "" || newHead == "" || approved == newHead {
		return false, fmt.Errorf("distinct approved and new head SHAs are required")
	}
	created := false
	err := s.taskTx(ctx, id, func(tx *sql.Tx) error {
		t, err := getTaskRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if t.ApprovalStale && t.RefreshBaselineSHA == approved && t.RefreshHeadSHA == newHead {
			return nil
		}
		if err = taskWrite(ctx, tx, id, map[string]any{"approved_head_sha": approved, "approval_stale": true, "refresh_baseline_sha": approved, "refresh_head_sha": newHead, "refresh_review_scope": scope}); err != nil {
			return err
		}
		created = true
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "approval.stale", Payload: core.JSONPayload(map[string]any{"workspace": t.Workspace, "task_id": id, "reason_code": reason, "approved_head": approved, "new_head": newHead, "review_scope": scope})})
	})
	return created, err
}
func (s *Store) AdvanceTaskRefreshHead(ctx context.Context, id, head string) error {
	head = strings.TrimSpace(head)
	if head == "" {
		return fmt.Errorf("new head SHA is required")
	}
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		t, err := getTaskRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if !t.ApprovalStale {
			return fmt.Errorf("task %s has no stale approval to refresh", id)
		}
		if t.RefreshHeadSHA == head {
			return nil
		}
		if err = taskWrite(ctx, tx, id, map[string]any{"refresh_head_sha": head}); err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "review.refresh_head_advanced", Payload: core.JSONPayload(map[string]any{"workspace": t.Workspace, "task_id": id, "approved_head": t.RefreshBaselineSHA, "prior_head": t.RefreshHeadSHA, "new_head": head, "review_scope": t.RefreshReviewScope})})
	})
}
func (s *Store) SkipTaskRefresh(ctx context.Context, id, head, reason string) error {
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		t, err := getTaskRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = taskWrite(ctx, tx, id, approvalValues(head)); err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "review.refresh_skipped", Payload: core.JSONPayload(map[string]any{"workspace": t.Workspace, "task_id": id, "reason_code": reason, "approved_head": t.ApprovedHeadSHA, "new_head": head})})
	})
}

// Predicates execute before LIMIT/OFFSET. All values remain bound arguments.
func taskPredicate(ctx context.Context, f store.TaskFilter) (string, []any) {
	q := "workspace_id=?"
	args := []any{documentWorkspace(ctx)}
	addIn := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		q += " AND " + column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + ")"
		for _, v := range values {
			args = append(args, v)
		}
	}
	states := make([]string, len(f.States))
	for i, v := range f.States {
		states[i] = string(v)
	}
	addIn("state", states)
	addIn("repo_name", f.Repositories)
	if f.Assignee == "unassigned" {
		q += " AND assignee_user_id IS NULL"
	} else if f.Assignee != "" {
		q += " AND assignee_user_id=?"
		args = append(args, f.Assignee)
	}
	if f.Query != "" {
		q += " AND (LOCATE(LOWER(?),LOWER(title))>0 OR LOCATE(LOWER(?),LOWER(id))>0 OR LOCATE(LOWER(?),LOWER(source))>0 OR LOCATE(LOWER(?),LOWER(branch))>0)"
		for range 4 {
			args = append(args, f.Query)
		}
	}
	if !f.CreatedFrom.IsZero() {
		q += " AND created_at>=?"
		args = append(args, f.CreatedFrom)
	}
	if !f.CreatedTo.IsZero() {
		q += " AND created_at<?"
		args = append(args, f.CreatedTo)
	}
	for _, pair := range []struct {
		ids            []string
		added, removed string
	}{{f.ServesRequirementIDs, store.TaskContextRequirementAdded, store.TaskContextRequirementRemoved}, {f.GoverningDesignIDs, store.TaskContextDesignAdded, store.TaskContextDesignRemoved}} {
		if len(pair.ids) == 0 {
			continue
		}
		q += ` AND EXISTS (SELECT 1 FROM events e WHERE e.workspace_id=tasks.workspace_id AND e.task_id=tasks.id AND e.kind=? AND JSON_EXTRACT_STRING(e.payload_json,'id') IN (` + strings.TrimSuffix(strings.Repeat("?,", len(pair.ids)), ",") + `) AND NOT EXISTS (SELECT 1 FROM events later WHERE later.workspace_id=e.workspace_id AND later.task_id=e.task_id AND later.id>e.id AND later.kind IN (?,?) AND JSON_EXTRACT_STRING(later.payload_json,'id')=JSON_EXTRACT_STRING(e.payload_json,'id')))`
		args = append(args, pair.added)
		for _, id := range pair.ids {
			args = append(args, id)
		}
		args = append(args, pair.added, pair.removed)
	}
	return q, args
}
func (s *Store) ListTasks(ctx context.Context) ([]core.Task, error) {
	return s.ListTasksFiltered(ctx, store.TaskFilter{})
}
func (s *Store) ListTasksFiltered(ctx context.Context, f store.TaskFilter) ([]core.Task, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return s.listTaskRows(ctx, store.TaskOperationsQuery{TaskFilter: f})
}
func (s *Store) listTaskRows(ctx context.Context, q store.TaskOperationsQuery) ([]core.Task, error) {
	predicate, args := taskPredicate(ctx, q.TaskFilter)
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE ` + predicate + ` ORDER BY created_at DESC,id ASC`
	if q.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, q.Limit, q.Offset)
	}
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	out := []core.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err = s.hydrateTaskRelations(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *Store) ListTaskPage(ctx context.Context, q store.TaskOperationsQuery) (store.TaskPage, error) {
	if err := q.Validate(); err != nil {
		return store.TaskPage{}, err
	}
	predicate, args := taskPredicate(ctx, q.TaskFilter)
	var total int
	if err := documentRow(ctx, s.db, `SELECT COUNT(*) FROM tasks WHERE `+predicate, args...).Scan(&total); err != nil {
		return store.TaskPage{}, err
	}
	tasks, err := s.listTaskRows(ctx, q)
	return store.TaskPage{Tasks: tasks, Total: total}, err
}
func (s *Store) ListTaskOperations(ctx context.Context, q store.TaskOperationsQuery) (store.TaskOperationsPage, error) {
	page, err := s.ListTaskPage(ctx, q)
	if err != nil {
		return store.TaskOperationsPage{}, err
	}
	out := store.TaskOperationsPage{Tasks: page.Tasks, Total: page.Total, Events: map[string][]core.Event{}, Plans: map[string]core.SpecVersion{}}
	for _, t := range out.Tasks {
		events, err := s.ListEvents(ctx, t.ID)
		if err != nil {
			return store.TaskOperationsPage{}, err
		}
		for _, event := range events {
			switch event.Kind {
			case store.TaskContextRequirementAdded, store.TaskContextRequirementRemoved, store.TaskContextDesignAdded, store.TaskContextDesignRemoved, "task.state_changed":
				out.Events[t.ID] = append(out.Events[t.ID], event)
			}
		}
		plan, ok, err := s.GetLatestSpecVersion(ctx, t.ID)
		if err != nil {
			return store.TaskOperationsPage{}, err
		}
		if ok {
			out.Plans[t.ID] = plan
		}
	}
	return out, nil
}

func (s *Store) ApproveSpecVersionAndMaterialize(ctx context.Context, taskID string, version int) ([]core.Task, error) {
	var children []core.Task
	err := s.taskTx(ctx, taskID, func(tx *sql.Tx) error {
		if err := lockDependencyEdgesTx(ctx, tx, documentWorkspace(ctx)); err != nil {
			return err
		}
		parentRow, err := getTaskRow(ctx, tx, taskID)
		if err != nil {
			return notFound(err, "task %s", taskID)
		}
		parent := parentRow
		var decompositionJSON []byte
		if err = tx.QueryRowContext(ctx, `SELECT decomposition FROM task_specs
			WHERE task_id=? AND version=? AND workspace_id=?
			  AND version=(SELECT MAX(version) FROM task_specs WHERE task_id=? AND workspace_id=?)
			FOR UPDATE`, taskID, version, documentWorkspace(ctx), taskID, documentWorkspace(ctx)).Scan(&decompositionJSON); err != nil {
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
			if err = tx.QueryRowContext(ctx, `SELECT default_base FROM repos WHERE workspace_id=? AND name=?`,
				documentWorkspace(ctx), strings.TrimSpace(item.Repo)).Scan(&baseBranch); err != nil {
				return fmt.Errorf("blueprint %s repository %q is not configured in workspace %s", item.ID, item.Repo, documentWorkspace(ctx))
			}
			baseBranches[item.ID] = baseBranch
		}
		childrenBySub := map[string]core.Task{}
		rows, err := tx.QueryContext(ctx, `SELECT id,origin_sub_id FROM tasks
			WHERE workspace_id=? AND parent_task_id=? AND origin_sub_id<>''
			ORDER BY origin_spec_version,created_at,id`, documentWorkspace(ctx), taskID)
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
			childRow, getErr := getTaskRow(ctx, tx, id)
			if getErr != nil {
				return getErr
			}
			childrenBySub[originSubID] = childRow
		}
		createdAt := time.Now().UTC()
		childrenToCreate := 0
		for _, item := range decomposition {
			if _, exists := childrenBySub[item.ID]; !exists {
				childrenToCreate++
			}
		}
		if childrenToCreate > 0 {
			err = taskEvent(ctx, tx, core.Event{
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
			if err = insertTaskRow(ctx, tx, child); err != nil {
				return fmt.Errorf("materialize %s: %w", item.ID, err)
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: child.ID, Kind: "task.created", Payload: core.JSONPayload(child), At: createdAt}); err != nil {
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
				if _, err = tx.ExecContext(ctx, `INSERT INTO task_dependencies
					(workspace_id,task_id,depends_on_task_id) VALUES (?,?,?)
					ON DUPLICATE KEY UPDATE task_id=VALUES(task_id)`, documentWorkspace(ctx), child.ID, dependency.ID); err != nil {
					return fmt.Errorf("materialize dependency %s -> %s: %w", item.ID, dependencySubID, err)
				}
				if err = taskEvent(ctx, tx, core.Event{TaskID: child.ID, Kind: "task.dependency_added", At: createdAt,
					Payload: core.JSONPayload(map[string]string{"task_id": child.ID, "depends_on_task_id": dependency.ID})}); err != nil {
					return err
				}
			}
		}
		if err = approveSpecTx(ctx, tx, taskID, version); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})}); err != nil {
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

func (s *Store) ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, command taskops.Command) (core.Task, error) {
	if !lease.ValidFor(id) {
		return core.Task{}, fmt.Errorf("task lifecycle mutation requires a valid taskops lease")
	}
	var result core.Task
	err := s.taskTx(ctx, id, func(tx *sql.Tx) error {
		before, err := getTaskRow(ctx, tx, id)
		if err != nil {
			return notFound(err, "task %s", id)
		}
		state, err := core.TransitionTask(core.TaskState(before.State), command.Kind)
		if err != nil {
			return err
		}
		values := map[string]any{"state": state}
		result = before
		result.State = state
		if command.ProjectStages {
			values["next_stage"] = command.NextStage
			values["recovery_stage"] = command.RecoveryStage
			result.NextStage = command.NextStage
			result.RecoveryStage = command.RecoveryStage
		}
		if err := taskWrite(ctx, tx, id, values); err != nil {
			return err
		}
		if core.TaskTerminal(state) {
			if err := deleteProposedTaskContextTx(ctx, tx, id); err != nil {
				return err
			}
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state, "command": command.Kind})}); err != nil {
			return err
		}
		if command.FailureMessage != "" {
			if err := taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "dispatch.failed", Payload: core.JSONPayload(map[string]any{
				"attempt": command.Attempt, "max_attempts": command.MaxAttempts, "error": command.FailureMessage,
			})}); err != nil {
				return err
			}
		}
		if err := s.recordDependencyOutcomeTx(ctx, tx, id, state, time.Now().UTC()); err != nil {
			return err
		}
		if command.ProjectStages {
			if err := taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{
				"from_stage": before.NextStage, "next_stage": command.NextStage, "recovery_stage": command.RecoveryStage, "state": state,
			})}); err != nil {
				return err
			}
		}
		if before.ParentTaskID != "" && core.TaskTerminal(state) {
			if _, err := s.closeBlueprintParentTx(ctx, tx, before.ParentTaskID); err != nil {
				return err
			}
		}
		if command.Kind == core.TaskRecover {
			closed, closeErr := s.closeBlueprintParentTx(ctx, tx, id)
			if closeErr != nil {
				return closeErr
			}
			if closed {
				result.State = core.TaskClosed
			}
		}
		if result.State == core.TaskQueued {
			_, err := s.enqueueTaskTx(ctx, tx, id, before.Workspace)
			return err
		}
		return nil
	})
	return result, err
}

func (s *Store) closeBlueprintParentTx(ctx context.Context, tx *sql.Tx, parentID string) (bool, error) {
	if err := s.lockTaskOperation(ctx, tx, documentWorkspace(ctx), parentID); err != nil {
		return false, err
	}
	var parentState core.TaskState
	if err := documentRow(ctx, tx, `SELECT state FROM tasks
		WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), parentID).Scan(&parentState); err != nil {
		return false, notFound(err, "task %s", parentID)
	}
	closed, err := core.TransitionTask(parentState, core.TaskBlueprintClose)
	if err != nil {
		return false, nil
	}
	var childCount, nonTerminal int
	if err = documentRow(ctx, tx, `SELECT count(*),
		COALESCE(SUM(state NOT IN ('merged','closed')),0)
		FROM tasks WHERE workspace_id=? AND parent_task_id=?`,
		documentWorkspace(ctx), parentID).Scan(&childCount, &nonTerminal); err != nil {
		return false, err
	}
	if childCount == 0 || nonTerminal != 0 {
		return false, nil
	}
	if err = taskWrite(ctx, tx, parentID, map[string]any{"state": closed}); err != nil {
		return false, err
	}
	if err = deleteProposedTaskContextTx(ctx, tx, parentID); err != nil {
		return false, err
	}
	if err = taskEvent(ctx, tx, core.Event{TaskID: parentID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{
		"from": parentState, "to": closed, "command": core.TaskBlueprintClose,
	})}); err != nil {
		return false, err
	}
	if err = taskEvent(ctx, tx, core.Event{TaskID: parentID, Kind: "blueprint.closed", Payload: core.JSONPayload(map[string]any{
		"children": childCount, "terminal_states": []core.TaskState{core.TaskMerged, core.TaskClosed},
	})}); err != nil {
		return false, err
	}
	if err = s.recordDependencyOutcomeTx(ctx, tx, parentID, closed, time.Now().UTC()); err != nil {
		return false, err
	}
	return true, nil
}

func deleteProposedTaskContextTx(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM task_context_proposals WHERE workspace_id=? AND task_id=? AND state='proposed'`, documentWorkspace(ctx), id)
	return err
}

func normalizeIntervention(ctx context.Context, i core.Intervention) core.Intervention {
	a := store.ActorFromContext(ctx)
	if i.ActorID == "" {
		i.ActorID = a.ID
	}
	if i.ActorRole == "" {
		i.ActorRole = a.Role
	}
	if i.At.IsZero() {
		i.At = time.Now().UTC()
	}
	return i
}
func insertInterventionTx(ctx context.Context, tx *sql.Tx, i core.Intervention) error {
	if !i.Action.Valid() {
		return fmt.Errorf("invalid intervention action %q", i.Action)
	}
	if _, err := getTaskRow(ctx, tx, i.TaskID); err != nil {
		return err
	}
	if i.JobID != "" {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), i.JobID).Scan(&id); err != nil || id != i.TaskID {
			return fmt.Errorf("job %s does not belong to task %s", i.JobID, i.TaskID)
		}
	}
	if strings.ContainsRune(i.ActorID, 0) {
		return fmt.Errorf("audit actor contains NUL")
	}
	_, err := writeRow(ctx, tx, rowWrite{table: "interventions", operation: "INSERT", values: map[string]any{"workspace_id": documentWorkspace(ctx), "task_id": i.TaskID, "job_id": nullString(i.JobID), "actor_id": i.ActorID, "actor_role": i.ActorRole, "action": i.Action, "reason_code": i.ReasonCode, "comment": i.Comment, "at": i.At}})
	return err
}
func (s *Store) CreateIntervention(ctx context.Context, i core.Intervention) error {
	i = normalizeIntervention(ctx, i)
	return s.taskTx(ctx, i.TaskID, func(tx *sql.Tx) error {
		if err := insertInterventionTx(ctx, tx, i); err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: i.TaskID, JobID: i.JobID, Kind: "intervention." + string(i.Action), ActorID: i.ActorID, ActorRole: i.ActorRole, At: i.At, Payload: core.JSONPayload(map[string]any{"reason_code": i.ReasonCode, "comment": i.Comment})})
	})
}
func (s *Store) ListInterventions(ctx context.Context, id string) ([]core.Intervention, error) {
	rows, err := documentRows(ctx, s.db, `SELECT id,task_id,COALESCE(job_id,''),actor_id,actor_role,action,reason_code,comment,at FROM interventions WHERE workspace_id=? AND task_id=? ORDER BY id`, documentWorkspace(ctx), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Intervention{}
	for rows.Next() {
		var i core.Intervention
		if err = rows.Scan(&i.ID, &i.TaskID, &i.JobID, &i.ActorID, &i.ActorRole, &i.Action, &i.ReasonCode, &i.Comment, &i.At); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (s *Store) CancelTaskCommand(ctx context.Context, lease taskops.TaskLease, i core.Intervention) (core.Task, error) {
	if !lease.ValidFor(i.TaskID) {
		return core.Task{}, fmt.Errorf("task cancellation requires a valid taskops lease")
	}
	if i.Action != core.InterventionCancel || strings.TrimSpace(i.ReasonCode) == "" {
		return core.Task{}, fmt.Errorf("cancel intervention requires a reason")
	}
	i = normalizeIntervention(ctx, i)
	err := s.taskTx(ctx, i.TaskID, func(tx *sql.Tx) error {
		before, err := getTaskRow(ctx, tx, i.TaskID)
		if err != nil {
			return err
		}
		if core.TaskTerminal(before.State) {
			return store.ErrTaskTerminal
		}
		state, err := core.TransitionTask(before.State, core.TaskCancel)
		if err != nil {
			return err
		}
		if err = insertInterventionTx(ctx, tx, i); err != nil {
			return err
		}
		event := func(kind string, payload any, job string) error {
			return taskEvent(ctx, tx, core.Event{TaskID: i.TaskID, JobID: job, Kind: kind, ActorID: i.ActorID, ActorRole: i.ActorRole, Payload: core.JSONPayload(payload), At: i.At})
		}
		if err = event("intervention.cancel", map[string]any{"reason_code": i.ReasonCode, "comment": i.Comment}, i.JobID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,job_id,state,attempt_id,session_id FROM work_orders WHERE workspace_id=? AND task_id=? AND state NOT IN ('completed','cancelled') FOR UPDATE`, documentWorkspace(ctx), i.TaskID)
		if err != nil {
			return err
		}
		type cancelledOrder struct {
			id, job, attempt, session string
			state                     core.WorkOrderState
		}
		var orders []cancelledOrder
		for rows.Next() {
			var o cancelledOrder
			if err = rows.Scan(&o.id, &o.job, &o.state, &o.attempt, &o.session); err != nil {
				rows.Close()
				return err
			}
			if _, err = core.TransitionWorkOrder(o.state, core.WorkOrderCmdCancel); err != nil {
				rows.Close()
				return err
			}
			orders = append(orders, o)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		var cancelled []string
		for _, o := range orders {
			v := map[string]any{"state": core.WorkOrderCancelled, "attempt_id": "", "updated_at": i.At}
			if o.state == core.WorkOrderClaimed {
				v["last_attempt_outcome"] = "cancelled"
			}
			if o.attempt != "" {
				v["last_attempt_id"] = o.attempt
				for _, key := range []string{"claimant_id", "session_id", "client_token_hash", "agent", "model", "worker_id", "model_enforcement"} {
					v[key] = ""
				}
				for _, key := range []string{"lease_expires_at", "execution_started_at", "execution_deadline"} {
					v[key] = nil
				}
			}
			if _, err = writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "UPDATE", values: v, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": o.id}}); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='failed',ended_at=?,updated_at=? WHERE workspace_id=? AND task_id=? AND id=? AND state<>'done'`, i.At, i.At, documentWorkspace(ctx), i.TaskID, o.job); err != nil {
				return err
			}
			cancelled = append(cancelled, o.id)
			if err = event("work_order.cancelled", map[string]any{"id": o.id, "attempt_id": o.attempt, "session_id": o.session, "state": core.WorkOrderCancelled, "from": o.state, "command": core.WorkOrderCmdCancel}, o.job); err != nil {
				return err
			}
		}
		if err = taskWrite(ctx, tx, i.TaskID, map[string]any{"state": state, "next_stage": "", "recovery_stage": ""}); err != nil {
			return err
		}
		if err = deleteProposedTaskContextTx(ctx, tx, i.TaskID); err != nil {
			return err
		}
		if err = event("task.state_changed", map[string]any{"from": before.State, "to": state, "command": core.TaskCancel}, ""); err != nil {
			return err
		}
		if err = event("task.cancelled", map[string]any{"actor": i.ActorID, "reason": i.ReasonCode, "comment": i.Comment, "from": before.State, "cancelled_work_orders": cancelled}, ""); err != nil {
			return err
		}
		return s.recordDependencyOutcomeTx(ctx, tx, i.TaskID, state, i.At)
	})
	if err != nil {
		return core.Task{}, err
	}
	return s.GetTask(ctx, i.TaskID)
}
func (s *Store) EnsureTaskEnqueued(ctx context.Context, id string) error {
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		t, err := getTaskRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if t.State != core.TaskQueued {
			return fmt.Errorf("task %s is not queued", id)
		}
		inserted, err := s.enqueueTaskTx(ctx, tx, id, t.Workspace)
		if err != nil || !inserted {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "dispatch.reconciled", Payload: core.JSONPayload(map[string]string{"reason": "missing durable queue job"})})
	})
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
	err := s.taskTx(ctx, request.TaskID, func(tx *sql.Tx) error {
		before, err := getTaskRow(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}

		var latestPayload []byte
		events := []core.Event{}
		if err = tx.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE workspace_id=? AND task_id=? AND kind='task.state_changed' ORDER BY id DESC LIMIT 1`, documentWorkspace(ctx), request.TaskID).Scan(&latestPayload); err != nil {
			return err
		}
		events = append(events, core.Event{Kind: "task.state_changed", Payload: latestPayload})
		if !store.AtMergeGate(before, events) {
			return fmt.Errorf("task %s is not at the merge gate", request.TaskID)
		}
		if request.JobID != "" {
			job, jobErr := scanJob(documentRow(ctx, tx, `SELECT `+jobColumns+` FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), request.JobID))
			if jobErr != nil || job.TaskID != request.TaskID || core.Stage(job.Stage) != core.StageReview {
				return fmt.Errorf("review job %s does not belong to task %s", request.JobID, request.TaskID)
			}
		}
		count, _, err := bounceCountsTx(ctx, tx, request.TaskID)
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
		if err = insertInterventionTx(ctx, tx, plan.Intervention); err != nil {
			return err
		}
		for _, event := range plan.Events {
			if err = taskEvent(ctx, tx, event); err != nil {
				return err
			}
		}
		if err = taskWrite(ctx, tx, request.TaskID, map[string]any{"state": state, "next_stage": plan.NextStage, "recovery_stage": plan.Recovery}); err != nil {
			return err
		}
		result = before
		result.State = state
		result.NextStage = plan.NextStage
		result.RecoveryStage = plan.Recovery

		if request.Hold && !before.Hold {
			if err = taskWrite(ctx, tx, request.TaskID, map[string]any{"hold": true}); err != nil {
				return err
			}
			result.Hold = true
			if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, Kind: "task.hold_changed", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"hold": true, "reason_code": store.UserRequestChangesReason})}); err != nil {
				return err
			}
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, Kind: "task.state_changed", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state, "command": plan.Command})}); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, Kind: "pipeline.transition_decided", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"from_stage": before.NextStage, "next_stage": plan.NextStage, "recovery_stage": plan.Recovery, "state": state})}); err != nil {
			return err
		}
		_, err = s.enqueueTaskTx(ctx, tx, request.TaskID, before.Workspace)
		return err
	})
	return result, err
}

func (s *Store) ChangeTaskSetupCommand(ctx context.Context, lease taskops.TaskLease, raw store.SetupChangeRequest) (store.SetupChangeResult, error) {
	var result store.SetupChangeResult
	r, validationErr := store.PrepareSetupChangeRequest(raw)
	if !lease.ValidForCommand(r.TaskID, taskops.SetupChangeCommand) {
		return result, fmt.Errorf("taskops lease does not authorize setup change for task %s", r.TaskID)
	}
	if validationErr != nil {
		return result, validationErr
	}
	err := s.taskTx(ctx, r.TaskID, func(tx *sql.Tx) error {
		ws := documentWorkspace(ctx)
		if err := lockKey(ctx, tx, "task-setup-request:"+r.RequestID); err != nil {
			return err
		}
		identity := core.JSONPayload(store.SetupChangeIdentity(r))
		var sw string
		var priorRequest, priorResult []byte
		err := tx.QueryRowContext(ctx, `SELECT workspace_id,request_json,result_json FROM task_setup_changes WHERE request_id=?`, r.RequestID).Scan(&sw, &priorRequest, &priorResult)
		if err == nil {
			if sw != ws || !equalJSON(identity, priorRequest) {
				return fmt.Errorf("%w: request_id was already used for different inputs", store.ErrSetupChangeConflict)
			}
			return json.Unmarshal(priorResult, &result)
		}
		if err != sql.ErrNoRows {
			return err
		}
		t, err := getTaskRow(ctx, tx, r.TaskID)
		if err != nil {
			return err
		}
		if core.TaskTerminal(t.State) {
			return fmt.Errorf("%w: terminal task cannot change setup", store.ErrSetupChangeConflict)
		}
		orders, err := s.listOrders(ctx, tx, []string{t.ID})
		if err != nil {
			return err
		}
		byID := map[string]core.WorkOrder{}
		for _, o := range orders {
			byID[o.ID] = o
			if o.State == core.WorkOrderClaimed {
				return fmt.Errorf("%w: task has a claimed attempt", store.ErrSetupChangeConflict)
			}
			if o.Stage == core.StageReview && o.State == core.WorkOrderSubmitted {
				return fmt.Errorf("%w: task has an in-flight review verdict", store.ErrSetupChangeConflict)
			}
		}
		for _, d := range r.WorkOrderUpdates {
			o, ok := byID[d.ID]
			if !ok || o.State != core.WorkOrderQueued || o.SessionID != "" || o.WorkerID != "" {
				return fmt.Errorf("%w: work order %s is not an unclaimed queued order", store.ErrSetupChangeConflict, d.ID)
			}
		}
		prior := t.SetupContract
		if err = taskWrite(ctx, tx, t.ID, map[string]any{"setup_name": r.Setup.Name, "setup_contract": core.JSONPayload(r.Setup)}); err != nil {
			return err
		}
		t.SetupName = r.Setup.Name
		t.SetupContract = r.Setup
		result = store.SetupChangeResult{RequestID: r.RequestID, Task: t, ReviewTransition: r.ReviewTransition, UpdatedWorkOrders: []string{}, CreatedWorkOrders: []string{}, RetainedWorkOrders: append([]string{}, r.RetainedWorkOrderIDs...), SupersededWorkOrders: append([]string{}, r.SupersedeWorkOrderIDs...)}
		now := time.Now().UTC()
		actor := store.ActorFromContext(ctx)
		for _, d := range r.WorkOrderUpdates {
			o := byID[d.ID]
			o.RequiredModel = d.RequiredModel
			o.RequiredHarness = d.RequiredHarness
			o.RequiredEffort = d.RequiredEffort
			o.RequiredHarnessConfig = d.RequiredHarnessConfig
			o.ExecutionTimeoutText = d.ExecutionTimeoutText
			o.LastAttemptOutcome = ""
			o.LastFailureMessage = ""
			o.LastFailureDetail = ""
			o.LastFailureExitStatus = nil
			o.LastFailureAt = time.Time{}
			o.AutomaticRetryCount = 0
			o.NextRetryAt = time.Time{}
			o.RetrySuppressed = false
			o.RetrySuppressionReason = ""
			o.QueueEnteredAt = d.QueueEnteredAt
			o.QueueDeadline = d.QueueDeadline
			o.RedispatchCount++
			o.UpdatedAt = now
			if err = orderWrite(ctx, tx, o); err != nil {
				return err
			}
			result.UpdatedWorkOrders = append(result.UpdatedWorkOrders, d.ID)
			if d.Stage == core.StageReview {
				if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, JobID: d.JobID, Kind: "review.seat.setup_rebuilt", At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": ws, "request_id": r.RequestID, "review_round": d.ReviewRound, "review_seat": d.ReviewSeat, "work_order_id": d.ID, "outcome": "rebuilt_future_work", "previous_setup": prior, "new_setup": r.Setup})}); err != nil {
					return err
				}
			}
		}
		for _, id := range r.SupersedeWorkOrderIDs {
			o, ok := byID[id]
			if !ok {
				return fmt.Errorf("work order %s not found", id)
			}
			old := o.State
			values := map[string]any{"review_superseded": true, "updated_at": now}
			if old == core.WorkOrderQueued {
				next, err := core.TransitionWorkOrder(old, core.WorkOrderCmdCancel)
				if err != nil {
					return err
				}
				o.State = next
				values["state"] = next
				if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, JobID: o.JobID, Kind: "work_order.cancelled", At: now, Payload: core.JSONPayload(map[string]any{"id": id, "state": next, "from": old, "command": core.WorkOrderCmdCancel})}); err != nil {
					return err
				}
				if _, err = writeRow(ctx, tx, rowWrite{table: "jobs", operation: "UPDATE", values: map[string]any{"state": core.JobFailed, "ended_at": now, "updated_at": now}, where: map[string]any{"workspace_id": ws, "id": o.JobID}}); err != nil {
					return err
				}
			}
			if _, err = writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "UPDATE", values: values, where: map[string]any{"workspace_id": ws, "id": id}}); err != nil {
				return err
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, JobID: o.JobID, Kind: "review.seat.setup_superseded", At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": ws, "request_id": r.RequestID, "work_order_id": id, "prior_state": old, "resulting_state": o.State, "outcome": "historical_only", "previous_setup": prior, "new_setup": r.Setup})}); err != nil {
				return err
			}
		}
		for i, j := range r.NewJobs {
			o := orderDefaults(r.NewWorkOrders[i])
			o.State = core.WorkOrderQueued
			if j.TaskID != t.ID || o.TaskID != t.ID || o.JobID != j.ID || o.Stage != j.Stage {
				return fmt.Errorf("invalid setup-change review member %d", i)
			}
			if err = insertJobRow(ctx, tx, j); err != nil {
				return err
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, JobID: j.ID, Kind: "job.created", Payload: core.JSONPayload(j), At: now}); err != nil {
				return err
			}
			if err = s.insertOrderTx(ctx, tx, o, false); err != nil {
				return err
			}
			result.CreatedWorkOrders = append(result.CreatedWorkOrders, o.ID)
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, JobID: j.ID, Kind: "review.seat.setup_rebuilt", At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": ws, "request_id": r.RequestID, "review_round": o.ReviewRound, "review_seat": o.ReviewSeat, "work_order_id": o.ID, "outcome": "created_under_new_setup", "previous_setup": prior, "new_setup": r.Setup})}); err != nil {
				return err
			}
		}
		for _, id := range result.RetainedWorkOrders {
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, Kind: "review.seat.setup_retained", At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": ws, "request_id": r.RequestID, "work_order_id": id, "outcome": "retained_compatible_verdict", "previous_setup": prior, "new_setup": r.Setup})}); err != nil {
				return err
			}
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, Kind: "task.setup.changed", At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": ws, "task_id": t.ID, "actor": actor.ID, "request_id": r.RequestID, "reason": r.Reason, "previous_setup": prior, "new_setup": r.Setup, "lifecycle_boundary": "future_work", "stage": t.NextStage, "updated_work_order_ids": result.UpdatedWorkOrders, "review_transition": map[string]any{"kind": r.ReviewTransition, "prior_round": r.PriorReviewRound, "resulting_round": r.ResultingReviewRound, "retained_work_order_ids": result.RetainedWorkOrders, "superseded_work_order_ids": result.SupersededWorkOrders, "created_work_order_ids": result.CreatedWorkOrders}})}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO task_setup_changes(workspace_id,request_id,task_id,request_json,result_json,actor_id,created_at) VALUES(?,?,?,?,?,?,?)`, ws, r.RequestID, t.ID, identity, core.JSONPayload(result), actor.ID, now)
		return err
	})
	return result, err
}
