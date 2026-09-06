package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) ListBlockingTaskIDs(ctx context.Context, taskID string) ([]string, error) {
	rows, err := documentRows(ctx, s.db, `SELECT dependency.id
		FROM task_dependencies edge
		JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
			AND dependency.id=edge.depends_on_task_id
		WHERE edge.workspace_id=? AND edge.task_id=? AND dependency.state<>'merged'
		ORDER BY dependency.id`, documentWorkspace(ctx), taskID)
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
		if err := documentRow(ctx, s.db, `SELECT workspace_id, state FROM tasks WHERE id=?`, dependencyID).
			Scan(&dependencyWorkspace, &dependencyState); err != nil {
			return notFound(err, "dependency task %s", dependencyID)
		}
		if dependencyWorkspace != documentWorkspace(ctx) {
			return fmt.Errorf("dependency task %s belongs to another workspace", dependencyID)
		}
		if core.TaskTerminal(core.TaskState(dependencyState)) {
			return fmt.Errorf("dependency task %s is not open", dependencyID)
		}
	}
	return nil
}

func (s *Store) ListDependentTaskIDs(ctx context.Context, taskID string) ([]string, error) {
	rows, err := documentRows(ctx, s.db, `SELECT task_id FROM task_dependencies
		WHERE workspace_id=? AND depends_on_task_id=? ORDER BY task_id`, documentWorkspace(ctx), taskID)
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
	rows, err := documentBatchRows(ctx, s.db, `SELECT edge.task_id, dependency.id, dependency.state
		FROM task_dependencies edge
		JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
			AND dependency.id=edge.depends_on_task_id
		WHERE edge.workspace_id=? AND edge.task_id IN (%s)
			AND dependency.state<>'merged'
		ORDER BY edge.task_id, dependency.id`, documentWorkspace(ctx), taskIDs)
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
	err := s.taskTx(ctx, request.TaskID, func(tx *sql.Tx) error {
		if err := lockDependencyEdgesTx(ctx, tx, documentWorkspace(ctx)); err != nil {
			return err
		}
		var prior store.DependencyAdditionRequest
		var actorID, actorRole string
		var priorAdded bool
		err := documentRow(ctx, tx, `SELECT task_id,depends_on_task_id,reason,request_id,actor_id,actor_role,added
			FROM task_dependency_additions WHERE workspace_id=? AND request_id=?`,
			documentWorkspace(ctx), request.RequestID).
			Scan(&prior.TaskID, &prior.DependsOnTaskID, &prior.Reason, &prior.RequestID, &actorID, &actorRole, &priorAdded)
		if err == nil {
			if prior != request || actorID != actor.ID || actorRole != string(actor.Role) {
				return fmt.Errorf("%w: request_id %s was already used for different dependency addition inputs", store.ErrTaskDependencyConflict, request.RequestID)
			}
			added = priorAdded
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		states := map[string]core.TaskState{}
		rows, err := documentRows(ctx, tx, `SELECT id,state FROM tasks
			WHERE workspace_id=? AND id IN (?,?) ORDER BY id FOR UPDATE`,
			documentWorkspace(ctx), request.TaskID, request.DependsOnTaskID)
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
		if err = documentRow(ctx, tx, `SELECT EXISTS (SELECT 1 FROM task_dependencies
			WHERE workspace_id=? AND task_id=? AND depends_on_task_id=?)`,
			documentWorkspace(ctx), request.TaskID, request.DependsOnTaskID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			cycle, err := dependencyReaches(ctx, tx, request.DependsOnTaskID, request.TaskID)
			if err != nil {
				return err
			}
			if cycle {
				return fmt.Errorf("%w: %s already reaches %s", store.ErrTaskDependencyCycle, request.DependsOnTaskID, request.TaskID)
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO task_dependencies (workspace_id,task_id,depends_on_task_id)
				VALUES (?,?,?)`, documentWorkspace(ctx), request.TaskID, request.DependsOnTaskID); err != nil {
				return err
			}
			added = true
		}
		now := time.Now().UTC()
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_dependency_additions
			(workspace_id,request_id,task_id,depends_on_task_id,reason,actor_id,actor_role,added,created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			documentWorkspace(ctx), request.RequestID, request.TaskID, request.DependsOnTaskID,
			request.Reason, actor.ID, actor.Role, added, now); err != nil {
			return err
		}
		if !added {
			return nil
		}
		if err = taskEvent(ctx, tx, core.Event{
			TaskID: request.TaskID, Kind: "task.dependency_added", ActorID: actor.ID, ActorRole: actor.Role, At: now,
			Payload: core.JSONPayload(map[string]any{
				"task_id": request.TaskID, "depends_on_task_id": request.DependsOnTaskID,
				"reason": request.Reason, "request_id": request.RequestID,
			}),
		}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE work_orders SET queue_blocked_at=?,updated_at=?
			WHERE workspace_id=? AND task_id=? AND stage='implement'
				AND state='queued' AND queue_blocked_at IS NULL`, now, now, documentWorkspace(ctx), request.TaskID)
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
	err := s.taskTx(ctx, request.TaskID, func(tx *sql.Tx) error {
		if err := lockDependencyEdgesTx(ctx, tx, documentWorkspace(ctx)); err != nil {
			return err
		}
		var prior store.DependencyRemovalRequest
		var actorID, actorRole string
		err := documentRow(ctx, tx, `SELECT task_id,depends_on_task_id,reason,request_id,actor_id,actor_role
			FROM task_dependency_removals WHERE workspace_id=? AND request_id=?`,
			documentWorkspace(ctx), request.RequestID).
			Scan(&prior.TaskID, &prior.DependsOnTaskID, &prior.Reason, &prior.RequestID, &actorID, &actorRole)
		if err == nil {
			if prior != request || actorID != actor.ID || actorRole != string(actor.Role) {
				return fmt.Errorf("request_id %s was already used for different dependency removal inputs", request.RequestID)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		command, err := tx.ExecContext(ctx, `DELETE FROM task_dependencies
			WHERE workspace_id=? AND task_id=? AND depends_on_task_id=?`,
			documentWorkspace(ctx), request.TaskID, request.DependsOnTaskID)
		if err != nil {
			return err
		}
		if affected, _ := command.RowsAffected(); affected != 1 {
			return fmt.Errorf("dependency edge %s -> %s not found", request.TaskID, request.DependsOnTaskID)
		}
		now := time.Now().UTC()
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_dependency_removals
			(workspace_id,request_id,task_id,depends_on_task_id,reason,actor_id,actor_role,created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			documentWorkspace(ctx), request.RequestID, request.TaskID, request.DependsOnTaskID,
			request.Reason, actor.ID, actor.Role, now); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{
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

func (s *Store) resumeDependencyQueueClocksTx(ctx context.Context, tx *sql.Tx, taskID string, now time.Time) error {
	var blocked bool
	if err := documentRow(ctx, tx, `SELECT EXISTS (
		SELECT 1 FROM task_dependencies edge
		JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
			AND dependency.id=edge.depends_on_task_id
		WHERE edge.workspace_id=? AND edge.task_id=? AND dependency.state<>'merged'
	)`, documentWorkspace(ctx), taskID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE work_orders
		SET queue_deadline=TIMESTAMPADD(MICROSECOND,TIMESTAMPDIFF(MICROSECOND,queue_blocked_at,?),queue_deadline),
			queue_blocked_at=NULL, updated_at=?
		WHERE workspace_id=? AND task_id=? AND stage='implement'
			AND state='queued' AND queue_blocked_at IS NOT NULL`,
		now, now, documentWorkspace(ctx), taskID)
	return err
}

func dependencyReaches(ctx context.Context, tx *sql.Tx, from, to string) (bool, error) {
	seen := map[string]bool{}
	pending := []string{from}
	for len(pending) > 0 {
		id := pending[0]
		pending = pending[1:]
		if id == to {
			return true, nil
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		rows, err := tx.QueryContext(ctx, `SELECT depends_on_task_id FROM task_dependencies WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), id)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var next string
			if err = rows.Scan(&next); err != nil {
				rows.Close()
				return false, err
			}
			pending = append(pending, next)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
	}
	return false, nil
}
func (s *Store) recordDependencyOutcomeTx(ctx context.Context, tx *sql.Tx, id string, state core.TaskState, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT task_id FROM task_dependencies WHERE workspace_id=? AND depends_on_task_id=? ORDER BY task_id`, documentWorkspace(ctx), id)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var dependent string
		if err = rows.Scan(&dependent); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, dependent)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, dependent := range ids {
		if state == core.TaskMerged {
			if err = s.resumeDependencyQueueClocksTx(ctx, tx, dependent, now); err != nil {
				return err
			}
		} else if core.TaskTerminal(state) {
			var exists bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE workspace_id=? AND task_id=? AND kind='task.dependency_unsatisfiable' AND JSON_EXTRACT_STRING(payload_json,'depends_on_task_id')=?)`, documentWorkspace(ctx), dependent, id).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: dependent, Kind: "task.dependency_unsatisfiable", At: now, Payload: core.JSONPayload(map[string]any{"task_id": dependent, "depends_on_task_id": id, "dependency_state": state})}); err != nil {
				return err
			}
		}
	}
	return nil
}
