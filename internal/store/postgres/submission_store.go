package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) BindTaskApproval(ctx context.Context, id, headSHA string) error {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return fmt.Errorf("approved head SHA is required")
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		task, err := q.BindTaskApproval(ctx, db.BindTaskApprovalParams{ID: id, WorkspaceID: workspace(ctx), HeadSha: headSHA})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: "approval.bound", Payload: core.JSONPayload(map[string]any{"workspace": task.WorkspaceID, "task_id": id, "approved_head": headSHA})})
	})
}

func (s *Store) MarkTaskApprovalStale(ctx context.Context, id, approvedHeadSHA, newHeadSHA, scope, reason string) (bool, error) {
	if approvedHeadSHA == "" || newHeadSHA == "" || approvedHeadSHA == newHeadSHA {
		return false, fmt.Errorf("distinct approved and new head SHAs are required")
	}
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), id); err != nil {
			return err
		}
		task, err := q.MarkTaskApprovalStale(ctx, db.MarkTaskApprovalStaleParams{ID: id, WorkspaceID: workspace(ctx), ApprovedHeadSha: approvedHeadSHA, NewHeadSha: newHeadSHA, RefreshReviewScope: scope})
		if errors.Is(err, pgx.ErrNoRows) {
			if _, getErr := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)}); getErr != nil {
				return notFound(getErr, "task %s", id)
			}
			return nil
		}
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if err = s.supersedeVerificationTx(ctx, tx, id, newHeadSHA); err != nil {
			return err
		}
		created = true
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: "approval.stale", Payload: core.JSONPayload(map[string]any{"workspace": task.WorkspaceID, "task_id": id, "reason_code": reason, "approved_head": approvedHeadSHA, "new_head": newHeadSHA, "review_scope": scope})})
	})
	return created, err
}

// AdvanceTaskRefreshHead moves a stale approval's refresh target to the head
// most recently submitted for review, so the next refresh round contracts the
// pushed fix rather than the head recorded when the approval went stale
// . Re-advancing to the current refresh head is an idempotent
// no-op.
func (s *Store) AdvanceTaskRefreshHead(ctx context.Context, id, newHeadSHA string) error {
	newHeadSHA = strings.TrimSpace(newHeadSHA)
	if newHeadSHA == "" {
		return fmt.Errorf("new head SHA is required")
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), id); err != nil {
			return err
		}
		task, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if !task.ApprovalStale {
			return fmt.Errorf("task %s has no stale approval to refresh", id)
		}
		if task.RefreshHeadSha == newHeadSHA {
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE tasks SET refresh_head_sha=$1, updated_at=now() WHERE id=$2 AND workspace_id=$3`, newHeadSHA, id, workspace(ctx)); err != nil {
			return err
		}
		if err = s.supersedeVerificationTx(ctx, tx, id, newHeadSHA); err != nil {
			return err
		}
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: "review.refresh_head_advanced", Payload: core.JSONPayload(map[string]any{"workspace": task.WorkspaceID, "task_id": id, "approved_head": task.RefreshBaselineSha, "prior_head": task.RefreshHeadSha, "new_head": newHeadSHA, "review_scope": task.RefreshReviewScope})})
	})
}

func (s *Store) SkipTaskRefresh(ctx context.Context, id, newHeadSHA, reason string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if taskFromDB(before).SetupContract.VerifyStage {
			return fmt.Errorf("verify-stage tasks cannot skip refresh verification")
		}
		if _, err = q.SkipTaskRefresh(ctx, db.SkipTaskRefreshParams{ID: id, WorkspaceID: workspace(ctx), HeadSha: newHeadSHA}); err != nil {
			return err
		}
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: "review.refresh_skipped", Payload: core.JSONPayload(map[string]any{"workspace": before.WorkspaceID, "task_id": id, "reason_code": reason, "approved_head": before.ApprovedHeadSha, "new_head": newHeadSHA})})
	})
}

func (s *Store) CreateConflictFixCommand(ctx context.Context, lease taskops.TaskLease, request store.ConflictFixRequest) (store.ConflictFixResult, error) {
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return store.ConflictFixResult{}, fmt.Errorf("conflict-fix create requires a valid taskops lease")
	}
	if request.TaskID == "" || request.Job.TaskID != request.TaskID || request.Job.Stage != core.StageImplement ||
		request.WorkOrder.ID != request.Job.ID || request.WorkOrder.JobID != request.Job.ID ||
		request.WorkOrder.TaskID != request.TaskID || request.WorkOrder.Stage != core.StageImplement ||
		request.WorkOrder.ReasonCode != "merge-conflict" || request.Intervention.TaskID != request.TaskID ||
		request.Intervention.Action != core.InterventionRedirect || request.Intervention.ReasonCode != "merge-conflict" {
		return store.ConflictFixResult{}, fmt.Errorf("invalid conflict-fix command for task %s", request.TaskID)
	}
	var result store.ConflictFixResult
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		workspaceID := workspace(ctx)
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:task-operation:"+workspaceID+":"+request.TaskID); err != nil {
			return err
		}
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: request.TaskID, WorkspaceID: workspaceID})
		if err != nil {
			return notFound(err, "task %s", request.TaskID)
		}
		rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at,id", workspaceID, request.TaskID)
		if err != nil {
			return err
		}
		var orders []core.WorkOrder
		for rows.Next() {
			order, scanErr := scanWorkOrder(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			orders = append(orders, order)
			if order.Stage == core.StageImplement && order.ReasonCode == "merge-conflict" && core.WorkOrderActiveForConflictDispatch(order) {
				result.WorkOrder = order
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if result.WorkOrder.ID != "" {
			return nil
		}
		eventRows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(request.TaskID), WorkspaceID: workspaceID})
		if err != nil {
			return err
		}
		events := make([]core.Event, len(eventRows))
		for i := range eventRows {
			events[i] = eventFromDB(eventRows[i])
		}
		if store.InterruptedReviewRecoveryNeeded(taskFromDB(before), store.CurrentReviewOrders(orders, events), events) != nil {
			return store.ErrConflictReviewRecovery
		}
		state, transitionCommand, err := core.TransitionConflictDispatch(core.TaskState(before.State))
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		intervention := request.Intervention
		actor := store.ActorFromContext(ctx)
		if intervention.ActorID == "" {
			intervention.ActorID = actor.ID
		}
		if intervention.ActorRole == "" {
			intervention.ActorRole = actor.Role
		}
		if intervention.At.IsZero() {
			intervention.At = now
		}
		if _, err = q.InsertIntervention(ctx, interventionInsertParams(intervention)); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "intervention.redirect", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"reason_code": intervention.ReasonCode, "comment": intervention.Comment}), At: intervention.At}); err != nil {
			return err
		}
		if _, err = q.UpdateTaskTransition(ctx, db.UpdateTaskTransitionParams{ID: request.TaskID, WorkspaceID: workspaceID, State: string(state), NextStage: string(core.StageImplement), RecoveryStage: ""}); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state, "command": transitionCommand})}); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": before.NextStage, "next_stage": core.StageImplement, "recovery_stage": "", "state": state})}); err != nil {
			return err
		}
		if _, err = q.InsertJob(ctx, jobInsertParams(request.Job)); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: request.Job.ID, Kind: "job.created", Payload: core.JSONPayload(request.Job)}); err != nil {
			return err
		}
		order := request.WorkOrder
		if order.CreatedAt.IsZero() {
			order.CreatedAt = now
		}
		if order.QueueEnteredAt.IsZero() {
			order.QueueEnteredAt = order.CreatedAt
		}
		if order.QueueDeadline.IsZero() {
			order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
		}
		order.State, order.Claimable = core.WorkOrderQueued, true
		var blocked bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM task_dependencies edge JOIN tasks dependency
			ON dependency.workspace_id=edge.workspace_id AND dependency.id=edge.depends_on_task_id
			WHERE edge.workspace_id=$1 AND edge.task_id=$2 AND dependency.state<>'merged'
		)`, workspaceID, request.TaskID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			order.QueueBlockedAt, order.Claimable = order.QueueEnteredAt, false
		}
		_, err = tx.Exec(ctx, `INSERT INTO work_orders (
			id, workspace_id, task_id, job_id, stage, state, claimant_id,
			session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
			review_round, review_seat, required_model, required_harness, required_harness_config, execution_timeout, model_enforcement,
			reason_code, review_kind, review_scope, baseline_sha, head_sha,
			queue_entered_at, queue_deadline, queue_blocked_at, execution_started_at, execution_deadline,
			last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
			automatic_retry_count, next_retry_at, retry_suppressed,
			redispatch_count, progress, cost_usd, tokens_in, tokens_out,
			usage_reported, self_reported, created_at, updated_at, served_requirement_snapshot, governance_snapshot
		) VALUES ($1,$2,$3,$4,$5,$6,'','','','','','',NULL,0,0,$7,$8,$9,$10,'',$11,'','',$12,'',$13,$14,$15,NULL,NULL,'','',NULL,NULL,0,NULL,false,0,'',0,0,0,false,false,$16,$16,$17,$18)`,
			order.ID, workspaceID, request.TaskID, request.Job.ID, order.Stage, order.State,
			order.RequiredModel, order.RequiredHarness, harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText,
			order.ReasonCode, order.BaselineSHA, order.QueueEnteredAt, order.QueueDeadline,
			nullableTimeValue(order.QueueBlockedAt), order.CreatedAt, servedRequirementSnapshotJSON(order.ServedRequirementSnapshot), governanceSnapshotJSON(order.GovernanceSnapshot))
		if err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: request.Job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order)}); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: request.Job.ID, Kind: "pipeline.awaiting_work_order", Payload: core.JSONPayload(map[string]any{"stage": core.StageImplement, "execution": "mcp", "reason_code": "merge-conflict"})}); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: request.Job.ID, Kind: "merge.conflict_fix_dispatched", Payload: core.JSONPayload(map[string]any{"workspace": workspaceID, "task_id": request.TaskID, "reason_code": "merge-conflict", "approved_head": request.ApprovedHead, "new_head": request.NewHead, "work_order_id": order.ID})}); err != nil {
			return err
		}
		if _, err = s.enqueueTaskTx(ctx, tx, request.TaskID, workspaceID); err != nil {
			return err
		}
		result = store.ConflictFixResult{WorkOrder: order, Created: true}
		return nil
	})
	return result, err
}

// SubmitImplementationCommand records the submitted implement order and
// completes the job named by the order in one transaction. A refused order
// update, missing job, or non-running job rolls back every write
// (component-work-orders).
func (s *Store) SubmitImplementationCommand(ctx context.Context, lease taskops.TaskLease, request store.ImplementationSubmission) (core.Job, error) {
	if err := store.ValidateImplementationSubmission(request); err != nil {
		return core.Job{}, err
	}
	order := request.Order
	var lifecycleErr error
	var completed core.Job
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var storedTask, storedJob, storedStage string
		if err := tx.QueryRow(ctx, `SELECT task_id, job_id, stage FROM work_orders WHERE workspace_id=$1 AND id=$2`, workspace(ctx), order.ID).Scan(&storedTask, &storedJob, &storedStage); err != nil {
			return notFound(err, "work order %s", order.ID)
		}
		if storedTask != order.TaskID || storedJob != order.JobID || core.Stage(storedStage) != core.StageImplement {
			return fmt.Errorf("work order %s does not match its submission", order.ID)
		}
		// The order update takes the task lock first, matching claim and
		// release ordering; any later refusal rolls the update back.
		var err error
		lifecycleErr, err = s.updateWorkOrderCommandTx(ctx, tx, q, lease, order, core.WorkOrderCmdSubmitForReview)
		if err != nil || lifecycleErr != nil {
			return err
		}
		var current core.JobState
		var jobTaskID string
		found := true
		if err = tx.QueryRow(ctx, `SELECT j.task_id, j.state
			FROM jobs j
			JOIN tasks t ON t.id=j.task_id
			WHERE t.workspace_id=$1 AND j.id=$2
			FOR UPDATE OF j`, workspace(ctx), order.JobID).Scan(&jobTaskID, &current); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			found = false
		}
		if err = store.ValidateSubmissionJob(order, core.Job{ID: order.JobID, TaskID: jobTaskID, State: current}, found); err != nil {
			return err
		}
		jobs, err := q.ListJobs(ctx, db.ListJobsParams{TaskID: order.TaskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return err
		}
		for _, row := range jobs {
			if row.ID == order.JobID {
				completed = jobFromDB(row)
			}
		}
		if completed.ID == "" {
			return fmt.Errorf("work order %s job %s not found for task %s", order.ID, order.JobID, order.TaskID)
		}
		ended := request.EndedAt.UTC()
		if ended.IsZero() {
			ended = time.Now().UTC()
		}
		completed.State, completed.EndedAt = core.JobDone, ended
		if _, err = q.UpdateJob(ctx, jobUpdateParams(completed, workspace(ctx))); err != nil {
			return notFound(err, "job %s", completed.ID)
		}
		return insertEvent(ctx, q, core.Event{TaskID: completed.TaskID, JobID: completed.ID, Kind: "job.updated", Payload: core.JSONPayload(completed)})
	})
	if err != nil {
		return core.Job{}, err
	}
	if lifecycleErr != nil {
		return core.Job{}, lifecycleErr
	}
	return completed, nil
}
