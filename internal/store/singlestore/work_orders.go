package singlestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

const workOrderColumns = `id, task_id, job_id, stage, state, claimant_id,
session_id, attempt_id, client_token_hash, agent, model, worker_id, lease_expires_at,
				review_round, review_seat, required_model, required_harness, required_effort, required_harness_config, execution_timeout, model_enforcement,
				reason_code, review_kind, review_scope, baseline_sha, head_sha,
queue_entered_at, queue_deadline, queue_blocked_at, execution_started_at, execution_deadline,
last_attempt_id, last_attempt_outcome, last_failure_category, last_failure_message, last_failure_detail, last_failure_exit_status, last_failure_at,
automatic_retry_count, next_retry_at, retry_suppressed, retry_suppression_reason,
redispatch_count, operator_direction, checkpoint, continuation_session_id, continuation_attempt_id, continuation_harness, continuation_launch_environment, progress, cost_usd, tokens_in, tokens_out, usage_reported, self_reported,
rate_limit, rate_limit_observed_at, created_at, updated_at, served_requirement_snapshot, governance_snapshot`

func servedRequirementSnapshotJSON(snapshot []core.ServedRequirementContext) any {
	if snapshot == nil {
		return nil
	}
	return core.JSONPayload(snapshot)
}

func governanceSnapshotJSON(snapshot *core.GovernanceSnapshot) any {
	if snapshot == nil {
		return nil
	}
	return core.JSONPayload(snapshot)
}

func checkpointJSON(checkpoint *core.WorkOrderCheckpoint) any {
	if checkpoint == nil {
		return json.RawMessage(`{}`)
	}
	return core.JSONPayload(checkpoint)
}

func scanWorkOrder(row interface{ Scan(...any) error }) (core.WorkOrder, error) {
	var order core.WorkOrder
	var stage, state string
	var harnessConfig, checkpoint, rateLimit, servedRequirementSnapshot, governanceSnapshot []byte
	var lease, queueEntered, queueDeadline, queueBlockedAt, executionStarted, executionDeadline, lastFailureAt, nextRetryAt, rateLimitObservedAt sql.NullTime
	err := row.Scan(&order.ID, &order.TaskID, &order.JobID, &stage, &state, &order.ClaimantID,
		&order.SessionID, &order.AttemptID, &order.ClientTokenHash, &order.Agent, &order.Model, &order.WorkerID, &lease,
		&order.ReviewRound, &order.ReviewSeat, &order.RequiredModel, &order.RequiredHarness, &order.RequiredEffort, &harnessConfig, &order.ExecutionTimeoutText, &order.ModelEnforcement,
		&order.ReasonCode, &order.ReviewKind, &order.ReviewScope, &order.BaselineSHA, &order.HeadSHA,
		&queueEntered, &queueDeadline, &queueBlockedAt, &executionStarted, &executionDeadline,
		&order.LastAttemptID, &order.LastAttemptOutcome, &order.LastFailureCategory, &order.LastFailureMessage, &order.LastFailureDetail, &order.LastFailureExitStatus, &lastFailureAt,
		&order.AutomaticRetryCount, &nextRetryAt, &order.RetrySuppressed, &order.RetrySuppressionReason,
		&order.RedispatchCount, &order.OperatorDirection, &checkpoint, &order.ContinuationSessionID, &order.ContinuationAttemptID,
		&order.ContinuationHarness, &order.ContinuationLaunchEnvironment, &order.Progress, &order.CostUSD, &order.TokensIn,
		&order.TokensOut, &order.UsageReported, &order.SelfReported, &rateLimit, &rateLimitObservedAt, &order.CreatedAt, &order.UpdatedAt, &servedRequirementSnapshot, &governanceSnapshot)
	order.Stage, order.State = core.Stage(stage), core.WorkOrderState(state)
	if len(checkpoint) > 0 && string(checkpoint) != "{}" {
		var value core.WorkOrderCheckpoint
		if err == nil {
			err = json.Unmarshal(checkpoint, &value)
		}
		if err == nil {
			order.Checkpoint = &value
		}
	}
	if len(harnessConfig) > 0 && string(harnessConfig) != "{}" {
		var snapshot core.HarnessSnapshot
		if err == nil {
			err = json.Unmarshal(harnessConfig, &snapshot)
		}
		if err == nil && snapshot.Name != "" {
			order.RequiredHarnessConfig = &snapshot
			order.RequiredEffort = snapshot.Effort
		}
	}
	if lease.Valid {
		order.LeaseExpiresAt = lease.Time
	}
	if queueEntered.Valid {
		order.QueueEnteredAt = queueEntered.Time
	}
	if queueDeadline.Valid {
		order.QueueDeadline = queueDeadline.Time
	}
	if queueBlockedAt.Valid {
		order.QueueBlockedAt = queueBlockedAt.Time
	}
	if executionStarted.Valid {
		order.ExecutionStartedAt = executionStarted.Time
	}
	if executionDeadline.Valid {
		order.ExecutionDeadline = executionDeadline.Time
	}
	if lastFailureAt.Valid {
		order.LastFailureAt = lastFailureAt.Time
	}
	if nextRetryAt.Valid {
		order.NextRetryAt = nextRetryAt.Time
	}
	if len(rateLimit) > 0 && string(rateLimit) != "{}" {
		var status core.RateLimitStatus
		if err == nil {
			err = json.Unmarshal(rateLimit, &status)
		}
		if err == nil && status.Status != "" {
			order.RateLimit = &status
		}
	}
	if rateLimitObservedAt.Valid {
		order.RateLimitObservedAt = rateLimitObservedAt.Time
	}
	if servedRequirementSnapshot != nil && err == nil {
		err = json.Unmarshal(servedRequirementSnapshot, &order.ServedRequirementSnapshot)
	}
	if governanceSnapshot != nil && err == nil {
		var snapshot core.GovernanceSnapshot
		err = json.Unmarshal(governanceSnapshot, &snapshot)
		order.GovernanceSnapshot = &snapshot
	}
	order.Claimable = order.ClaimableAt(time.Now().UTC())
	return order, err
}

func harnessSnapshotJSON(snapshot *core.HarnessSnapshot) []byte {
	if snapshot == nil {
		return []byte("{}")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func rateLimitJSON(status *core.RateLimitStatus) []byte {
	if status == nil {
		return nil
	}
	data, err := json.Marshal(status)
	if err != nil {
		return nil
	}
	return data
}

func orderValues(o core.WorkOrder) map[string]any {
	return map[string]any{
		"id":                              o.ID,
		"task_id":                         o.TaskID,
		"job_id":                          o.JobID,
		"stage":                           o.Stage,
		"state":                           o.State,
		"claimant_id":                     o.ClaimantID,
		"session_id":                      o.SessionID,
		"attempt_id":                      o.AttemptID,
		"client_token_hash":               o.ClientTokenHash,
		"agent":                           o.Agent,
		"model":                           o.Model,
		"worker_id":                       o.WorkerID,
		"lease_expires_at":                nullableTimeValue(o.LeaseExpiresAt),
		"review_round":                    o.ReviewRound,
		"review_seat":                     o.ReviewSeat,
		"required_model":                  o.RequiredModel,
		"required_harness":                o.RequiredHarness,
		"required_effort":                 o.RequiredEffort,
		"required_harness_config":         harnessSnapshotJSON(o.RequiredHarnessConfig),
		"execution_timeout":               o.ExecutionTimeoutText,
		"model_enforcement":               o.ModelEnforcement,
		"reason_code":                     o.ReasonCode,
		"review_kind":                     o.ReviewKind,
		"review_scope":                    o.ReviewScope,
		"baseline_sha":                    o.BaselineSHA,
		"head_sha":                        o.HeadSHA,
		"queue_entered_at":                nullableTimeValue(o.QueueEnteredAt),
		"queue_deadline":                  nullableTimeValue(o.QueueDeadline),
		"queue_blocked_at":                nullableTimeValue(o.QueueBlockedAt),
		"execution_started_at":            nullableTimeValue(o.ExecutionStartedAt),
		"execution_deadline":              nullableTimeValue(o.ExecutionDeadline),
		"last_attempt_id":                 o.LastAttemptID,
		"last_attempt_outcome":            o.LastAttemptOutcome,
		"last_failure_category":           o.LastFailureCategory,
		"last_failure_message":            o.LastFailureMessage,
		"last_failure_detail":             o.LastFailureDetail,
		"last_failure_exit_status":        o.LastFailureExitStatus,
		"last_failure_at":                 nullableTimeValue(o.LastFailureAt),
		"automatic_retry_count":           o.AutomaticRetryCount,
		"next_retry_at":                   nullableTimeValue(o.NextRetryAt),
		"retry_suppressed":                o.RetrySuppressed,
		"retry_suppression_reason":        o.RetrySuppressionReason,
		"redispatch_count":                o.RedispatchCount,
		"operator_direction":              o.OperatorDirection,
		"checkpoint":                      checkpointJSON(o.Checkpoint),
		"continuation_session_id":         o.ContinuationSessionID,
		"continuation_attempt_id":         o.ContinuationAttemptID,
		"continuation_harness":            o.ContinuationHarness,
		"continuation_launch_environment": o.ContinuationLaunchEnvironment,
		"progress":                        o.Progress,
		"cost_usd":                        o.CostUSD,
		"tokens_in":                       o.TokensIn,
		"tokens_out":                      o.TokensOut,
		"usage_reported":                  o.UsageReported,
		"self_reported":                   o.SelfReported,
		"rate_limit":                      rateLimitJSON(o.RateLimit),
		"rate_limit_observed_at":          nullableTimeValue(o.RateLimitObservedAt),
		"created_at":                      o.CreatedAt,
		"updated_at":                      o.UpdatedAt,
		"served_requirement_snapshot":     servedRequirementSnapshotJSON(o.ServedRequirementSnapshot),
		"governance_snapshot":             governanceSnapshotJSON(o.GovernanceSnapshot),
	}
}
func getOrderRow(ctx context.Context, db s2log.Executor, id string) (core.WorkOrder, error) {
	o, err := scanWorkOrder(documentRow(ctx, db, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), id))
	return o, notFound(err, "work order %s", id)
}
func orderWrite(ctx context.Context, tx *sql.Tx, o core.WorkOrder) error {
	values := orderValues(o)
	delete(values, "id")
	delete(values, "task_id")
	delete(values, "job_id")
	delete(values, "created_at")
	_, err := writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "UPDATE", values: values, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": o.ID}})
	return err
}
func (s *Store) orderTx(ctx context.Context, id string, fn func(*sql.Tx, core.WorkOrder) error) error {
	var task string
	if err := documentRow(ctx, s.db, `SELECT task_id FROM work_orders WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), id).Scan(&task); err != nil {
		return notFound(err, "work order %s", id)
	}
	return s.taskTx(ctx, task, func(tx *sql.Tx) error {
		o, err := getOrderRow(ctx, tx, id)
		if err != nil {
			return err
		}
		return fn(tx, o)
	})
}
func (s *Store) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	o, err := getOrderRow(ctx, s.db, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	o = store.ProjectWorkOrderAt(o, time.Now().UTC())
	orders := []core.WorkOrder{o}
	if err = s.hydrateWorkOrderAssignees(ctx, orders); err != nil {
		return core.WorkOrder{}, err
	}
	return orders[0], nil
}
func (s *Store) listOrders(ctx context.Context, db s2log.Executor, ids []string) ([]core.WorkOrder, error) {
	query := `SELECT ` + workOrderColumns + ` FROM work_orders WHERE workspace_id=?`
	args := []any{documentWorkspace(ctx)}
	if ids != nil {
		if len(ids) == 0 {
			return []core.WorkOrder{}, nil
		}
		query += ` AND task_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	rows, err := documentRows(ctx, db, query+` ORDER BY created_at,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.WorkOrder{}
	for rows.Next() {
		o, err := scanWorkOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *Store) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	orders, err := s.listOrders(ctx, s.db, nil)
	if err == nil {
		projectOrders(orders)
		err = s.hydrateWorkOrderAssignees(ctx, orders)
	}
	return orders, err
}
func (s *Store) ListWorkOrdersForTasks(ctx context.Context, ids []string) ([]core.WorkOrder, error) {
	if len(ids) == 0 {
		return []core.WorkOrder{}, nil
	}
	orders, err := s.listOrders(ctx, s.db, ids)
	projectOrders(orders)
	return orders, err
}
func (s *Store) ListTaskWorkOrders(ctx context.Context, id string) ([]core.WorkOrder, error) {
	orders, err := s.listOrders(ctx, s.db, []string{id})
	projectOrders(orders)
	return orders, err
}
func (s *Store) ListTaskWorkOrdersSnapshot(ctx context.Context, id string) ([]core.WorkOrder, error) {
	orders, err := s.listOrders(ctx, s.db, []string{id})
	projectOrders(orders)
	return orders, err
}
func (s *Store) hydrateWorkOrderAssignees(ctx context.Context, orders []core.WorkOrder) error {
	for i := range orders {
		var a core.TaskAssignee
		err := documentRow(ctx, s.db, `SELECT u.id,u.email,u.display_name FROM tasks t JOIN workspace_role_bindings b ON b.workspace_id=t.workspace_id AND b.user_id=t.assignee_user_id JOIN users u ON u.id=b.user_id WHERE t.workspace_id=? AND t.id=?`, documentWorkspace(ctx), orders[i].TaskID).Scan(&a.UserID, &a.Email, &a.DisplayName)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		orders[i].Assignee = &a
	}
	return nil
}
func orderDefaults(o core.WorkOrder) core.WorkOrder {
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	o.UpdatedAt = o.CreatedAt
	if o.QueueEnteredAt.IsZero() {
		o.QueueEnteredAt = o.CreatedAt
	}
	if o.QueueDeadline.IsZero() {
		o.QueueDeadline = o.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
	}
	if o.State == "" {
		o.State = core.WorkOrderQueued
	}
	return o
}
func taskBlockedTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var blocked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_dependencies e JOIN tasks d ON d.workspace_id=e.workspace_id AND d.id=e.depends_on_task_id WHERE e.workspace_id=? AND e.task_id=? AND d.state<>'merged')`, documentWorkspace(ctx), id).Scan(&blocked)
	return blocked, err
}
func (s *Store) insertOrderTx(ctx context.Context, tx *sql.Tx, o core.WorkOrder, repin bool) error {
	if _, err := getTaskRow(ctx, tx, o.TaskID); err != nil {
		return err
	}
	var linked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE workspace_id=? AND id=? AND task_id=? AND stage=?)`, documentWorkspace(ctx), o.JobID, o.TaskID, o.Stage).Scan(&linked); err != nil {
		return err
	}
	if !linked {
		return fmt.Errorf("work order task %s and job %s are not linked in workspace %s", o.TaskID, o.JobID, documentWorkspace(ctx))
	}
	if o.Stage == core.StageImplement {
		blocked, err := taskBlockedTx(ctx, tx, o.TaskID)
		if err != nil {
			return err
		}
		if blocked {
			o.QueueBlockedAt = o.QueueEnteredAt
			o.Claimable = false
		}
		if repin {
			if err = repinTaskDesignContextTx(ctx, tx, documentWorkspace(ctx), o.TaskID, o.QueueEnteredAt); err != nil {
				return err
			}
		}
	}
	if err := lockKey(ctx, tx, "work-order-id:"+o.ID); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_orders WHERE id=?`, o.ID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("work order %s already exists", o.ID)
	}
	values := orderValues(o)
	values["workspace_id"] = documentWorkspace(ctx)
	if _, err := writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "INSERT", values: values}); err != nil {
		return err
	}
	o.Claimable = o.ClaimableAt(time.Now().UTC())
	return taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.created", Payload: core.JSONPayload(o)})
}
func (s *Store) CreateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, o core.WorkOrder) error {
	if !lease.ValidForCommand(o.TaskID, string(core.WorkOrderCmdCreate)) {
		return fmt.Errorf("work-order create requires a valid taskops lease")
	}
	if o.State != "" && o.State != core.WorkOrderQueued {
		return &core.ErrInvalidTransition{Space: core.WorkOrderLifecycle, From: "", Command: string(core.WorkOrderCmdCreate), Allowed: []core.TransitionAlternative{{Command: string(core.WorkOrderCmdCreate), To: string(core.WorkOrderQueued)}}}
	}
	o = orderDefaults(o)
	return s.taskTx(ctx, o.TaskID, func(tx *sql.Tx) error {
		if o.Stage != core.StageReview {
			var active string
			err := tx.QueryRowContext(ctx, `SELECT id FROM work_orders WHERE workspace_id=? AND task_id=? AND stage=? AND state='claimed' ORDER BY created_at,id LIMIT 1`, documentWorkspace(ctx), o.TaskID, o.Stage).Scan(&active)
			if err == nil {
				return fmt.Errorf("work order %s cannot be created while same-stage order %s is actively claimed", o.ID, active)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if err := s.insertOrderTx(ctx, tx, o, true); err != nil {
			return err
		}
		return s.retireOrderSiblingsTx(ctx, tx, o, "superseded by successor creation", time.Now().UTC(), true)
	})
}
func (s *Store) CreateStageWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, j core.Job, o core.WorkOrder) (bool, error) {
	if !lease.ValidForCommand(j.TaskID, string(core.WorkOrderCmdCreate)) {
		return false, fmt.Errorf("stage work-order create requires a valid taskops lease")
	}
	if j.Stage == core.StageReview || o.Stage != j.Stage || o.TaskID != j.TaskID || o.JobID != j.ID || o.ID != j.ID {
		return false, fmt.Errorf("invalid stage work order %s", o.ID)
	}
	created := false
	err := s.taskTx(ctx, j.TaskID, func(tx *sql.Tx) error {
		if _, err := getTaskRow(ctx, tx, j.TaskID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_orders WHERE workspace_id=? AND task_id=? AND stage=? AND state IN ('queued','claimed'))`, documentWorkspace(ctx), j.TaskID, o.Stage).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		if err := insertJobRow(ctx, tx, j); err != nil {
			return err
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.created", Payload: core.JSONPayload(j)}); err != nil {
			return err
		}
		o = orderDefaults(o)
		o.State = core.WorkOrderQueued
		if err := s.insertOrderTx(ctx, tx, o, false); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}
func (s *Store) retireOrderSiblingsTx(ctx context.Context, tx *sql.Tx, authoritative core.WorkOrder, reason string, now time.Time, olderOnly bool) error {
	if authoritative.Stage == core.StageReview {
		return nil
	}
	orders, err := s.listOrders(ctx, tx, []string{authoritative.TaskID})
	if err != nil {
		return err
	}
	for _, o := range orders {
		if o.Stage != authoritative.Stage || o.ID == authoritative.ID || (o.State != core.WorkOrderQueued && o.State != core.WorkOrderStale) {
			continue
		}
		if olderOnly && (!o.CreatedAt.Before(authoritative.CreatedAt) && (!o.CreatedAt.Equal(authoritative.CreatedAt) || o.ID >= authoritative.ID)) {
			continue
		}
		prior := o.State
		o.State = core.WorkOrderCancelled
		o.ClaimantID = ""
		o.SessionID = ""
		o.AttemptID = ""
		o.ClientTokenHash = ""
		o.Agent = ""
		o.Model = ""
		o.WorkerID = ""
		o.LeaseExpiresAt = time.Time{}
		o.ModelEnforcement = ""
		o.LastAttemptOutcome = core.WorkOrderOutcomeCancelled
		o.NextRetryAt = time.Time{}
		o.RetrySuppressed = true
		o.RetrySuppressionReason = "superseded"
		o.UpdatedAt = now
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='failed',ended_at=COALESCE(ended_at,?),updated_at=? WHERE workspace_id=? AND id=?`, now, now, documentWorkspace(ctx), o.JobID); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.retired", At: now, Payload: core.JSONPayload(map[string]any{"work_order_id": o.ID, "authoritative_work_order_id": authoritative.ID, "stage": o.Stage, "prior_state": prior, "new_state": core.WorkOrderCancelled, "reason": reason})}); err != nil {
			return err
		}
	}
	return nil
}
func clearOrderClaim(o *core.WorkOrder) {
	o.ClaimantID = ""
	o.SessionID = ""
	o.AttemptID = ""
	o.ClientTokenHash = ""
	o.Agent = ""
	o.Model = ""
	o.WorkerID = ""
	o.LeaseExpiresAt = time.Time{}
	o.ModelEnforcement = ""
}
func resetOrderJob(ctx context.Context, tx *sql.Tx, o core.WorkOrder, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=? WHERE workspace_id=? AND id=?`, now, documentWorkspace(ctx), o.JobID)
	return err
}
func (s *Store) expireWorkOrderClaimTx(ctx context.Context, tx *sql.Tx, o core.WorkOrder, now time.Time) (core.WorkOrder, error) {
	state, err := core.TransitionWorkOrder(o.State, core.WorkOrderCmdExpire)
	if err != nil {
		return core.WorkOrder{}, err
	}
	attempt := o.AttemptID
	run := core.IsTaskRunClaimantID(o.ClaimantID)
	clearOrderClaim(&o)
	o.State = state
	o.LastAttemptID = attempt
	o.ExecutionStartedAt = time.Time{}
	o.ExecutionDeadline = time.Time{}
	o.LastAttemptOutcome = core.WorkOrderOutcomeExpired
	o.NextRetryAt = time.Time{}
	o.RetrySuppressed = !run
	o.UpdatedAt = now
	if err = orderWrite(ctx, tx, o); err != nil {
		return core.WorkOrder{}, err
	}
	if err = resetOrderJob(ctx, tx, o, now); err != nil {
		return core.WorkOrder{}, err
	}
	err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.expired", At: now, Payload: core.JSONPayload(map[string]any{"attempt_id": attempt, "outcome": o.LastAttemptOutcome, "release_cause": core.WorkOrderReleaseCauseLeaseLoss, "retry_suppressed": o.RetrySuppressed})})
	return o, err
}
func (s *Store) transitionWorkOrderTx(ctx context.Context, tx *sql.Tx, o core.WorkOrder, command core.WorkOrderCommand, kind string, now time.Time) (core.WorkOrder, error) {
	state, err := core.TransitionWorkOrder(o.State, command)
	if err != nil {
		return core.WorkOrder{}, err
	}
	attempt := ""
	if state == core.WorkOrderTimedOut && o.State == core.WorkOrderClaimed {
		attempt = o.AttemptID
		clearOrderClaim(&o)
		o.LastAttemptID = attempt
	}
	o.State = state
	o.LeaseExpiresAt = time.Time{}
	o.UpdatedAt = now
	if err = orderWrite(ctx, tx, o); err != nil {
		return core.WorkOrder{}, err
	}
	if state == core.WorkOrderTimedOut {
		ended := now
		if !o.ExecutionDeadline.IsZero() && !o.ExecutionDeadline.After(now) {
			ended = o.ExecutionDeadline
		}
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='failed',ended_at=COALESCE(ended_at,?),updated_at=? WHERE workspace_id=? AND id=?`, ended, now, documentWorkspace(ctx), o.JobID); err != nil {
			return core.WorkOrder{}, err
		}
	}
	eventOrder := o
	eventOrder.AttemptID = attempt
	err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: kind, At: now, Payload: core.JSONPayload(eventOrder)})
	return o, err
}
func reviewSeatAcceptedTx(ctx context.Context, tx *sql.Tx, ws, task, id string) (bool, error) {
	var accepted bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE workspace_id=? AND task_id=? AND kind='review.accepted' AND JSON_EXTRACT_STRING(payload_json,'review_work_order_id')=?)`, ws, task, id).Scan(&accepted)
	return accepted, err
}
func (s *Store) ListElapsedWorkOrderTaskIDs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := documentRows(ctx, s.db, `SELECT DISTINCT task_id FROM work_orders WHERE workspace_id=? AND ((state IN ('queued','claimed') AND execution_deadline IS NOT NULL AND execution_deadline<=?) OR (state='claimed' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?) OR (state='queued' AND execution_started_at IS NULL AND queue_blocked_at IS NULL AND queue_deadline<=?)) ORDER BY task_id`, documentWorkspace(ctx), now, now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (s *Store) ApplyWorkOrderClock(ctx context.Context, lease taskops.TaskLease, id string, now time.Time) (int, error) {
	if !lease.ValidFor(id) {
		return 0, fmt.Errorf("work-order lifecycle mutation requires a valid taskops lease")
	}
	count := 0
	err := s.taskTx(ctx, id, func(tx *sql.Tx) error {
		orders, err := s.listOrders(ctx, tx, []string{id})
		if err != nil {
			return err
		}
		blocked, err := taskBlockedTx(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, o := range orders {
			if o.State != core.WorkOrderQueued && o.State != core.WorkOrderClaimed {
				continue
			}
			if o.Stage == core.StageImplement && o.State == core.WorkOrderQueued {
				if blocked {
					if o.QueueBlockedAt.IsZero() {
						o.QueueBlockedAt = now
						o.UpdatedAt = now
						if err = orderWrite(ctx, tx, o); err != nil {
							return err
						}
					}
					continue
				}
				if !o.QueueBlockedAt.IsZero() {
					o.QueueDeadline = o.QueueDeadline.Add(now.Sub(o.QueueBlockedAt))
					o.QueueBlockedAt = time.Time{}
					o.UpdatedAt = now
					if err = orderWrite(ctx, tx, o); err != nil {
						return err
					}
				}
			}
			if !o.ExecutionDeadline.IsZero() && !o.ExecutionDeadline.After(now) {
				_, err = s.transitionWorkOrderTx(ctx, tx, o, core.WorkOrderCmdTimeout, "work_order.timed_out", now)
			} else if o.State == core.WorkOrderClaimed && !o.LeaseExpiresAt.After(now) {
				_, err = s.expireWorkOrderClaimTx(ctx, tx, o, now)
			} else if o.State == core.WorkOrderQueued && o.ExecutionStartedAt.IsZero() && o.QueueBlockedAt.IsZero() && !o.QueueDeadline.IsZero() && !o.QueueDeadline.After(now) {
				_, err = s.transitionWorkOrderTx(ctx, tx, o, core.WorkOrderCmdMarkStale, "work_order.stale", now)
			} else {
				continue
			}
			if err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) UpdateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder, commands ...core.WorkOrderCommand) error {
	var lifecycleErr error
	err := s.orderTx(ctx, order.ID, func(tx *sql.Tx, current core.WorkOrder) error {
		var err error
		now := time.Now().UTC()
		if (current.State == core.WorkOrderQueued || current.State == core.WorkOrderClaimed) &&
			!current.ExecutionDeadline.IsZero() && !current.ExecutionDeadline.After(now) {
			if _, err = s.transitionWorkOrderTx(ctx, tx, current, core.WorkOrderCmdTimeout, "work_order.timed_out", now); err != nil {
				return err
			}
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, order.ID)
			return nil
		}
		if current.State == core.WorkOrderTimedOut {
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, order.ID)
			return nil
		}
		if current.State == core.WorkOrderStale {
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderStale, order.ID)
			return nil
		}
		if current.State == core.WorkOrderCancelled {
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderCancelled, order.ID)
			return nil
		}
		if current.State == core.WorkOrderClaimed && !current.LeaseExpiresAt.After(now) {
			if _, err = s.expireWorkOrderClaimTx(ctx, tx, current, now); err != nil {
				return err
			}
			lifecycleErr = fmt.Errorf("work order lease expired")
			return nil
		}
		if updateRequiresClaim(order.State, current.State) &&
			(current.State != core.WorkOrderClaimed || current.SessionID == "" || current.SessionID != order.SessionID) {
			return fmt.Errorf("work order %s is not claimed by this session", order.ID)
		}
		command := taskops.WorkOrderMetadataCommand
		if len(commands) == 1 {
			command = commands[0]
		} else if current.State != order.State {
			if inferred, inferredOK := store.InferWorkOrderUpdateCommand(current, order); inferredOK {
				command = inferred
			}
		}
		if !lease.ValidForCommand(order.TaskID, string(command)) {
			return fmt.Errorf("work-order update requires a valid taskops lease")
		}
		if current.State != order.State {
			if len(commands) == 0 {
				if inferred, ok := store.InferWorkOrderUpdateCommand(current, order); ok {
					commands = []core.WorkOrderCommand{inferred}
				}
			}
			if len(commands) != 1 {
				return fmt.Errorf("work order %s state change requires exactly one lifecycle command", order.ID)
			}
			to, transitionErr := core.TransitionWorkOrder(current.State, commands[0])
			if transitionErr != nil {
				return transitionErr
			}
			if to != order.State {
				return &core.ErrInvalidTransition{Space: core.WorkOrderLifecycle, From: string(current.State), Command: string(commands[0]), Allowed: core.WorkOrderTransitionAlternatives(current.State)}
			}
		}
		if order.State == core.WorkOrderCompleted {
			order.OperatorDirection = ""
			order.ContinuationSessionID, order.ContinuationAttemptID = "", ""
			order.ContinuationHarness, order.ContinuationLaunchEnvironment = "", ""
		}
		values := orderValues(order)
		allowed := map[string]bool{"state": true, "claimant_id": true, "session_id": true, "attempt_id": true, "client_token_hash": true, "agent": true, "model": true, "lease_expires_at": true, "model_enforcement": true, "queue_entered_at": true, "queue_deadline": true, "execution_started_at": true, "execution_deadline": true, "last_attempt_id": true, "last_attempt_outcome": true, "last_failure_category": true, "last_failure_message": true, "last_failure_detail": true, "last_failure_exit_status": true, "last_failure_at": true, "automatic_retry_count": true, "next_retry_at": true, "retry_suppressed": true, "retry_suppression_reason": true, "redispatch_count": true, "progress": true, "cost_usd": true, "tokens_in": true, "tokens_out": true, "usage_reported": true, "self_reported": true, "operator_direction": true, "continuation_session_id": true, "continuation_attempt_id": true, "continuation_harness": true, "continuation_launch_environment": true, "rate_limit": true, "rate_limit_observed_at": true, "updated_at": true}
		for key := range values {
			if !allowed[key] {
				delete(values, key)
			}
		}
		values["updated_at"] = now
		if _, err = writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "UPDATE", values: values, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": current.ID}}); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order)}); err != nil {
			return err
		}
		if current.State != core.WorkOrderCompleted && order.State == core.WorkOrderCompleted {
			return s.retireOrderSiblingsTx(ctx, tx, order, "stage completed", now, false)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return lifecycleErr
}

func updateRequiresClaim(next, current core.WorkOrderState) bool {
	if next == core.WorkOrderClaimed {
		return true
	}
	return current != next && (next == core.WorkOrderSubmitted || next == core.WorkOrderCompleted)
}

func (s *Store) claimOwnerTx(ctx context.Context, tx *sql.Tx, claim *core.WorkOrderClaim, task core.Task) error {
	if claim.OwnerUserID == "" {
		if c, ok := store.CredentialFromContext(ctx); ok {
			claim.OwnerUserID = c.OwnerUserID
		}
	}
	if claim.WorkerID != "" {
		var owner sql.NullString
		var revoked sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT owner_user_id,revoked_at FROM workers WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), claim.WorkerID).Scan(&owner, &revoked)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if revoked.Valid {
				return fmt.Errorf("%w: worker %s", store.ErrWorkerUnauthorized, claim.WorkerID)
			}
			claim.OwnerUserID = owner.String
			if owner.Valid {
				var status string
				err = tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id=? FOR UPDATE`, owner.String).Scan(&status)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err != nil || status != "active" {
					return store.ErrWorkerUnauthorized
				}
				var exists int
				err = tx.QueryRowContext(ctx, `SELECT 1 FROM workspace_role_bindings WHERE workspace_id=? AND user_id=? FOR UPDATE`, documentWorkspace(ctx), owner.String).Scan(&exists)
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: worker %s owner has no live workspace binding", store.ErrWorkerUnauthorized, claim.WorkerID)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	if claim.RequireForgeToken {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id=? FOR UPDATE`, claim.OwnerUserID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrForgeTokenRequired
		}
		if err != nil {
			return err
		}
		if status != "active" {
			return store.ErrForgeTokenRequired
		}
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT user_id FROM user_forge_tokens WHERE user_id=? FOR UPDATE`, claim.OwnerUserID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrForgeTokenRequired
		}
		if err != nil {
			return err
		}
	}
	if task.Assignee != nil && task.Assignee.UserID != claim.OwnerUserID {
		return fmt.Errorf("task %s is assigned to %s; only that assignee may claim its work orders", task.ID, task.Assignee.UserID)
	}
	return nil
}
func (s *Store) reviewClaimGuardTx(ctx context.Context, tx *sql.Tx, o core.WorkOrder, claim core.WorkOrderClaim, hash string) error {
	var id string
	var version int
	err := tx.QueryRowContext(ctx, `SELECT document_id,version FROM system_design_versions WHERE workspace_id=? AND origin=? AND origin_task_id=? AND NOT confirmed AND NOT dismissed ORDER BY document_id,version LIMIT 1`, documentWorkspace(ctx), core.SystemDesignOriginImplementation, o.TaskID).Scan(&id, &version)
	if err == nil {
		return fmt.Errorf("review for task %s is waiting on task-authored System Design proposal %s v%d", o.TaskID, id, version)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT requirement_id,version FROM requirement_versions WHERE workspace_id=? AND origin=? AND origin_task_id=? AND NOT confirmed AND NOT retired ORDER BY requirement_id,version LIMIT 1`, documentWorkspace(ctx), core.RequirementOriginImplementation, o.TaskID).Scan(&id, &version)
	if err == nil {
		return fmt.Errorf("review for task %s is waiting on task-authored requirement proposal %s v%d", o.TaskID, id, version)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	accepted, err := reviewSeatAcceptedTx(ctx, tx, documentWorkspace(ctx), o.TaskID, o.ID)
	if err != nil {
		return err
	}
	if accepted {
		return fmt.Errorf("accepted review seat %s is terminal and cannot be claimed", o.ID)
	}
	orders, err := s.listOrders(ctx, tx, []string{o.TaskID})
	if err != nil {
		return err
	}
	independent := func(candidate core.WorkOrder) bool {
		return candidate.ID != o.ID && (candidate.Stage == core.StageImplement || (candidate.Stage == core.StageReview && candidate.ReviewRound == o.ReviewRound))
	}
	for _, candidate := range orders {
		if independent(candidate) && ((claim.SessionID != "" && candidate.SessionID == claim.SessionID) || (hash != "" && candidate.ClientTokenHash == hash)) {
			return fmt.Errorf("self-review forbidden: review session independence requires a fresh session and client token")
		}
	}
	events, err := documentTaskEvents(ctx, tx, o.TaskID)
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.Kind != "work_order.claimed" {
			continue
		}
		var candidate core.WorkOrder
		if err = json.Unmarshal(e.Payload, &candidate); err != nil {
			return err
		}
		if independent(candidate) && claim.SessionID != "" && candidate.SessionID == claim.SessionID {
			return fmt.Errorf("self-review forbidden: review session independence requires a fresh session and client token")
		}
	}
	if claim.WorkerID != "" && o.RequiredModel != "" && claim.Model != o.RequiredModel {
		return fmt.Errorf("worker review model %q does not match pinned seat model %q", claim.Model, o.RequiredModel)
	}
	return nil
}
func (s *Store) ClaimWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	var result core.WorkOrder
	var lifecycleErr error
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		if !lease.ValidForCommand(o.TaskID, string(core.WorkOrderCmdClaim)) {
			return fmt.Errorf("work-order claim requires a valid taskops lease")
		}
		if o.Stage != core.StageReview {
			var active string
			err := tx.QueryRowContext(ctx, `SELECT id FROM work_orders WHERE workspace_id=? AND task_id=? AND stage=? AND id<>? AND state='claimed' ORDER BY created_at,id LIMIT 1`, documentWorkspace(ctx), o.TaskID, o.Stage, id).Scan(&active)
			if err == nil {
				return fmt.Errorf("work order %s cannot be claimed while same-stage order %s is actively claimed", id, active)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		task, err := getTaskRow(ctx, tx, o.TaskID)
		if err != nil {
			return err
		}
		if err = s.claimOwnerTx(ctx, tx, &claim, task); err != nil {
			return err
		}
		hash := ""
		if claim.ClientToken != "" {
			hash = fmt.Sprintf("%x", sha256.Sum256([]byte(claim.ClientToken)))
		}
		if o.Stage == core.StageReview {
			if err = s.reviewClaimGuardTx(ctx, tx, o, claim, hash); err != nil {
				return err
			}
			if claim.WorkerID != "" {
				o.ModelEnforcement = "worker-pinned"
			} else {
				o.ModelEnforcement = "self-reported"
			}
		}
		now := time.Now().UTC()
		if o.Stage == core.StageImplement {
			blocked, err := taskBlockedTx(ctx, tx, o.TaskID)
			if err != nil {
				return err
			}
			if blocked {
				return fmt.Errorf("task %s is blocked by unmerged dependencies", o.TaskID)
			}
			if !o.QueueBlockedAt.IsZero() {
				o.QueueDeadline = o.QueueDeadline.Add(now.Sub(o.QueueBlockedAt))
				o.QueueBlockedAt = time.Time{}
				o.UpdatedAt = now
				if err = orderWrite(ctx, tx, o); err != nil {
					return err
				}
			}
		}
		if (o.State == core.WorkOrderQueued || o.State == core.WorkOrderClaimed) && !o.ExecutionDeadline.IsZero() && !o.ExecutionDeadline.After(now) {
			if _, err = s.transitionWorkOrderTx(ctx, tx, o, core.WorkOrderCmdTimeout, "work_order.timed_out", now); err != nil {
				return err
			}
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
			return nil
		}
		if o.State == core.WorkOrderQueued && o.ExecutionStartedAt.IsZero() && !o.QueueDeadline.IsZero() && !o.QueueDeadline.After(now) {
			if _, err = s.transitionWorkOrderTx(ctx, tx, o, core.WorkOrderCmdMarkStale, "work_order.stale", now); err != nil {
				return err
			}
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderStale, id)
			return nil
		}
		if o.State == core.WorkOrderStale {
			return fmt.Errorf("%w: %s", store.ErrWorkOrderStale, id)
		}
		if o.State == core.WorkOrderTimedOut {
			return fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
		}
		if o.State == core.WorkOrderClaimed {
			if o.LeaseExpiresAt.After(now) {
				return fmt.Errorf("work order %s is already claimed", id)
			}
			if _, err = s.expireWorkOrderClaimTx(ctx, tx, o, now); err != nil {
				return err
			}
			lifecycleErr = fmt.Errorf("work order %s lease expired; operator recovery is required", id)
			return nil
		}
		if o.State != core.WorkOrderQueued {
			return fmt.Errorf("work order %s is not claimable", id)
		}
		if !o.ClaimableAt(now) {
			if o.RetrySuppressed {
				return fmt.Errorf("work order %s automatic retry is suppressed; operator recovery is required", id)
			}
			return fmt.Errorf("work order %s is in retry backoff until %s", id, o.NextRetryAt.Format(time.RFC3339Nano))
		}
		if !o.ExecutionStartedAt.IsZero() && o.ExecutionDeadline.IsZero() && claim.ExecutionTimeout > 0 {
			o.ExecutionDeadline = o.ExecutionStartedAt.Add(claim.ExecutionTimeout)
			if !o.ExecutionDeadline.After(now) {
				if _, err = s.transitionWorkOrderTx(ctx, tx, o, core.WorkOrderCmdTimeout, "work_order.timed_out", now); err != nil {
					return err
				}
				lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
				return nil
			}
		}
		duration := claim.Lease
		if duration <= 0 {
			duration = core.DefaultWorkOrderClaimLease
		}
		next, err := core.TransitionWorkOrder(o.State, core.WorkOrderCmdClaim)
		if err != nil {
			return err
		}
		if o.ExecutionStartedAt.IsZero() {
			o.ExecutionStartedAt = now
			if claim.ExecutionTimeout > 0 {
				o.ExecutionDeadline = now.Add(claim.ExecutionTimeout)
			}
		}
		o.State = next
		o.ClaimantID = claim.ClaimantID
		o.SessionID = claim.SessionID
		o.AttemptID = core.NewWorkOrderAttemptID()
		o.ClientTokenHash = hash
		o.Agent = claim.Agent
		o.Model = claim.Model
		o.WorkerID = claim.WorkerID
		o.LeaseExpiresAt = now.Add(duration)
		o.UpdatedAt = now
		if o.ServedRequirementSnapshot == nil {
			o.ServedRequirementSnapshot = claim.Requirements
		}
		if claim.Governance != nil {
			if o.GovernanceSnapshot == nil {
				o.GovernanceSnapshot = claim.Governance
			} else {
				o.GovernanceSnapshot.PendingDesignProposals = claim.Governance.PendingDesignProposals
				o.GovernanceSnapshot.ResolutionNotes = claim.Governance.ResolutionNotes
			}
		}
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		o, err = getOrderRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='running',model_tier=?,started_at=COALESCE(started_at,?),updated_at=? WHERE workspace_id=? AND id=?`, claim.Model, o.ExecutionStartedAt, o.ExecutionStartedAt, documentWorkspace(ctx), o.JobID); err != nil {
			return err
		}
		if task.State == core.TaskQueued {
			state, err := core.TransitionTask(task.State, core.TaskOrderClaim)
			if err != nil {
				return err
			}
			if err = taskWrite(ctx, tx, task.ID, map[string]any{"state": state}); err != nil {
				return err
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: task.ID, JobID: o.JobID, Kind: "task.state_changed", At: now, Payload: core.JSONPayload(map[string]any{"from": task.State, "to": state, "command": core.TaskOrderClaim})}); err != nil {
				return err
			}
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(o)}); err != nil {
			return err
		}
		result = o
		result.Claimable = result.ClaimableAt(now)
		return nil
	})
	if err != nil {
		return core.WorkOrder{}, err
	}
	return result, lifecycleErr
}
func (s *Store) orderSupersessionGuardTx(ctx context.Context, tx *sql.Tx, o core.WorkOrder) error {
	t, err := getTaskRow(ctx, tx, o.TaskID)
	if err != nil {
		return err
	}
	orders, err := s.listOrders(ctx, tx, []string{o.TaskID})
	if err != nil {
		return err
	}
	return store.WorkOrderRecoverySupersessionError(t, o, orders)
}
func (s *Store) RedispatchWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, timeout time.Duration) (core.WorkOrder, error) {
	if timeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	var result core.WorkOrder
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		if !lease.ValidForCommand(o.TaskID, string(core.WorkOrderCmdRedispatch)) {
			return fmt.Errorf("work-order redispatch requires a valid taskops lease")
		}
		now := time.Now().UTC()
		var err error
		if o.State == core.WorkOrderClaimed && o.LeaseExpiresAt.After(now) {
			return fmt.Errorf("work order %s has an active claim and cannot be redispatched", id)
		}
		if o.State == core.WorkOrderQueued && o.ExecutionStartedAt.IsZero() && !o.QueueDeadline.IsZero() && !o.QueueDeadline.After(now) {
			o, err = s.transitionWorkOrderTx(ctx, tx, o, core.WorkOrderCmdMarkStale, "work_order.stale", now)
			if err != nil {
				return err
			}
		}
		if o.State != core.WorkOrderStale {
			return fmt.Errorf("work order %s is not stale and cannot be redispatched", id)
		}
		if !o.ExecutionStartedAt.IsZero() {
			return fmt.Errorf("work order %s was already claimed and requires operator recovery", id)
		}
		if err = s.orderSupersessionGuardTx(ctx, tx, o); err != nil {
			return err
		}
		state, err := core.TransitionWorkOrder(o.State, core.WorkOrderCmdRedispatch)
		if err != nil {
			return err
		}
		clearOrderClaim(&o)
		o.State = state
		o.QueueEnteredAt = now
		o.QueueDeadline = now.Add(timeout)
		o.ExecutionStartedAt = time.Time{}
		o.ExecutionDeadline = time.Time{}
		o.RedispatchCount++
		o.Progress = ""
		o.UpdatedAt = now
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		if err = resetOrderJob(ctx, tx, o, now); err != nil {
			return err
		}
		if o.Stage == core.StageImplement {
			if err = repinTaskDesignContextTx(ctx, tx, documentWorkspace(ctx), o.TaskID, now); err != nil {
				return err
			}
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.redispatched", At: now, Payload: core.JSONPayload(map[string]any{"work_order_id": id, "prior_state": core.WorkOrderStale, "new_state": o.State, "command": core.WorkOrderCmdRedispatch, "reason": "stale never-claimed queue redispatch"})}); err != nil {
			return err
		}
		if err = s.retireOrderSiblingsTx(ctx, tx, o, "superseded by stale redispatch", now, false); err != nil {
			return err
		}
		result, err = getOrderRow(ctx, tx, id)
		return err
	})
	return result, err
}
func (s *Store) RecoverWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id, requestID, direction string, timeout time.Duration, refreeze ...*store.RecoveryRefreeze) (core.WorkOrder, error) {
	direction, err := core.NormalizeWorkOrderOperatorDirection(direction)
	if err != nil {
		return core.WorkOrder{}, err
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return core.WorkOrder{}, fmt.Errorf("recovery request_id is required")
	}
	if timeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	var result core.WorkOrder
	err = s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		if !lease.ValidForCommand(o.TaskID, string(core.WorkOrderCmdRecover)) {
			return fmt.Errorf("work-order recovery requires a valid taskops lease")
		}
		var duplicate bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_order_recoveries WHERE workspace_id=? AND work_order_id=? AND request_id=?)`, documentWorkspace(ctx), id, requestID).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate {
			result = o
			return nil
		}
		now := time.Now().UTC()
		var err error
		if o.State == core.WorkOrderQueued && o.ExecutionStartedAt.IsZero() && !o.QueueDeadline.IsZero() && !o.QueueDeadline.After(now) {
			o, err = s.transitionWorkOrderTx(ctx, tx, o, core.WorkOrderCmdMarkStale, "work_order.stale", now)
			if err != nil {
				return err
			}
		}
		eligible := o.State == core.WorkOrderQueued && (o.LastAttemptOutcome != "" || o.RetrySuppressed || !o.NextRetryAt.IsZero())
		if !eligible && o.State != core.WorkOrderStale && o.State != core.WorkOrderTimedOut {
			return fmt.Errorf("work order %s is not released, expired, or retry-suppressed", id)
		}
		if err = s.orderSupersessionGuardTx(ctx, tx, o); err != nil {
			return err
		}
		prior := o
		transient := 0
		if prior.LastFailureCategory == core.WorkOrderFailureTransientConnectivity {
			err = tx.QueryRowContext(ctx, `SELECT COALESCE(JSON_EXTRACT_BIGINT(payload_json,'consecutive_transient_failures'),0) FROM events WHERE workspace_id=? AND task_id=? AND job_id=? AND kind IN ('work_order.child_failed','work_order.stalled') ORDER BY id DESC LIMIT 1`, documentWorkspace(ctx), o.TaskID, o.JobID).Scan(&transient)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		command := core.WorkOrderCmdRecover
		kind := "work_order.recovered"
		if o.State == core.WorkOrderQueued {
			command = ""
			kind = "work_order.redispatched"
		} else if _, err = core.TransitionWorkOrder(o.State, command); err != nil {
			return err
		}
		if len(refreeze) > 0 && refreeze[0] != nil {
			change := refreeze[0]
			task, err := getTaskRow(ctx, tx, o.TaskID)
			if err != nil {
				return err
			}
			setup, err := json.Marshal(change.Setup)
			if err != nil {
				return err
			}
			if err = taskWrite(ctx, tx, o.TaskID, map[string]any{"setup_contract": setup}); err != nil {
				return err
			}
			o.RequiredModel = change.RequiredModel
			o.RequiredHarness = change.RequiredHarness
			o.RequiredEffort = change.RequiredEffort
			o.RequiredHarnessConfig = change.RequiredHarnessConfig
			o.ExecutionTimeoutText = change.ExecutionTimeoutText
			if !reflect.DeepEqual(task.SetupContract, change.Setup) {
				actor := store.ActorFromContext(ctx)
				if err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "task.setup.refrozen", ActorID: actor.ID, ActorRole: actor.Role, At: now, Payload: core.JSONPayload(map[string]any{"prior": task.SetupContract, "new": change.Setup, "request_id": requestID, "work_order_id": id, "actor": actor.ID})}); err != nil {
					return err
				}
			}
		}
		clearOrderClaim(&o)
		o.State = core.WorkOrderQueued
		o.ExecutionStartedAt = time.Time{}
		o.ExecutionDeadline = time.Time{}
		o.LastAttemptOutcome = ""
		o.RetrySuppressed = false
		o.RetrySuppressionReason = ""
		o.AutomaticRetryCount = 0
		o.NextRetryAt = time.Time{}
		o.QueueEnteredAt = now
		o.QueueDeadline = now.Add(timeout)
		o.RedispatchCount++
		o.OperatorDirection = direction
		o.UpdatedAt = now
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO work_order_recoveries (workspace_id,work_order_id,request_id,created_at) VALUES (?,?,?,?)`, documentWorkspace(ctx), id, requestID, now); err != nil {
			return err
		}
		if o.Stage == core.StageImplement {
			if err = repinTaskDesignContextTx(ctx, tx, documentWorkspace(ctx), o.TaskID, now); err != nil {
				return err
			}
		}
		if err = resetOrderJob(ctx, tx, o, now); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: kind, At: now, Payload: core.JSONPayload(map[string]any{"attempt_id": prior.LastAttemptID, "workspace_id": documentWorkspace(ctx), "work_order_id": id, "request_id": requestID, "prior_state": prior.State, "prior_outcome": prior.LastAttemptOutcome, "new_state": o.State, "command": command, "reason": "operator recovery", "direction": direction, "failure_category": prior.LastFailureCategory, "consecutive_transient_failures": transient, "next_retry_at": prior.NextRetryAt})}); err != nil {
			return err
		}
		result, err = getOrderRow(ctx, tx, id)
		return err
	})
	return result, err
}

func (s *Store) RefreshWorkOrderHarnessSnapshot(ctx context.Context, id string, snapshot *core.HarnessSnapshot) (core.WorkOrder, error) {
	var result core.WorkOrder
	if snapshot == nil || snapshot.Name == "" {
		return result, fmt.Errorf("harness snapshot requires a name")
	}
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		if o.Stage == core.StageReview {
			accepted, err := reviewSeatAcceptedTx(ctx, tx, documentWorkspace(ctx), o.TaskID, o.ID)
			if err != nil {
				return err
			}
			if accepted {
				return fmt.Errorf("review work order already accepted")
			}
		}
		if (o.State != core.WorkOrderQueued && o.State != core.WorkOrderStale) || o.SessionID != "" || o.WorkerID != "" {
			return fmt.Errorf("work order %s is not an unclaimed queued or stale order", id)
		}
		if o.RequiredHarnessConfig == nil || o.RequiredHarnessConfig.Name != snapshot.Name {
			return fmt.Errorf("harness snapshot does not match frozen harness")
		}
		previous := o.RequiredHarnessConfig.Command
		o.RequiredHarnessConfig = snapshot
		o.UpdatedAt = time.Now().UTC()
		if err := orderWrite(ctx, tx, o); err != nil {
			return err
		}
		result = o
		return taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.harness_refreshed", Payload: core.JSONPayload(map[string]any{"work_order_id": o.ID, "previous_command": previous, "command": snapshot.Command})})
	})
	return result, err
}
func (s *Store) RequestPlanRevisionCommand(ctx context.Context, lease taskops.TaskLease, id string, claim core.WorkOrderClaimIdentity, rationale string) (store.PlanRevisionRequestResult, error) {
	var result store.PlanRevisionRequestResult
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return result, fmt.Errorf("rationale is required")
	}
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		if !lease.ValidForCommand(o.TaskID, string(core.WorkOrderCmdRequestPlanRevision)) {
			return fmt.Errorf("plan revision request requires a valid taskops lease")
		}
		now := time.Now().UTC()
		if o.SessionID == "" || o.SessionID != claim.SessionID || (o.State != core.WorkOrderClaimed || !o.LeaseExpiresAt.After(now) || (!o.ExecutionDeadline.IsZero() && !o.ExecutionDeadline.After(now))) {
			return store.ErrWorkOrderClaimLost
		}
		if o.WorkerID != claim.WorkerID || o.ClaimantID != claim.ClaimantID {
			return store.ErrWorkOrderClaimUnauthorized
		}
		if o.Stage != core.StageImplement {
			return fmt.Errorf("request_plan_revision requires an implement-stage work order")
		}
		t, err := getTaskRow(ctx, tx, o.TaskID)
		if err != nil {
			return err
		}
		version := 0
		err = tx.QueryRowContext(ctx, `SELECT version FROM task_specs WHERE workspace_id=? AND task_id=? AND approved=true ORDER BY version DESC LIMIT 1`, documentWorkspace(ctx), t.ID).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) && t.ParentTaskID != "" && t.OriginSpecVersion > 0 {
			err = tx.QueryRowContext(ctx, `SELECT version FROM task_specs WHERE workspace_id=? AND task_id=? AND version=? AND approved=true`, documentWorkspace(ctx), t.ParentTaskID, t.OriginSpecVersion).Scan(&version)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("request_plan_revision requires an approved execution plan")
		}
		if err != nil {
			return err
		}
		nextOrder, err := core.TransitionWorkOrder(o.State, core.WorkOrderCmdRequestPlanRevision)
		if err != nil {
			return err
		}
		nextTask, err := core.TransitionTask(t.State, core.TaskGatePlanRevision)
		if err != nil {
			return err
		}
		timeout := o.QueueDeadline.Sub(o.QueueEnteredAt)
		if timeout <= 0 {
			timeout = config.DefaultWorkOrderQueueTimeout
		}
		attempt := o.AttemptID
		clearOrderClaim(&o)
		o.State = nextOrder
		o.LastAttemptID = attempt
		o.ExecutionStartedAt = time.Time{}
		o.ExecutionDeadline = time.Time{}
		o.LastAttemptOutcome = core.WorkOrderOutcomeReleased
		o.LastFailureCategory = ""
		o.LastFailureMessage = core.WorkOrderReleaseReasonPlanRevisionRequested
		o.LastFailureDetail = ""
		o.LastFailureExitStatus = nil
		o.LastFailureAt = now
		o.NextRetryAt = time.Time{}
		o.RetrySuppressed = true
		o.RetrySuppressionReason = ""
		o.QueueEnteredAt = now
		o.QueueDeadline = now.Add(timeout)
		o.OperatorDirection = ""
		o.UpdatedAt = now
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		if err = resetOrderJob(ctx, tx, o, now); err != nil {
			return err
		}
		if err = taskWrite(ctx, tx, t.ID, map[string]any{"state": nextTask}); err != nil {
			return err
		}
		eventCtx := workerClaimActorContext(ctx, claim.WorkerID)
		for _, e := range []core.Event{{TaskID: t.ID, JobID: o.JobID, Kind: "work_order.plan_revision_requested", Payload: core.JSONPayload(map[string]any{"work_order_id": id, "attempt_id": attempt, "session_id": claim.SessionID, "rationale": rationale, "plan_version": version}), At: now}, {TaskID: t.ID, JobID: o.JobID, Kind: "work_order.released", Payload: core.JSONPayload(map[string]any{"attempt_id": attempt, "session_id": claim.SessionID, "reason": core.WorkOrderReleaseReasonPlanRevisionRequested, "release_cause": core.WorkOrderReleaseCauseOperatorAction, "outcome": core.WorkOrderOutcomeReleased, "automatic_retry_count": o.AutomaticRetryCount, "retry_suppressed": true}), At: now}, {TaskID: t.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": t.State, "to": nextTask, "command": core.TaskGatePlanRevision}), At: now}} {
			if err = taskEvent(eventCtx, tx, e); err != nil {
				return err
			}
		}
		t.State = nextTask
		o, err = getOrderRow(ctx, tx, id)
		result = store.PlanRevisionRequestResult{WorkOrder: o, Task: t, PlanVersion: version, Rationale: rationale}
		return err
	})
	return result, err
}
func (s *Store) CancelPlanRevisionWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id, attempt string) (core.WorkOrder, error) {
	var result core.WorkOrder
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		if !lease.ValidForCommand(o.TaskID, string(core.WorkOrderCmdCancel)) {
			return fmt.Errorf("plan-revision cancellation requires a valid taskops lease")
		}
		if o.Stage != core.StageImplement || o.LastAttemptID != attempt || o.LastFailureMessage != core.WorkOrderReleaseReasonPlanRevisionRequested {
			return fmt.Errorf("work order %s is not the contested plan-revision attempt", id)
		}
		if o.State == core.WorkOrderCancelled {
			result = o
			return nil
		}
		prior := o.State
		next, err := core.TransitionWorkOrder(prior, core.WorkOrderCmdCancel)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		o.State = next
		o.UpdatedAt = now
		o.Claimable = false
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		j, err := scanJob(documentRow(ctx, tx, `SELECT `+jobColumns+` FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), o.JobID))
		if err != nil {
			return err
		}
		if j.State != core.JobDone {
			if _, err = writeRow(ctx, tx, rowWrite{table: "jobs", operation: "UPDATE", values: map[string]any{"state": core.JobFailed, "ended_at": now, "updated_at": now}, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": o.JobID}}); err != nil {
				return err
			}
		}
		result = o
		return taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.cancelled", Payload: core.JSONPayload(map[string]any{"work_order_id": id, "attempt_id": attempt, "prior_state": prior, "state": next, "command": core.WorkOrderCmdCancel, "reason": "plan revision approved"}), At: now})
	})
	return result, err
}
func equalJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
func (s *Store) PreemptWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, raw store.WorkOrderPreemptRequest) (store.WorkOrderPreemptResult, error) {
	var result store.WorkOrderPreemptResult
	r, err := store.PrepareWorkOrderPreemptRequest(raw)
	if err != nil {
		return result, err
	}
	var lifecycleErr error
	err = s.orderTx(ctx, r.WorkOrderID, func(tx *sql.Tx, o core.WorkOrder) error {
		ws := documentWorkspace(ctx)
		if err := lockKey(ctx, tx, "work-order-preempt:"+r.RequestID); err != nil {
			return err
		}
		identity := core.JSONPayload(r)
		var sw string
		var requestData, resultData []byte
		err := tx.QueryRowContext(ctx, `SELECT workspace_id,request_json,result_json FROM work_order_preemptions WHERE request_id=?`, r.RequestID).Scan(&sw, &requestData, &resultData)
		if err == nil {
			if sw != ws || !equalJSON(identity, requestData) {
				return fmt.Errorf("%w: request_id was already used for different inputs", store.ErrWorkOrderPreemptConflict)
			}
			return json.Unmarshal(resultData, &result)
		}
		if err != sql.ErrNoRows {
			return err
		}
		if !lease.ValidForCommand(o.TaskID, string(core.WorkOrderCmdPreempt)) {
			return fmt.Errorf("work-order preempt requires a valid taskops lease")
		}
		now := time.Now().UTC()
		conflict := fmt.Errorf("%w: work order %s does not have an active claimed attempt", store.ErrWorkOrderPreemptConflict, o.ID)
		if o.State == core.WorkOrderClaimed && !o.ExecutionDeadline.IsZero() && !o.ExecutionDeadline.After(now) {
			if _, err = s.transitionWorkOrderTx(ctx, tx, o, core.WorkOrderCmdTimeout, "work_order.timed_out", now); err != nil {
				return err
			}
			lifecycleErr = conflict
			return nil
		}
		if o.State == core.WorkOrderClaimed && !o.LeaseExpiresAt.After(now) {
			if _, err = s.expireWorkOrderClaimTx(ctx, tx, o, now); err != nil {
				return err
			}
			lifecycleErr = conflict
			return nil
		}
		retired := o.State == core.WorkOrderQueued || o.State == core.WorkOrderStale
		if !retired && (o.State != core.WorkOrderClaimed || o.SessionID == "" || o.AttemptID == "") {
			return conflict
		}
		if _, err = core.TransitionWorkOrder(o.State, core.WorkOrderCmdPreempt); err != nil {
			return err
		}
		prior := o.State
		last := o.AttemptID
		if last == "" {
			last = o.LastAttemptID
		}
		result.RequestID = r.RequestID
		if !retired {
			result.RevokedAttemptID = o.AttemptID
			result.RevokedSessionID = o.SessionID
			result.RevokedWorkerID = o.WorkerID
			result.GraceBound = "one renewal interval"
		}
		clearOrderClaim(&o)
		o.State = core.WorkOrderQueued
		o.LastAttemptID = last
		o.LastAttemptOutcome = core.WorkOrderOutcomePreempted
		o.RetrySuppressed = false
		o.RetrySuppressionReason = ""
		if retired {
			o.State = core.WorkOrderCancelled
			o.LastAttemptOutcome = core.WorkOrderOutcomeCancelled
			o.RetrySuppressed = true
			o.RetrySuppressionReason = "operator retirement"
		}
		o.ExecutionStartedAt = time.Time{}
		o.ExecutionDeadline = time.Time{}
		o.LastFailureCategory = ""
		o.LastFailureMessage = ""
		o.LastFailureDetail = ""
		o.LastFailureExitStatus = nil
		o.LastFailureAt = time.Time{}
		o.NextRetryAt = time.Time{}
		o.UpdatedAt = now
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		o, err = getOrderRow(ctx, tx, o.ID)
		if err != nil {
			return err
		}
		o.Claimable = o.ClaimableAt(now)
		result.WorkOrder = o
		values := map[string]any{"state": core.JobPending, "started_at": nil, "ended_at": nil, "updated_at": now}
		if retired {
			values["state"] = core.JobFailed
			values["ended_at"] = now
			delete(values, "started_at")
		}
		if _, err = writeRow(ctx, tx, rowWrite{table: "jobs", operation: "UPDATE", values: values, where: map[string]any{"workspace_id": ws, "id": o.JobID}}); err != nil {
			return err
		}
		kind := "work_order.preempted"
		if retired {
			kind = "work_order.retired"
		}
		actor := store.ActorFromContext(ctx)
		if err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: kind, At: now, Payload: core.JSONPayload(map[string]any{"work_order_id": o.ID, "request_id": r.RequestID, "reason": r.Reason, "attempt_id": result.RevokedAttemptID, "session_id": result.RevokedSessionID, "worker_id": result.RevokedWorkerID, "prior_state": prior, "new_state": o.State, "command": core.WorkOrderCmdPreempt, "queue_entered_at": o.QueueEnteredAt, "queue_deadline": o.QueueDeadline, "grace_bound": result.GraceBound})}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO work_order_preemptions(workspace_id,request_id,work_order_id,request_json,result_json,revoked_attempt_id,revoked_session_id,revoked_worker_id,actor_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, ws, r.RequestID, o.ID, identity, core.JSONPayload(result), result.RevokedAttemptID, result.RevokedSessionID, result.RevokedWorkerID, actor.ID, now)
		return err
	})
	if err == nil {
		err = lifecycleErr
	}
	return result, err
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
	err := s.taskTx(ctx, request.TaskID, func(tx *sql.Tx) error {
		workspaceID := documentWorkspace(ctx)
		before, err := getTaskRow(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}
		orders, err := s.listOrders(ctx, tx, []string{request.TaskID})
		if err != nil {
			return err
		}
		for _, o := range orders {
			if o.Stage == core.StageImplement && o.ReasonCode == "merge-conflict" && core.WorkOrderActiveForConflictDispatch(o) {
				result.WorkOrder = o
			}
		}

		if result.WorkOrder.ID != "" {
			return nil
		}
		events, err := documentTaskEvents(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}

		if store.InterruptedReviewRecoveryNeeded(before, store.CurrentReviewOrders(orders, events), events) != nil {
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
		if err = insertInterventionTx(ctx, tx, intervention); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, Kind: "intervention.redirect", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"reason_code": intervention.ReasonCode, "comment": intervention.Comment}), At: intervention.At}); err != nil {
			return err
		}
		if err = taskWrite(ctx, tx, request.TaskID, map[string]any{"state": state, "next_stage": core.StageImplement, "recovery_stage": ""}); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state, "command": transitionCommand})}); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": before.NextStage, "next_stage": core.StageImplement, "recovery_stage": "", "state": state})}); err != nil {
			return err
		}
		if err = insertJobRow(ctx, tx, request.Job); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, JobID: request.Job.ID, Kind: "job.created", Payload: core.JSONPayload(request.Job)}); err != nil {
			return err
		}
		order := orderDefaults(request.WorkOrder)
		order.State = core.WorkOrderQueued
		if err = s.insertOrderTx(ctx, tx, order, false); err != nil {
			return err
		}

		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, JobID: request.Job.ID, Kind: "pipeline.awaiting_work_order", Payload: core.JSONPayload(map[string]any{"stage": core.StageImplement, "execution": "mcp", "reason_code": "merge-conflict"})}); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: request.TaskID, JobID: request.Job.ID, Kind: "merge.conflict_fix_dispatched", Payload: core.JSONPayload(map[string]any{"workspace": workspaceID, "task_id": request.TaskID, "reason_code": "merge-conflict", "approved_head": request.ApprovedHead, "new_head": request.NewHead, "work_order_id": order.ID})}); err != nil {
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

func projectOrders(orders []core.WorkOrder) {
	now := time.Now().UTC()
	for i := range orders {
		orders[i] = store.ProjectWorkOrderAt(orders[i], now)
	}
}
