package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, command taskops.Command) (core.Task, error) {
	if command.Kind == core.TaskStartOver {
		return core.Task{}, fmt.Errorf("task start over requires StartOverTaskCommand")
	}
	if !lease.ValidFor(id) {
		return core.Task{}, fmt.Errorf("task lifecycle mutation requires a valid taskops lease")
	}
	var result core.Task
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		key := "conveyor:task-operation:" + workspace(ctx) + ":" + id
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
			return err
		}
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		state, err := core.TransitionTask(core.TaskState(before.State), command.Kind)
		if err != nil {
			return err
		}
		ready, err := verifyReviewReadyTx(ctx, tx, taskFromDB(before))
		if err != nil {
			return err
		}
		if !ready {
			if state == core.TaskApproved || state == core.TaskMerged {
				return fmt.Errorf("verification is required before approval or merge")
			}
			if command.NextStage == core.StageReview {
				command.NextStage = core.StageVerify
			}
			if command.RecoveryStage == core.StageReview {
				command.RecoveryStage = core.StageVerify
			}
		}
		if command.ProjectStages {
			updated, updateErr := q.UpdateTaskTransition(ctx, db.UpdateTaskTransitionParams{
				ID: id, WorkspaceID: workspace(ctx), State: string(state), NextStage: string(command.NextStage), RecoveryStage: string(command.RecoveryStage),
			})
			if updateErr != nil {
				return updateErr
			}
			result = taskFromDB(updated)
		} else {
			updated, updateErr := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{ID: id, WorkspaceID: workspace(ctx), State: string(state)})
			if updateErr != nil {
				return updateErr
			}
			result = taskFromDB(updated)
		}
		if core.TaskTerminal(state) {
			if err := deleteProposedTaskContextTx(ctx, tx, id); err != nil {
				return err
			}
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state, "command": command.Kind})}); err != nil {
			return err
		}
		if command.FailureMessage != "" {
			if err := insertEvent(ctx, q, core.Event{TaskID: id, Kind: "dispatch.failed", Payload: core.JSONPayload(map[string]any{
				"attempt": command.Attempt, "max_attempts": command.MaxAttempts, "error": command.FailureMessage,
			})}); err != nil {
				return err
			}
		}
		if err := s.recordDependencyOutcomeTx(ctx, tx, id, state, time.Now().UTC()); err != nil {
			return err
		}
		if command.ProjectStages {
			if err := insertEvent(ctx, q, core.Event{TaskID: id, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{
				"from_stage": before.NextStage, "next_stage": command.NextStage, "recovery_stage": command.RecoveryStage, "state": state,
			})}); err != nil {
				return err
			}
		}
		if before.ParentTaskID.Valid && core.TaskTerminal(state) {
			if _, err := s.closeBlueprintParentTx(ctx, tx, q, before.ParentTaskID.String); err != nil {
				return err
			}
		}
		if command.Kind == core.TaskRecover {
			closed, closeErr := s.closeBlueprintParentTx(ctx, tx, q, id)
			if closeErr != nil {
				return closeErr
			}
			if closed {
				result.State = core.TaskClosed
			}
		}
		if result.State == core.TaskQueued {
			_, err := s.enqueueTaskTx(ctx, tx, id, before.WorkspaceID)
			return err
		}
		return nil
	})
	return result, err
}

// closeBlueprintParentTx applies the level-triggered blueprint close edge
// while holding the same task-operation lock as every ordinary lifecycle
// command and a row lock shared with human cancellation. State and child
// eligibility are revalidated after both locks are held.
func (s *Store) closeBlueprintParentTx(ctx context.Context, tx pgx.Tx, q *db.Queries, parentID string) (bool, error) {
	parentKey := "conveyor:task-operation:" + workspace(ctx) + ":" + parentID
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", parentKey); err != nil {
		return false, err
	}
	var parentState core.TaskState
	if err := tx.QueryRow(ctx, `SELECT state FROM tasks
		WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), parentID).Scan(&parentState); err != nil {
		return false, notFound(err, "task %s", parentID)
	}
	closed, err := core.TransitionTask(parentState, core.TaskBlueprintClose)
	if err != nil {
		return false, nil
	}
	var childCount, nonTerminal int
	if err = tx.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE state NOT IN ('merged','closed'))
		FROM tasks WHERE workspace_id=$1 AND parent_task_id=$2`,
		workspace(ctx), parentID).Scan(&childCount, &nonTerminal); err != nil {
		return false, err
	}
	if childCount == 0 || nonTerminal != 0 {
		return false, nil
	}
	if _, err = q.UpdateTaskState(ctx, db.UpdateTaskStateParams{
		ID: parentID, WorkspaceID: workspace(ctx), State: string(closed),
	}); err != nil {
		return false, err
	}
	if err = deleteProposedTaskContextTx(ctx, tx, parentID); err != nil {
		return false, err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: parentID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{
		"from": parentState, "to": closed, "command": core.TaskBlueprintClose,
	})}); err != nil {
		return false, err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: parentID, Kind: "blueprint.closed", Payload: core.JSONPayload(map[string]any{
		"children": childCount, "terminal_states": []core.TaskState{core.TaskMerged, core.TaskClosed},
	})}); err != nil {
		return false, err
	}
	if err = s.recordDependencyOutcomeTx(ctx, tx, parentID, closed, time.Now().UTC()); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) resumeDependencyQueueClocksTx(ctx context.Context, tx pgx.Tx, taskID string, now time.Time) error {
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM task_dependencies edge
		JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
			AND dependency.id=edge.depends_on_task_id
		WHERE edge.workspace_id=$1 AND edge.task_id=$2 AND dependency.state<>'merged'
	)`, workspace(ctx), taskID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE work_orders
		SET queue_deadline=queue_deadline+($1-queue_blocked_at),
			queue_blocked_at=NULL, updated_at=$1
		WHERE workspace_id=$2 AND task_id=$3 AND stage IN ('implement','verify')
			AND state='queued' AND queue_blocked_at IS NOT NULL`,
		now, workspace(ctx), taskID)
	return err
}

func (s *Store) recordDependencyOutcomeTx(ctx context.Context, tx pgx.Tx, dependencyID string, state core.TaskState, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT task_id FROM task_dependencies
		WHERE workspace_id=$1 AND depends_on_task_id=$2 ORDER BY task_id`,
		workspace(ctx), dependencyID)
	if err != nil {
		return err
	}
	var dependents []string
	for rows.Next() {
		var dependentID string
		if err = rows.Scan(&dependentID); err != nil {
			rows.Close()
			return err
		}
		dependents = append(dependents, dependentID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if state == core.TaskMerged {
		for _, dependentID := range dependents {
			if err = s.resumeDependencyQueueClocksTx(ctx, tx, dependentID, now); err != nil {
				return err
			}
		}
		return nil
	}
	if !core.TaskTerminal(state) {
		return nil
	}
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return err
	}
	for _, dependentID := range dependents {
		payload := core.JSONPayload(map[string]any{
			"task_id": dependentID, "depends_on_task_id": dependencyID, "dependency_state": state,
		})
		var insertedID int64
		err = tx.QueryRow(ctx, `INSERT INTO events
			(workspace_id,task_id,job_id,kind,actor_id,actor_role,payload_json,at)
			VALUES ($1,$2,NULL,'task.dependency_unsatisfiable',$3,$4,$5,$6)
			ON CONFLICT DO NOTHING
			RETURNING id`,
			workspace(ctx), dependentID, actor.ID, actor.Role, payload, now).Scan(&insertedID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func retireWorkOrderSiblingsTx(ctx context.Context, tx pgx.Tx, q *db.Queries, workspaceID string, authoritative core.WorkOrder, reason string, now time.Time, olderOnly bool) error {
	// Review seats are intentionally parallel siblings, not superseded retries.
	if authoritative.Stage == core.StageReview {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id,job_id,state FROM work_orders
		WHERE workspace_id=$1 AND task_id=$2 AND stage=$3 AND id<>$4 AND state IN ('queued','stale')
		AND (NOT $5 OR (created_at,id)<($6,$4))
		ORDER BY created_at,id FOR UPDATE`, workspaceID, authoritative.TaskID, authoritative.Stage, authoritative.ID, olderOnly, authoritative.CreatedAt)
	if err != nil {
		return err
	}
	type sibling struct {
		id, jobID string
		state     core.WorkOrderState
	}
	var siblings []sibling
	for rows.Next() {
		var item sibling
		if err = rows.Scan(&item.id, &item.jobID, &item.state); err != nil {
			rows.Close()
			return err
		}
		siblings = append(siblings, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range siblings {
		tag, updateErr := tx.Exec(ctx, `UPDATE work_orders SET state='cancelled',claimant_id='',session_id='',attempt_id='',
			client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',
			last_attempt_outcome=$1,next_retry_at=NULL,retry_suppressed=true,retry_suppression_reason='superseded',updated_at=$2
			WHERE workspace_id=$3 AND id=$4 AND state=$5`, core.WorkOrderOutcomeCancelled, now, workspaceID, item.id, item.state)
		if updateErr != nil {
			return updateErr
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("work order %s changed during sibling retirement", item.id)
		}
		if _, updateErr = tx.Exec(ctx, `UPDATE jobs SET state='failed',ended_at=COALESCE(ended_at,$1),updated_at=$1 WHERE id=$2`, now, item.jobID); updateErr != nil {
			return updateErr
		}
		if updateErr = insertEvent(ctx, q, core.Event{TaskID: authoritative.TaskID, JobID: item.jobID, Kind: "work_order.retired", Payload: core.JSONPayload(map[string]any{
			"work_order_id": item.id, "authoritative_work_order_id": authoritative.ID, "stage": authoritative.Stage,
			"prior_state": item.state, "new_state": core.WorkOrderCancelled, "reason": reason,
		}), At: now}); updateErr != nil {
			return updateErr
		}
	}
	return nil
}

func workOrderSupersessionGuardTx(ctx context.Context, tx pgx.Tx, workspaceID string, order core.WorkOrder) error {
	var task core.Task
	var policy []byte
	if err := tx.QueryRow(ctx, `SELECT id,state,next_stage,recovery_stage,setup_contract,reviewed_head_sha,approval_stale,refresh_head_sha FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, order.TaskID).
		Scan(&task.ID, &task.State, &task.NextStage, &task.RecoveryStage, &policy, &task.ReviewedHeadSHA, &task.ApprovalStale, &task.RefreshHeadSHA); err != nil {
		return err
	}
	if len(policy) > 0 {
		if err := json.Unmarshal(policy, &task.SetupContract); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at,id FOR UPDATE`, workspaceID, order.TaskID)
	if err != nil {
		return err
	}
	var taskOrders []core.WorkOrder
	for rows.Next() {
		candidate, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		taskOrders = append(taskOrders, candidate)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return store.WorkOrderRecoverySupersessionError(task, order, taskOrders)
}
