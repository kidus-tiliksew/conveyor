package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error) {
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := q.GetTask(ctx, db.GetTaskParams{ID: spec.TaskID, WorkspaceID: workspace(ctx)}); err != nil {
			return notFound(err, "task %s", spec.TaskID)
		}
		row, err := q.InsertSpecVersion(ctx, db.InsertSpecVersionParams{
			TaskID: spec.TaskID, Content: spec.Content, AcceptanceCount: int32(spec.AcceptanceCount),
			Acceptance: spec.Acceptance, Decomposition: spec.Decomposition, CreatedAt: timestamp(spec.CreatedAt), Agent: spec.Agent, Model: spec.Model,
		})
		if err != nil {
			return err
		}
		spec = specFromDB(row)
		return insertEvent(ctx, q, core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount})})
	})
	if err != nil {
		return core.SpecVersion{}, err
	}
	return spec, nil
}

func (s *Store) GetLatestSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error) {
	row, err := s.queries.GetLatestSpecVersion(ctx, db.GetLatestSpecVersionParams{TaskID: taskID, WorkspaceID: workspace(ctx)})
	if errors.Is(err, pgx.ErrNoRows) {
		return core.SpecVersion{}, false, nil
	}
	if err != nil {
		return core.SpecVersion{}, false, err
	}
	spec := specFromDB(row)
	spec.LegacyGate, err = s.legacySpecGateVersion(ctx, taskID, spec.Version)
	return spec, true, err
}

func (s *Store) GetSpecVersion(ctx context.Context, taskID string, version int) (core.SpecVersion, bool, error) {
	var row db.TaskSpec
	err := s.boundary.QueryRow(ctx, `SELECT s.task_id,s.version,s.content,s.acceptance_count,
	s.acceptance,s.decomposition,s.approved,s.created_at,s.approved_at,s.agent,s.model
FROM task_specs s
JOIN tasks t ON t.id = s.task_id
WHERE s.task_id = $1 AND s.version = $2 AND t.workspace_id = $3`, taskID, version, workspace(ctx)).Scan(
		&row.TaskID, &row.Version, &row.Content, &row.AcceptanceCount, &row.Acceptance,
		&row.Decomposition, &row.Approved, &row.CreatedAt, &row.ApprovedAt, &row.Agent, &row.Model,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.SpecVersion{}, false, nil
	}
	if err != nil {
		return core.SpecVersion{}, false, err
	}
	spec := specFromDB(row)
	spec.LegacyGate, err = s.legacySpecGateVersion(ctx, taskID, spec.Version)
	return spec, true, err
}

// GetApprovedSpecVersion returns the newest approved spec version, which is
// the one that governs: approval only ever lands on the newest
// version, so a later unapproved draft is a proposal that has materialized
// nothing and must not displace the blueprint currently in delivery. The
// query is hand-written rather than generated because sqlc cannot parse this
// schema's migration set.
func (s *Store) GetApprovedSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error) {
	var row db.TaskSpec
	err := s.boundary.QueryRow(ctx, `SELECT s.task_id,s.version,s.content,s.acceptance_count,
	s.acceptance,s.decomposition,s.approved,s.created_at,s.approved_at,s.agent,s.model
FROM task_specs s
JOIN tasks t ON t.id = s.task_id
WHERE s.task_id = $1 AND t.workspace_id = $2 AND s.approved
ORDER BY s.version DESC
LIMIT 1`, taskID, workspace(ctx)).Scan(&row.TaskID, &row.Version, &row.Content,
		&row.AcceptanceCount, &row.Acceptance, &row.Decomposition, &row.Approved,
		&row.CreatedAt, &row.ApprovedAt, &row.Agent, &row.Model)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.SpecVersion{}, false, nil
	}
	if err != nil {
		return core.SpecVersion{}, false, err
	}
	spec := specFromDB(row)
	spec.LegacyGate, err = s.legacySpecGateVersion(ctx, taskID, spec.Version)
	return spec, true, err
}

func (s *Store) legacySpecGateVersion(ctx context.Context, taskID string, version int) (bool, error) {
	var exists bool
	err := s.boundary.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM legacy_spec_gate_versions
		WHERE workspace_id=$1 AND task_id=$2 AND spec_version=$3
	)`, workspace(ctx), taskID, version).Scan(&exists)
	return exists, err
}

func (s *Store) ApproveSpecVersion(ctx context.Context, taskID string, version int) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := q.ApproveLatestSpecVersion(ctx, db.ApproveLatestSpecVersionParams{
			TaskID: taskID, Version: int32(version), WorkspaceID: workspace(ctx),
		}); err != nil {
			return err
		}
		return insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})})
	})
}

func (s *Store) ListWorkOrdersForTasks(ctx context.Context, taskIDs []string) ([]core.WorkOrder, error) {
	if len(taskIDs) == 0 {
		return []core.WorkOrder{}, nil
	}
	return s.listWorkOrdersForTasks(ctx, taskIDs)
}

const workOrderColumns = `id, task_id, job_id, stage, state, claimant_id,
session_id, attempt_id, client_token_hash, agent, model, worker_id, lease_expires_at,
				review_round, review_seat, required_model, required_harness, required_effort, required_harness_config, execution_timeout, model_enforcement,
				reason_code, review_kind, review_scope, baseline_sha, head_sha, verification_context_id,
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

func (s *Store) CreateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder) error {
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdCreate)) {
		return fmt.Errorf("work-order create requires a valid taskops lease")
	}
	if order.CreatedAt.IsZero() {
		order.CreatedAt = time.Now().UTC()
	}
	if order.QueueEnteredAt.IsZero() {
		order.QueueEnteredAt = order.CreatedAt
	}
	if order.QueueDeadline.IsZero() {
		order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
	}
	if order.State == "" {
		var err error
		order.State, err = core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
		if err != nil {
			return err
		}
	} else if expected, err := core.TransitionWorkOrder("", core.WorkOrderCmdCreate); err != nil || order.State != expected {
		return &core.ErrInvalidTransition{Space: core.WorkOrderLifecycle, From: "", Command: string(core.WorkOrderCmdCreate), Allowed: []core.TransitionAlternative{{Command: string(core.WorkOrderCmdCreate), To: string(expected)}}}
	}
	order.Claimable = order.ClaimableAt(time.Now().UTC())
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), order.TaskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:work-order-create:"+workspace(ctx)+":"+order.TaskID); err != nil {
			return err
		}
		if order.Stage != core.StageReview {
			var activeSiblingID string
			err := tx.QueryRow(ctx, `SELECT id FROM work_orders
				WHERE workspace_id=$1 AND task_id=$2 AND stage=$3 AND state='claimed'
				ORDER BY created_at,id LIMIT 1`, workspace(ctx), order.TaskID, order.Stage).Scan(&activeSiblingID)
			if err == nil {
				return fmt.Errorf("work order %s cannot be created while same-stage order %s is actively claimed", order.ID, activeSiblingID)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var linked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM tasks t
			JOIN jobs j ON j.task_id=t.id
			WHERE t.workspace_id=$1 AND t.id=$2 AND j.id=$3 AND j.stage=$4
		)`, workspace(ctx), order.TaskID, order.JobID, order.Stage).Scan(&linked); err != nil {
			return err
		}
		if !linked {
			return fmt.Errorf("work order task %s and job %s are not linked in workspace %s", order.TaskID, order.JobID, workspace(ctx))
		}
		if order.Stage == core.StageImplement || order.Stage == core.StageVerify {
			var blocked bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM task_dependencies edge
				JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
					AND dependency.id=edge.depends_on_task_id
				WHERE edge.workspace_id=$1 AND edge.task_id=$2 AND dependency.state<>'merged'
			)`, workspace(ctx), order.TaskID).Scan(&blocked); err != nil {
				return err
			}
			if blocked {
				order.QueueBlockedAt, order.Claimable = order.QueueEnteredAt, false
			}
			if err := repinTaskDesignContextTx(ctx, tx, q, workspace(ctx), order.TaskID, order.QueueEnteredAt); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO work_orders (
			id, workspace_id, task_id, job_id, stage, state, claimant_id,
			session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
				review_round, review_seat, required_model, required_harness, required_harness_config, execution_timeout, model_enforcement,
				reason_code, review_kind, review_scope, baseline_sha, head_sha,
			queue_entered_at, queue_deadline, queue_blocked_at, execution_started_at, execution_deadline,
			last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
			automatic_retry_count, next_retry_at, retry_suppressed,
			redispatch_count, progress, cost_usd, tokens_in, tokens_out,
			usage_reported, self_reported, created_at, updated_at, served_requirement_snapshot, governance_snapshot
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,false,false,$43,$43,$44,$45)`,
			order.ID, workspace(ctx), order.TaskID, order.JobID, order.Stage, order.State,
			order.ClaimantID, order.SessionID, order.ClientTokenHash, order.Agent, order.Model, order.WorkerID,
			nullableTimeValue(order.LeaseExpiresAt), order.ReviewRound, order.ReviewSeat,
			order.RequiredModel, order.RequiredHarness, harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText, order.ModelEnforcement,
			order.ReasonCode, order.ReviewKind, order.ReviewScope, order.BaselineSHA, order.HeadSHA,
			order.QueueEnteredAt, order.QueueDeadline, nullableTimeValue(order.QueueBlockedAt),
			nullableTimeValue(order.ExecutionStartedAt), nullableTimeValue(order.ExecutionDeadline),
			order.LastAttemptOutcome, order.LastFailureMessage, order.LastFailureExitStatus, nullableTimeValue(order.LastFailureAt),
			order.AutomaticRetryCount, nullableTimeValue(order.NextRetryAt), order.RetrySuppressed,
			order.RedispatchCount, order.Progress, order.CostUSD, order.TokensIn,
			order.TokensOut, order.CreatedAt, servedRequirementSnapshotJSON(order.ServedRequirementSnapshot), governanceSnapshotJSON(order.GovernanceSnapshot))
		if err != nil {
			return err
		}
		if err := firstOrderDirectionTx(ctx, tx, &order); err != nil {
			return err
		}
		if eventErr := insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.created", Payload: core.JSONPayload(order)}); eventErr != nil {
			return eventErr
		}
		return retireWorkOrderSiblingsTx(ctx, tx, q, workspace(ctx), order, "superseded by successor creation", time.Now().UTC(), true)
	})
}

func (s *Store) CreateReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	if !lease.ValidForCommand(taskID, string(core.WorkOrderCmdCreate)) {
		return fmt.Errorf("review-round create requires a valid taskops lease")
	}
	if len(jobs) == 0 || len(jobs) != len(orders) {
		return fmt.Errorf("review round requires one job per work order")
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:review-round-create:"+workspace(ctx)+":"+taskID); err != nil {
			return err
		}
		taskRow, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", taskID)
		}
		task := taskFromDB(taskRow)
		if task.SetupContract.VerifyStage {
			var ready bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='verify' AND state='completed' AND head_sha=$3 AND head_sha<>'' AND verification_context_id<>'')`, workspace(ctx), taskID, core.VerifyStageHead(task)).Scan(&ready); err != nil {
				return err
			}
			if !ready {
				return fmt.Errorf("review requires completed verification for the submitted head")
			}
		}
		var existing int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review' AND review_round=$3`, workspace(ctx), taskID, orders[0].ReviewRound).Scan(&existing); err != nil {
			return err
		}
		if existing == len(orders) {
			return nil
		}
		if existing != 0 {
			return fmt.Errorf("review round %d is only partially persisted", orders[0].ReviewRound)
		}
		for i, job := range jobs {
			if job.TaskID != taskID || job.Stage != core.StageReview || orders[i].TaskID != taskID || orders[i].JobID != job.ID || orders[i].ReviewRound != orders[0].ReviewRound {
				return fmt.Errorf("invalid review round member %d", i)
			}
		}
		now := time.Now().UTC()
		for i, job := range jobs {
			if _, err = q.InsertJob(ctx, jobInsertParams(job)); err != nil {
				return err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: taskID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job)}); err != nil {
				return err
			}
			order := orders[i]
			if order.CreatedAt.IsZero() {
				order.CreatedAt = now
			}
			if order.QueueEnteredAt.IsZero() {
				order.QueueEnteredAt = order.CreatedAt
			}
			if order.QueueDeadline.IsZero() {
				order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
			}
			state, transitionErr := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
			if transitionErr != nil {
				return transitionErr
			}
			order.State, order.Claimable = state, true
			_, err = tx.Exec(ctx, `INSERT INTO work_orders (
				id, workspace_id, task_id, job_id, stage, state, claimant_id,
				session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
				review_round, review_seat, required_model, required_harness, required_harness_config, execution_timeout, model_enforcement,
				reason_code, review_kind, review_scope, baseline_sha, head_sha,
				queue_entered_at, queue_deadline, execution_started_at, execution_deadline,
				last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
				automatic_retry_count, next_retry_at, retry_suppressed,
				redispatch_count, progress, cost_usd, tokens_in, tokens_out,
				usage_reported, self_reported, created_at, updated_at, served_requirement_snapshot, governance_snapshot
			) VALUES ($1,$2,$3,$4,$5,$6,'','','','','','',NULL,$7,$8,$9,$10,$11,$12,'',$13,$14,$15,$16,$17,$18,$19,NULL,NULL,'','',NULL,NULL,0,NULL,false,0,'',0,0,0,false,false,$20,$20,$21,$22)`,
				order.ID, workspace(ctx), taskID, job.ID, core.StageReview, order.State,
				order.ReviewRound, order.ReviewSeat, order.RequiredModel, order.RequiredHarness,
				harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText,
				order.ReasonCode, order.ReviewKind, order.ReviewScope, order.BaselineSHA, order.HeadSHA,
				order.QueueEnteredAt, order.QueueDeadline, order.CreatedAt, servedRequirementSnapshotJSON(order.ServedRequirementSnapshot), governanceSnapshotJSON(order.GovernanceSnapshot))
			if err != nil {
				return err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: taskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order)}); err != nil {
				return err
			}
		}
		return insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "review.round_created", Payload: core.JSONPayload(map[string]any{"review_round": orders[0].ReviewRound, "seat_count": len(orders)})})
	})
}

func (s *Store) CreateStageWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, job core.Job, order core.WorkOrder) (bool, error) {
	if !lease.ValidForCommand(job.TaskID, string(core.WorkOrderCmdCreate)) {
		return false, fmt.Errorf("stage work-order create requires a valid taskops lease")
	}
	if !core.ValidWorkOrderStage(order.Stage) || job.Stage == core.StageReview || order.Stage != job.Stage || order.TaskID != job.TaskID || order.JobID != job.ID || order.ID != job.ID {
		return false, fmt.Errorf("invalid stage work order %s", order.ID)
	}
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		workspaceID := workspace(ctx)
		if err := lockWorkOrderTaskTx(ctx, tx, workspaceID, job.TaskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:stage-order:"+workspaceID+":"+job.TaskID); err != nil {
			return err
		}
		taskRow, err := q.GetTask(ctx, db.GetTaskParams{ID: job.TaskID, WorkspaceID: workspaceID})
		if err != nil {
			return notFound(err, "task %s", job.TaskID)
		}
		if err := core.ValidateVerifyDispatch(taskFromDB(taskRow), order); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM work_orders
			WHERE workspace_id=$1 AND task_id=$2 AND stage=$3 AND state IN ('queued','claimed')
		)`, workspaceID, job.TaskID, order.Stage).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		if _, err := q.InsertJob(ctx, jobInsertParams(job)); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: job.TaskID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job)}); err != nil {
			return err
		}
		now := time.Now().UTC()
		if order.CreatedAt.IsZero() {
			order.CreatedAt = now
		}
		if order.QueueEnteredAt.IsZero() {
			order.QueueEnteredAt = order.CreatedAt
		}
		if order.QueueDeadline.IsZero() {
			order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
		}
		state, err := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
		if err != nil {
			return err
		}
		order.State, order.Claimable = state, true
		if order.Stage == core.StageImplement || order.Stage == core.StageVerify {
			var blocked bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM task_dependencies edge
				JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
					AND dependency.id=edge.depends_on_task_id
				WHERE edge.workspace_id=$1 AND edge.task_id=$2 AND dependency.state<>'merged'
			)`, workspaceID, order.TaskID).Scan(&blocked); err != nil {
				return err
			}
			if blocked {
				order.QueueBlockedAt, order.Claimable = order.QueueEnteredAt, false
			}
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
		) VALUES ($1,$2,$3,$4,$5,$6,'','','','','','',NULL,0,0,$7,$8,$9,$10,'',$11,'','',$12,$19,$13,$14,$15,NULL,NULL,'','',NULL,NULL,0,NULL,false,0,'',0,0,0,false,false,$16,$16,$17,$18)`,
			order.ID, workspaceID, job.TaskID, job.ID, order.Stage, order.State,
			order.RequiredModel, order.RequiredHarness, harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText,
			order.ReasonCode, order.BaselineSHA, order.QueueEnteredAt, order.QueueDeadline,
			nullableTimeValue(order.QueueBlockedAt), order.CreatedAt, servedRequirementSnapshotJSON(order.ServedRequirementSnapshot), governanceSnapshotJSON(order.GovernanceSnapshot), order.HeadSHA)
		if err != nil {
			return err
		}
		if err := firstOrderDirectionTx(ctx, tx, &order); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: job.TaskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order)}); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

func (s *Store) RetryReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, request store.ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (store.ReviewRoundRetryResult, error) {
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("review retry requires a valid taskops lease")
	}
	request.RequestID = strings.TrimSpace(request.RequestID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.PRHead = strings.TrimSpace(request.PRHead)
	if request.RequestID == "" || request.Reason == "" || request.PRHead == "" {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("review retry request_id, reason, and verified PR head are required")
	}
	if len(jobs) == 0 || len(jobs) != len(orders) {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("review retry requires one job per work order")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	workspaceID := workspace(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:review-retry-request:"+request.RequestID); err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	var storedWorkspace, storedTask, storedReason, storedHead, storedActor string
	var storedPrior, storedNew int
	err = tx.QueryRow(ctx, `SELECT workspace_id,task_id,reason,prior_round,new_round,pr_head,actor_id FROM review_round_retries WHERE request_id=$1`, request.RequestID).Scan(&storedWorkspace, &storedTask, &storedReason, &storedPrior, &storedNew, &storedHead, &storedActor)
	if err == nil {
		if storedWorkspace != workspaceID || storedTask != request.TaskID || storedReason != request.Reason || storedPrior != request.PriorRound || storedHead != request.PRHead {
			return store.ReviewRoundRetryResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", store.ErrReviewRetryConflict, request.RequestID)
		}
		created, loadErr := reviewRoundOrdersTx(ctx, tx, workspaceID, request.TaskID, storedNew)
		if loadErr != nil {
			return store.ReviewRoundRetryResult{}, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return store.ReviewRoundRetryResult{}, err
		}
		return store.ReviewRoundRetryResult{RequestID: request.RequestID, TaskID: request.TaskID, PriorRound: storedPrior, NewRound: storedNew, PRHead: storedHead, WorkOrders: created}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.ReviewRoundRetryResult{}, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:review-retry-task:"+workspaceID+":"+request.TaskID); err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	q := s.queries.WithTx(tx)
	before, err := q.GetTask(ctx, db.GetTaskParams{ID: request.TaskID, WorkspaceID: workspaceID})
	if err != nil {
		return store.ReviewRoundRetryResult{}, notFound(err, "task %s", request.TaskID)
	}
	var latestRound, activeCount, recoverableCount int
	if err = tx.QueryRow(ctx, `WITH latest AS (
		SELECT COALESCE(max(review_round),0) AS review_round FROM work_orders
		WHERE workspace_id=$1 AND task_id=$2 AND stage='review'
	) SELECT latest.review_round,
		count(*) FILTER (WHERE state IN ('queued','claimed','submitted')),
		count(*) FILTER (WHERE state='timed_out' OR (state='completed' AND (last_attempt_outcome<>'' OR retry_suppressed OR last_failure_message<>'') AND (attempt_id='' OR last_attempt_id=attempt_id)))
	FROM latest LEFT JOIN work_orders ON workspace_id=$1 AND task_id=$2 AND stage='review' AND work_orders.review_round=latest.review_round
	GROUP BY latest.review_round`, workspaceID, request.TaskID).Scan(&latestRound, &activeCount, &recoverableCount); err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	var resolved bool
	if latestRound > 0 {
		err = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='review.round_completed'
			AND payload_json->>'review_round'=$3
		)`, workspaceID, request.TaskID, strconv.Itoa(latestRound)).Scan(&resolved)
		if err != nil {
			return store.ReviewRoundRetryResult{}, err
		}
	}
	if latestRound == 0 || latestRound != request.PriorRound || recoverableCount == 0 || activeCount != 0 || resolved {
		return store.ReviewRoundRetryResult{}, fmt.Errorf("%w: task %s has no matching recoverable non-progressing review round", store.ErrReviewRetryConflict, request.TaskID)
	}
	newRound := request.PriorRound + 1
	for i, job := range jobs {
		order := orders[i]
		if job.TaskID != request.TaskID || job.Stage != core.StageReview || order.TaskID != request.TaskID || order.JobID != job.ID || order.Stage != core.StageReview || order.ReviewRound != newRound || order.ReviewSeat != i+1 {
			return store.ReviewRoundRetryResult{}, fmt.Errorf("invalid review retry member %d", i)
		}
	}
	now := time.Now().UTC()
	created := make([]core.WorkOrder, 0, len(orders))
	for i, job := range jobs {
		if _, err = q.InsertJob(ctx, jobInsertParams(job)); err != nil {
			return store.ReviewRoundRetryResult{}, err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job), At: now}); err != nil {
			return store.ReviewRoundRetryResult{}, err
		}
		order := orders[i]
		if order.CreatedAt.IsZero() {
			order.CreatedAt = now
		}
		if order.QueueEnteredAt.IsZero() {
			order.QueueEnteredAt = order.CreatedAt
		}
		if order.QueueDeadline.IsZero() {
			order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
		}
		state, transitionErr := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
		if transitionErr != nil {
			return store.ReviewRoundRetryResult{}, transitionErr
		}
		order.State, order.Claimable, order.UpdatedAt = state, true, now
		_, err = tx.Exec(ctx, `INSERT INTO work_orders (
			id, workspace_id, task_id, job_id, stage, state, claimant_id,
			session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
			review_round, review_seat, required_model, required_harness, required_harness_config, execution_timeout, model_enforcement,
			reason_code, review_kind, review_scope, baseline_sha, head_sha,
			queue_entered_at, queue_deadline, execution_started_at, execution_deadline,
			last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
			automatic_retry_count, next_retry_at, retry_suppressed,
			redispatch_count, progress, cost_usd, tokens_in, tokens_out,
			usage_reported, self_reported, created_at, updated_at, served_requirement_snapshot, governance_snapshot
		) VALUES ($1,$2,$3,$4,$5,$6,'','','','','','',NULL,$7,$8,$9,$10,$11,$12,'',$13,$14,$15,$16,$17,$18,$19,NULL,NULL,'','',NULL,NULL,0,NULL,false,0,'',0,0,0,false,false,$20,$20,$21,$22)`,
			order.ID, workspaceID, request.TaskID, job.ID, core.StageReview, core.WorkOrderQueued,
			order.ReviewRound, order.ReviewSeat, order.RequiredModel, order.RequiredHarness,
			harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText,
			order.ReasonCode, order.ReviewKind, order.ReviewScope, order.BaselineSHA, order.HeadSHA,
			order.QueueEnteredAt, order.QueueDeadline, order.CreatedAt, servedRequirementSnapshotJSON(order.ServedRequirementSnapshot), governanceSnapshotJSON(order.GovernanceSnapshot))
		if err != nil {
			return store.ReviewRoundRetryResult{}, err
		}
		created = append(created, order)
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order), At: now}); err != nil {
			return store.ReviewRoundRetryResult{}, err
		}
	}
	actor := store.ActorFromContext(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO review_round_retries (workspace_id,request_id,task_id,reason,prior_round,new_round,pr_head,actor_id,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, workspaceID, request.RequestID, request.TaskID, request.Reason, request.PriorRound, newRound, request.PRHead, actor.ID, now); err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	timedOutOrders, err := reviewRoundOrdersTx(ctx, tx, workspaceID, request.TaskID, request.PriorRound)
	if err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	var timedOutIDs []string
	var inconsistentIDs []string
	for _, order := range timedOutOrders {
		if order.State == core.WorkOrderTimedOut {
			timedOutIDs = append(timedOutIDs, order.ID)
		}
		if order.State == core.WorkOrderCompleted && (order.LastAttemptOutcome != "" || order.RetrySuppressed || order.LastFailureMessage != "") && (order.AttemptID == "" || order.LastAttemptID == order.AttemptID) {
			inconsistentIDs = append(inconsistentIDs, order.ID)
		}
	}
	retryTask := taskFromDB(before)
	payload := map[string]any{"request_id": request.RequestID, "workspace_id": workspaceID, "task_id": request.TaskID, "actor": actor.ID, "reason": request.Reason, "prior_round": request.PriorRound, "new_round": newRound, "pr_head": request.PRHead, "timed_out_work_order_ids": timedOutIDs, "inconsistent_work_order_ids": inconsistentIDs, "setup_name": retryTask.SetupName, "setup_contract": retryTask.SetupContract}
	if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "review.round_retried", Payload: core.JSONPayload(payload), At: now}); err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "review.round_created", Payload: core.JSONPayload(map[string]any{"review_round": newRound, "seat_count": len(created), "retry_request_id": request.RequestID}), At: now}); err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return store.ReviewRoundRetryResult{}, err
	}
	return store.ReviewRoundRetryResult{RequestID: request.RequestID, TaskID: request.TaskID, PriorRound: request.PriorRound, NewRound: newRound, PRHead: request.PRHead, WorkOrders: created}, nil
}

func (s *Store) RecoverInterruptedReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, request store.InterruptedReviewRecoveryRequest, queueTimeout time.Duration) (store.InterruptedReviewRecoveryResult, error) {
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdRecover)) {
		return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("interrupted review recovery requires a valid taskops lease")
	}
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.RequestID == "" || request.TaskID == "" || request.Round <= 0 || queueTimeout <= 0 {
		return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("interrupted review recovery requires task, request_id, round, and queue timeout")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck -- commit below owns the outcome
	workspaceID := workspace(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:interrupted-review-request:"+request.RequestID); err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	var priorWorkspace, priorTask string
	var priorRound int
	var priorJSON []byte
	err = tx.QueryRow(ctx, `SELECT workspace_id,task_id,review_round,result_json FROM interrupted_review_recoveries WHERE request_id=$1`, request.RequestID).Scan(&priorWorkspace, &priorTask, &priorRound, &priorJSON)
	if err == nil {
		if priorWorkspace != workspaceID || priorTask != request.TaskID || priorRound != request.Round {
			return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", store.ErrReviewRetryConflict, request.RequestID)
		}
		var prior store.InterruptedReviewRecoveryResult
		if err = json.Unmarshal(priorJSON, &prior); err != nil {
			return store.InterruptedReviewRecoveryResult{}, err
		}
		if prior.RecoveredOrders == nil {
			prior.RecoveredOrders = []core.WorkOrder{}
		}
		if prior.RetainedOrders == nil {
			prior.RetainedOrders = []core.WorkOrder{}
		}
		return prior, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:interrupted-review-task:"+workspaceID+":"+request.TaskID); err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	q := s.queries.WithTx(tx)
	taskRow, err := q.GetTask(ctx, db.GetTaskParams{ID: request.TaskID, WorkspaceID: workspaceID})
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, notFound(err, "task %s", request.TaskID)
	}
	recoveryTask := taskFromDB(taskRow)
	if core.TaskTerminal(recoveryTask.State) {
		return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: terminal task %s cannot recover review work", store.ErrReviewRetryConflict, request.TaskID)
	}
	eventRows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(request.TaskID), WorkspaceID: workspaceID})
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	events := make([]core.Event, len(eventRows))
	for i := range eventRows {
		events[i] = eventFromDB(eventRows[i])
	}
	var latest int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(review_round),0) FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review'`, workspaceID, request.TaskID).Scan(&latest); err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	if latest == 0 || latest != request.Round {
		return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: task %s has no matching latest review round", store.ErrReviewRetryConflict, request.TaskID)
	}
	rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review' AND review_round=$3 ORDER BY review_seat FOR UPDATE", workspaceID, request.TaskID, request.Round)
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	var roundOrders []core.WorkOrder
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			rows.Close()
			return store.InterruptedReviewRecoveryResult{}, scanErr
		}
		roundOrders = append(roundOrders, order)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return store.InterruptedReviewRecoveryResult{}, err
	}
	rows.Close()
	superseded, err := supersededReviewWorkOrdersTx(ctx, tx, workspaceID, request.TaskID)
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	currentOrders := roundOrders[:0]
	for _, order := range roundOrders {
		if !superseded[order.ID] {
			currentOrders = append(currentOrders, order)
		}
	}
	recovery := store.InterruptedReviewRecoveryNeeded(recoveryTask, currentOrders, events)
	if recovery == nil || recovery.ReviewRound != request.Round {
		return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: task %s has no recoverable interrupted review seats or has a conflicting active attempt", store.ErrReviewRetryConflict, request.TaskID)
	}
	now := time.Now().UTC()
	result := store.InterruptedReviewRecoveryResult{
		RequestID:       request.RequestID,
		TaskID:          request.TaskID,
		ReviewRound:     request.Round,
		RecoveredOrders: make([]core.WorkOrder, 0, len(recovery.EligibleOrders)),
		RetainedOrders:  append(make([]core.WorkOrder, 0, len(recovery.RetainedOrders)), recovery.RetainedOrders...),
	}
	actor := store.ActorFromContext(ctx)
	for _, eligible := range recovery.EligibleOrders {
		change := request.Refreezes[eligible.ID]
		if change == nil {
			continue
		}
		var priorJSON []byte
		if err = tx.QueryRow(ctx, `SELECT setup_contract FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, request.TaskID).Scan(&priorJSON); err != nil {
			return store.InterruptedReviewRecoveryResult{}, err
		}
		var priorContract config.ExecutionSetup
		_ = json.Unmarshal(priorJSON, &priorContract)
		if _, err = tx.Exec(ctx, `UPDATE tasks SET setup_contract=$1,updated_at=$2 WHERE workspace_id=$3 AND id=$4`, setupContractJSON(change.Setup), now, workspaceID, request.TaskID); err != nil {
			return store.InterruptedReviewRecoveryResult{}, err
		}
		recoveryTask.SetupContract = change.Setup
		if !reflect.DeepEqual(priorContract, change.Setup) {
			if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: eligible.JobID, Kind: "task.setup.refrozen", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"prior": priorContract, "new": change.Setup, "request_id": request.RequestID, "work_order_id": eligible.ID, "actor": actor.ID}), At: now}); err != nil {
				return store.InterruptedReviewRecoveryResult{}, err
			}
		}
		break
	}
	for _, eligible := range recovery.EligibleOrders {
		priorOutcome := eligible.LastAttemptOutcome
		eligible.LastAttemptOutcome = ""
		eligible.RetrySuppressed = false
		eligible.RetrySuppressionReason = ""
		eligible.AutomaticRetryCount = 0
		eligible.NextRetryAt = time.Time{}
		eligible.QueueEnteredAt, eligible.QueueDeadline = now, now.Add(queueTimeout)
		eligible.RedispatchCount++
		eligible.UpdatedAt, eligible.Claimable = now, true
		var command pgconn.CommandTag
		var updateErr error
		eligible.ClearExecutionPins()
		if change := request.Refreezes[eligible.ID]; change != nil {
			eligible.ExecutionTimeoutText = change.ExecutionTimeoutText
			command, updateErr = tx.Exec(ctx, `UPDATE work_orders SET last_attempt_outcome='',retry_suppressed=false,retry_suppression_reason='',automatic_retry_count=0,next_retry_at=NULL,queue_entered_at=$1,queue_deadline=$2,redispatch_count=redispatch_count+1,`+clearExecutionPinsSQL+`,execution_timeout=$3,updated_at=$1 WHERE workspace_id=$4 AND id=$5 AND state='queued' AND retry_suppressed=true AND session_id='' AND worker_id=''`, now, now.Add(queueTimeout), change.ExecutionTimeoutText, workspaceID, eligible.ID)
		} else {
			command, updateErr = tx.Exec(ctx, `UPDATE work_orders SET last_attempt_outcome='',retry_suppressed=false,retry_suppression_reason='',automatic_retry_count=0,next_retry_at=NULL,queue_entered_at=$1,queue_deadline=$2,redispatch_count=redispatch_count+1,`+clearExecutionPinsSQL+`,updated_at=$1 WHERE workspace_id=$3 AND id=$4 AND state='queued' AND retry_suppressed=true AND session_id='' AND worker_id=''`, now, now.Add(queueTimeout), workspaceID, eligible.ID)
		}
		if updateErr != nil {
			return store.InterruptedReviewRecoveryResult{}, updateErr
		}
		if command.RowsAffected() != 1 {
			return store.InterruptedReviewRecoveryResult{}, fmt.Errorf("%w: review seat %d changed concurrently", store.ErrReviewRetryConflict, eligible.ReviewSeat)
		}
		result.RecoveredOrders = append(result.RecoveredOrders, eligible)
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: eligible.JobID, Kind: "review.seat_recovered", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "review_round": request.Round, "review_seat": eligible.ReviewSeat, "work_order_id": eligible.ID, "request_id": request.RequestID, "prior_state": core.WorkOrderQueued, "prior_outcome": priorOutcome, "resulting_state": eligible.State, "outcome": "recovered", "setup_name": recoveryTask.SetupName, "setup_contract": recoveryTask.SetupContract}), At: now}); err != nil {
			return store.InterruptedReviewRecoveryResult{}, err
		}
	}
	for _, retained := range recovery.RetainedOrders {
		if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, JobID: retained.JobID, Kind: "review.seat_recovery_skipped", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "review_round": request.Round, "review_seat": retained.ReviewSeat, "work_order_id": retained.ID, "request_id": request.RequestID, "prior_state": retained.State, "resulting_state": retained.State, "outcome": "retained_completed", "setup_name": recoveryTask.SetupName, "setup_contract": recoveryTask.SetupContract}), At: now}); err != nil {
			return store.InterruptedReviewRecoveryResult{}, err
		}
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: request.TaskID, Kind: "review.round_recovered", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "review_round": request.Round, "request_id": request.RequestID, "actor": actor.ID, "recovered_seats": len(result.RecoveredOrders), "retained_completed_seats": len(result.RetainedOrders)}), At: now}); err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO interrupted_review_recoveries (workspace_id,request_id,task_id,review_round,actor_id,result_json,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, workspaceID, request.RequestID, request.TaskID, request.Round, actor.ID, resultJSON, now); err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return store.InterruptedReviewRecoveryResult{}, err
	}
	return result, nil
}

func reviewRoundOrdersTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID string, round int) ([]core.WorkOrder, error) {
	rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review' AND review_round=$3 ORDER BY review_seat", workspaceID, taskID, round)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.WorkOrder
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, order)
	}
	return result, rows.Err()
}

func (s *Store) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	order, err := scanWorkOrder(s.boundary.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2", workspace(ctx), id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	order = store.ProjectWorkOrderAt(order, time.Now().UTC())
	items := []core.WorkOrder{order}
	if err = s.hydrateWorkOrderAssignees(ctx, items); err != nil {
		return core.WorkOrder{}, err
	}
	return items[0], nil
}

func (s *Store) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	rows, err := s.boundary.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 ORDER BY created_at,id", workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]core.WorkOrder, 0)
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, store.ProjectWorkOrderAt(order, time.Now().UTC()))
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = s.hydrateWorkOrderAssignees(ctx, orders); err != nil {
		return nil, err
	}
	return orders, nil
}

func (s *Store) hydrateWorkOrderAssignees(ctx context.Context, orders []core.WorkOrder) error {
	if len(orders) == 0 {
		return nil
	}
	taskIDs := make([]string, 0, len(orders))
	byTask := make(map[string][]int, len(orders))
	for i := range orders {
		if _, ok := byTask[orders[i].TaskID]; !ok {
			taskIDs = append(taskIDs, orders[i].TaskID)
		}
		byTask[orders[i].TaskID] = append(byTask[orders[i].TaskID], i)
	}
	rows, err := s.boundary.Query(ctx, `SELECT t.id,u.id,u.email,u.display_name FROM tasks t
		JOIN workspace_role_bindings b ON b.workspace_id=t.workspace_id AND b.user_id=t.assignee_user_id
		JOIN users u ON u.id=b.user_id WHERE t.workspace_id=$1 AND t.id=ANY($2::text[])`, workspace(ctx), taskIDs)
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
		for _, index := range byTask[taskID] {
			copy := assignee
			orders[index].Assignee = &copy
		}
	}
	return rows.Err()
}

func (s *Store) ListCheckpointContextCandidates(ctx context.Context, requirementID string) ([]store.CheckpointContextCandidate, error) {
	rows, err := s.queries.ListCheckpointContextCandidates(ctx, db.ListCheckpointContextCandidatesParams{
		WorkspaceID: workspace(ctx), RequirementID: requirementID,
	})
	if err != nil {
		return nil, err
	}
	result := make([]store.CheckpointContextCandidate, len(rows))
	for i, row := range rows {
		result[i] = store.CheckpointContextCandidate{ID: row.ID, Title: row.Title, State: core.TaskState(row.State)}
	}
	return result, nil
}

// listWorkOrdersForTasks reads the orders of an explicit task set, or the whole
// workspace when the set is empty. It backs the activity-marker projection so a
// page-scoped caller pays for its page only.
func (s *Store) listWorkOrdersForTasks(ctx context.Context, taskIDs []string) ([]core.WorkOrder, error) {
	if len(taskIDs) == 0 {
		return s.ListWorkOrders(ctx)
	}
	rows, err := s.boundary.Query(ctx, "SELECT "+activityWorkOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=ANY($2::text[]) ORDER BY created_at,id", workspace(ctx), taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]core.WorkOrder, 0)
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, store.ProjectWorkOrderAt(order, time.Now().UTC()))
	}
	return orders, rows.Err()
}

func (s *Store) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	rows, err := s.boundary.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at,id", workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]core.WorkOrder, 0)
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, store.ProjectWorkOrderAt(order, time.Now().UTC()))
	}
	return orders, rows.Err()
}

func (s *Store) ListTaskWorkOrdersSnapshot(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	rows, err := s.boundary.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at,id", workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]core.WorkOrder, 0)
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, store.ProjectWorkOrderAt(order, time.Now().UTC()))
	}
	return orders, rows.Err()
}

func lockWorkOrderTaskTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:task-operation:"+workspaceID+":"+taskID); err != nil {
		return err
	}
	key := fmt.Sprintf("conveyor:work-order-claim:%s:%s", workspaceID, taskID)
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key)
	return err
}

func workOrderTaskIDTx(ctx context.Context, tx pgx.Tx, workspaceID, id string) (string, error) {
	var taskID string
	if err := tx.QueryRow(ctx, `SELECT task_id FROM work_orders WHERE workspace_id=$1 AND id=$2`, workspaceID, id).Scan(&taskID); err != nil {
		return "", notFound(err, "work order %s", id)
	}
	return taskID, nil
}

func (s *Store) ClaimWorkOrderCommand(ctx context.Context, lifecycleLease taskops.TaskLease, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	// Session and client-token independence spans the implementation order and
	// every seat in a review round. Serialize all claims for one task before
	// locking an individual order so concurrent seats cannot both pass the
	// sibling check against an uncommitted peer.
	if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
		return core.WorkOrder{}, err
	}
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	if !lifecycleLease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdClaim)) {
		return core.WorkOrder{}, fmt.Errorf("work-order claim requires a valid taskops lease")
	}
	if !core.ValidWorkOrderStage(order.Stage) {
		return core.WorkOrder{}, fmt.Errorf("invalid work-order stage %s", order.Stage)
	}
	taskRow, err := s.queries.WithTx(tx).GetTask(ctx, db.GetTaskParams{ID: order.TaskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return core.WorkOrder{}, err
	}
	if err := core.ValidateVerifyDispatch(taskFromDB(taskRow), order); err != nil {
		return core.WorkOrder{}, err
	}
	if order.Stage != core.StageReview {
		var activeSiblingID string
		activeErr := tx.QueryRow(ctx, `SELECT id FROM work_orders
			WHERE workspace_id=$1 AND task_id=$2 AND (stage=$3 OR ($3='verify' AND stage='implement') OR ($3='implement' AND stage='verify')) AND id<>$4 AND state='claimed'
			ORDER BY created_at,id LIMIT 1`, workspace(ctx), order.TaskID, order.Stage, order.ID).Scan(&activeSiblingID)
		if activeErr == nil {
			return core.WorkOrder{}, fmt.Errorf("work order %s cannot be claimed while same-stage order %s is actively claimed", order.ID, activeSiblingID)
		}
		if !errors.Is(activeErr, pgx.ErrNoRows) {
			return core.WorkOrder{}, activeErr
		}
	}
	var assigneeUserID pgtype.Text
	if err := tx.QueryRow(ctx, `SELECT assignee_user_id FROM tasks WHERE workspace_id=$1 AND id=$2`, workspace(ctx), order.TaskID).Scan(&assigneeUserID); err != nil {
		return core.WorkOrder{}, err
	}
	if claim.OwnerUserID == "" {
		if credential, ok := store.CredentialFromContext(ctx); ok {
			claim.OwnerUserID = credential.OwnerUserID
		}
	}
	if claim.WorkerID != "" {
		var workerOwner pgtype.Text
		var workerRevoked pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT owner_user_id,revoked_at FROM workers
			WHERE workspace_id=$1 AND id=$2
			FOR SHARE`, workspace(ctx), claim.WorkerID).Scan(&workerOwner, &workerRevoked); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return core.WorkOrder{}, err
		} else if err == nil {
			if workerRevoked.Valid {
				return core.WorkOrder{}, fmt.Errorf("%w: worker %s", store.ErrWorkerUnauthorized, claim.WorkerID)
			}
			// Worker ownership is durable enrollment state, never a client
			// assertion. Holding the worker, owner, and binding rows through claim
			// commit serializes against revocation and identity deactivation.
			claim.OwnerUserID = ""
			if workerOwner.Valid {
				claim.OwnerUserID = workerOwner.String
				var authorized int
				err := tx.QueryRow(ctx, `SELECT 1 FROM users u
					JOIN workspace_role_bindings b ON b.user_id=u.id AND b.workspace_id=$1
					WHERE u.id=$2 AND u.status='active'
					FOR SHARE OF u,b`, workspace(ctx), workerOwner.String).Scan(&authorized)
				if errors.Is(err, pgx.ErrNoRows) {
					return core.WorkOrder{}, fmt.Errorf("%w: worker %s owner has no live workspace binding", store.ErrWorkerUnauthorized, claim.WorkerID)
				}
				if err != nil {
					return core.WorkOrder{}, err
				}
			}
		}
	}
	if assigneeUserID.Valid && assigneeUserID.String != claim.OwnerUserID {
		return core.WorkOrder{}, fmt.Errorf("task %s is assigned to %s; only that assignee may claim its work orders", order.TaskID, assigneeUserID.String)
	}
	if order.Stage == core.StageReview || order.Stage == core.StageVerify {
		// The listing's helper, inside the claim's transaction
		// (req-260810-70ce2f AC-1.1; component-work-orders).
		blocking, blockingErr := claimBlockingProposalsTx(ctx, tx, workspace(ctx), order.TaskID)
		if blockingErr != nil {
			return core.WorkOrder{}, blockingErr
		}
		if len(blocking) > 0 {
			return core.WorkOrder{}, store.ClaimBlockingProposalError(order.TaskID, blocking[0])
		}
	}
	if order.Stage == core.StageReview {
		accepted, acceptedErr := reviewSeatAcceptedTx(ctx, tx, workspace(ctx), order.TaskID, order.ID)
		if acceptedErr != nil {
			return core.WorkOrder{}, acceptedErr
		}
		if accepted {
			return core.WorkOrder{}, fmt.Errorf("accepted review seat %s is terminal and cannot be claimed", id)
		}
	}
	now := time.Now().UTC()
	var blockingTaskIDs []string
	if order.Stage == core.StageImplement || order.Stage == core.StageVerify {
		blockingRows, blockingErr := tx.Query(ctx, `SELECT dependency.id
			FROM task_dependencies edge
			JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
				AND dependency.id=edge.depends_on_task_id
			WHERE edge.workspace_id=$1 AND edge.task_id=$2 AND dependency.state<>'merged'
			ORDER BY dependency.id`, workspace(ctx), order.TaskID)
		if blockingErr != nil {
			return core.WorkOrder{}, blockingErr
		}
		for blockingRows.Next() {
			var dependencyID string
			if err = blockingRows.Scan(&dependencyID); err != nil {
				blockingRows.Close()
				return core.WorkOrder{}, err
			}
			blockingTaskIDs = append(blockingTaskIDs, dependencyID)
		}
		if err = blockingRows.Err(); err != nil {
			blockingRows.Close()
			return core.WorkOrder{}, err
		}
		blockingRows.Close()
		if len(blockingTaskIDs) > 0 {
			return core.WorkOrder{}, fmt.Errorf("task %s is blocked by unmerged dependencies: %s", order.TaskID, strings.Join(blockingTaskIDs, ", "))
		}
		if !order.QueueBlockedAt.IsZero() {
			order.QueueDeadline = order.QueueDeadline.Add(now.Sub(order.QueueBlockedAt))
			if _, err = tx.Exec(ctx, `UPDATE work_orders SET queue_deadline=$1,queue_blocked_at=NULL,updated_at=$2
				WHERE workspace_id=$3 AND id=$4`, order.QueueDeadline, now, workspace(ctx), order.ID); err != nil {
				return core.WorkOrder{}, err
			}
			order.QueueBlockedAt = time.Time{}
		}
	}
	if (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed) &&
		!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now) {
		order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderCmdTimeout, "work_order.timed_out", now)
		if err != nil {
			return core.WorkOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
	}
	if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
		!order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
		order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderCmdMarkStale, "work_order.stale", now)
		if err != nil {
			return core.WorkOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderStale, id)
	}
	if order.State == core.WorkOrderStale {
		return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderStale, id)
	}
	if order.State == core.WorkOrderTimedOut {
		return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
	}
	if order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(now) {
		return core.WorkOrder{}, fmt.Errorf("work order %s is already claimed", id)
	}
	if order.State == core.WorkOrderClaimed {
		if _, err = s.expireWorkOrderClaimTx(ctx, tx, order, now); err != nil {
			return core.WorkOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return core.WorkOrder{}, fmt.Errorf("work order %s lease expired; operator recovery is required", id)
	}
	if order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not claimable", id)
	}
	if !order.ClaimableAt(now) {
		if order.RetrySuppressed {
			return core.WorkOrder{}, fmt.Errorf("work order %s automatic retry is suppressed; operator recovery is required", id)
		}
		return core.WorkOrder{}, fmt.Errorf("work order %s is in retry backoff until %s", id, order.NextRetryAt.Format(time.RFC3339Nano))
	}
	hash := ""
	if claim.ClientToken != "" {
		hash = fmt.Sprintf("%x", sha256.Sum256([]byte(claim.ClientToken)))
	}
	if order.Stage == core.StageReview {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_orders
			WHERE workspace_id=$1 AND task_id=$2 AND id<>$3
			AND (stage='implement' OR (stage='review' AND review_round=$4))
			AND (($5 <> '' AND session_id=$5) OR ($6 <> '' AND client_token_hash=$6)))
			OR EXISTS (SELECT 1 FROM events e JOIN tasks t ON t.id=e.task_id
			WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='work_order.claimed'
			AND e.payload_json->>'id'<>$3
			AND (e.payload_json->>'stage'='implement' OR (e.payload_json->>'stage'='review' AND COALESCE((e.payload_json->>'review_round')::integer,0)=$4))
			AND $5<>'' AND e.payload_json->>'session_id'=$5)`,
			workspace(ctx), order.TaskID, order.ID, order.ReviewRound, claim.SessionID, hash).Scan(&blocked); err != nil {
			return core.WorkOrder{}, err
		}
		if blocked {
			return core.WorkOrder{}, fmt.Errorf("self-review forbidden: review session independence requires a fresh session and client token")
		}
		if claim.WorkerID != "" {
			if order.RequiredModel != "" && claim.Model != order.RequiredModel {
				return core.WorkOrder{}, fmt.Errorf("worker review model %q does not match pinned seat model %q", claim.Model, order.RequiredModel)
			}
			order.ModelEnforcement = "worker-pinned"
		} else {
			order.ModelEnforcement = "self-reported"
		}
	}
	if !order.ExecutionStartedAt.IsZero() && order.ExecutionDeadline.IsZero() && claim.ExecutionTimeout > 0 {
		order.ExecutionDeadline = order.ExecutionStartedAt.Add(claim.ExecutionTimeout)
		if !order.ExecutionDeadline.After(now) {
			if _, err = tx.Exec(ctx, `UPDATE work_orders SET execution_deadline=$1 WHERE workspace_id=$2 AND id=$3`, order.ExecutionDeadline, workspace(ctx), id); err != nil {
				return core.WorkOrder{}, err
			}
			order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderCmdTimeout, "work_order.timed_out", now)
			if err != nil {
				return core.WorkOrder{}, err
			}
			if err = tx.Commit(ctx); err != nil {
				return core.WorkOrder{}, err
			}
			return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
		}
	}
	lease := claim.Lease
	if lease <= 0 {
		lease = core.DefaultWorkOrderClaimLease
	}
	if _, transitionErr := core.TransitionWorkOrder(order.State, core.WorkOrderCmdClaim); transitionErr != nil {
		return core.WorkOrder{}, transitionErr
	}
	expires := now.Add(lease)
	executionStarted, executionDeadline := order.ExecutionStartedAt, order.ExecutionDeadline
	if executionStarted.IsZero() {
		executionStarted = now
		if claim.ExecutionTimeout > 0 {
			executionDeadline = now.Add(claim.ExecutionTimeout)
		}
	}
	attemptID := core.NewWorkOrderAttemptID()
	row := tx.QueryRow(ctx, "UPDATE work_orders SET state='claimed', claimant_id=$1, session_id=$2, attempt_id=$3, client_token_hash=$4, agent=$5, model=$6, worker_id=$7, lease_expires_at=$8, execution_started_at=$9, execution_deadline=$10, model_enforcement=$11, updated_at=$12, served_requirement_snapshot=COALESCE(served_requirement_snapshot,$13), governance_snapshot=CASE WHEN $14::jsonb IS NULL THEN governance_snapshot WHEN governance_snapshot IS NULL THEN $14::jsonb ELSE jsonb_set(jsonb_set(governance_snapshot, '{pending_design_proposals}', COALESCE($14::jsonb->'pending_design_proposals','[]'::jsonb), true), '{resolution_notes}', COALESCE($14::jsonb->'resolution_notes','[]'::jsonb), true) END WHERE workspace_id=$15 AND id=$16 RETURNING "+workOrderColumns,
		claim.ClaimantID, claim.SessionID, attemptID, hash, claim.Agent, claim.Model, claim.WorkerID, expires,
		executionStarted, nullableTimeValue(executionDeadline), order.ModelEnforcement, now, servedRequirementSnapshotJSON(claim.Requirements), governanceSnapshotJSON(claim.Governance), workspace(ctx), id)
	order, err = scanWorkOrder(row)
	if err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state='running', model_tier=$1,
		started_at=COALESCE(started_at,$2), updated_at=$2 WHERE id=$3`, claim.Model, executionStarted, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	var taskState core.TaskState
	if err = tx.QueryRow(ctx, `SELECT state FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), order.TaskID).Scan(&taskState); err != nil {
		return core.WorkOrder{}, err
	}
	if taskState == core.TaskQueued {
		nextTaskState, transitionErr := core.TransitionTask(taskState, core.TaskOrderClaim)
		if transitionErr != nil {
			return core.WorkOrder{}, transitionErr
		}
		if _, err = tx.Exec(ctx, `UPDATE tasks SET state=$1,updated_at=$2 WHERE workspace_id=$3 AND id=$4 AND state=$5`, nextTaskState, now, workspace(ctx), order.TaskID, taskState); err != nil {
			return core.WorkOrder{}, err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": taskState, "to": nextTaskState, "command": core.TaskOrderClaim}), At: now}); err != nil {
			return core.WorkOrder{}, err
		}
	}
	if err := insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(order)}); err != nil {
		return core.WorkOrder{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func (s *Store) RedispatchWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, queueTimeout time.Duration) (core.WorkOrder, error) {
	if queueTimeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if err = lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
		return core.WorkOrder{}, err
	}
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdRedispatch)) {
		return core.WorkOrder{}, fmt.Errorf("work-order redispatch requires a valid taskops lease")
	}
	now := time.Now().UTC()
	if order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(now) {
		return core.WorkOrder{}, fmt.Errorf("work order %s has an active claim and cannot be redispatched", id)
	}
	if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
		!order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
		order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderCmdMarkStale, "work_order.stale", now)
		if err != nil {
			return core.WorkOrder{}, err
		}
	}
	if order.State != core.WorkOrderStale {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not stale and cannot be redispatched", id)
	}
	if !order.ExecutionStartedAt.IsZero() {
		return core.WorkOrder{}, fmt.Errorf("work order %s was already claimed and requires operator recovery", id)
	}
	if err = workOrderSupersessionGuardTx(ctx, tx, workspace(ctx), order); err != nil {
		return core.WorkOrder{}, err
	}
	if _, transitionErr := core.TransitionWorkOrder(order.State, core.WorkOrderCmdRedispatch); transitionErr != nil {
		return core.WorkOrder{}, transitionErr
	}
	row := tx.QueryRow(ctx, `UPDATE work_orders SET state='queued', claimant_id='',
		session_id='', client_token_hash='', agent='', model='', worker_id='', lease_expires_at=NULL, model_enforcement='',
		`+clearExecutionPinsSQL+`,
		queue_entered_at=$1, queue_deadline=$2, execution_started_at=NULL,
		execution_deadline=NULL, redispatch_count=redispatch_count+1, progress='', updated_at=$1
		WHERE workspace_id=$3 AND id=$4 RETURNING `+workOrderColumns,
		now, now.Add(queueTimeout), workspace(ctx), id)
	order, err = scanWorkOrder(row)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending', started_at=NULL,
		ended_at=NULL, updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if order.Stage == core.StageImplement {
		if err = repinTaskDesignContextTx(ctx, tx, q, workspace(ctx), order.TaskID, now); err != nil {
			return core.WorkOrder{}, err
		}
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.redispatched", Payload: core.JSONPayload(map[string]any{"work_order_id": id, "prior_state": core.WorkOrderStale, "new_state": order.State, "command": core.WorkOrderCmdRedispatch, "reason": "stale never-claimed queue redispatch"}), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if err = retireWorkOrderSiblingsTx(ctx, tx, q, workspace(ctx), order, "superseded by stale redispatch", now, false); err != nil {
		return core.WorkOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func reviewSeatAcceptedTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID, workOrderID string) (bool, error) {
	var accepted bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM events e
		JOIN tasks t ON t.id=e.task_id
		WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='review.accepted'
		AND e.payload_json->>'review_work_order_id'=$3
	)`, workspaceID, taskID, workOrderID).Scan(&accepted)
	return accepted, err
}

func (s *Store) RecoverWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id, requestID, direction string, queueTimeout time.Duration, refreeze ...*store.RecoveryRefreeze) (core.WorkOrder, error) {
	clean, sanitationErr := store.SanitizeVerificationRecovery(ctx, s)
	if sanitationErr != nil {
		return core.WorkOrder{}, sanitationErr
	}
	ctx = clean
	if err := store.AuthorizeVerificationRecovery(ctx, s); err != nil {
		return core.WorkOrder{}, err
	}
	var err error
	direction, err = core.NormalizeWorkOrderOperatorDirection(direction)
	if err != nil {
		return core.WorkOrder{}, err
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return core.WorkOrder{}, fmt.Errorf("recovery request_id is required")
	}
	if queueTimeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if err = lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
		return core.WorkOrder{}, err
	}
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdRecover)) {
		return core.WorkOrder{}, fmt.Errorf("work-order recovery requires a valid taskops lease")
	}
	if store.VerificationRecoveryFromContext(ctx) != nil {
		q := s.queries.WithTx(tx)
		records, e := q.ListVerificationRecords(ctx, workspace(ctx), taskID)
		if e != nil {
			return core.WorkOrder{}, e
		}
		task, e := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
		if e != nil {
			return core.WorkOrder{}, e
		}
		changes, events, e := store.PrepareVerificationRecovery(ctx, taskFromDB(task), order, verificationRows(records), requestID, time.Now().UTC())
		if e != nil {
			return core.WorkOrder{}, e
		}
		for _, r := range changes {
			if _, e = tx.Exec(ctx, "UPDATE "+r.Table+" SET state=$1,body=$2 WHERE workspace_id=$3 AND id=$4", r.State, r.Body, workspace(ctx), r.ID); e != nil {
				return core.WorkOrder{}, e
			}
		}
		for _, event := range events {
			if e = insertEvent(ctx, q, event); e != nil {
				return core.WorkOrder{}, e
			}
		}
		if e = store.VerificationFault(ctx, "recovery"); e != nil {
			return core.WorkOrder{}, e
		}
	}
	var duplicate bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_order_recoveries WHERE workspace_id=$1 AND work_order_id=$2 AND request_id=$3)`, workspace(ctx), id, requestID).Scan(&duplicate); err != nil {
		return core.WorkOrder{}, err
	}
	if duplicate {
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return order, nil
	}
	now := time.Now().UTC()
	if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
		!order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
		order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderCmdMarkStale, "work_order.stale", now)
		if err != nil {
			return core.WorkOrder{}, err
		}
	}
	eligibleQueued := order.State == core.WorkOrderQueued && (order.LastAttemptOutcome != "" || order.RetrySuppressed || !order.NextRetryAt.IsZero())
	if !eligibleQueued && order.State != core.WorkOrderStale && order.State != core.WorkOrderTimedOut {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not released, expired, or retry-suppressed", id)
	}
	if err = workOrderSupersessionGuardTx(ctx, tx, workspace(ctx), order); err != nil {
		return core.WorkOrder{}, err
	}
	prior := order.LastAttemptOutcome
	priorAttemptID := order.LastAttemptID
	priorState := order.State
	priorFailureCategory := order.LastFailureCategory
	priorNextRetryAt := order.NextRetryAt
	priorTransientFailures := 0
	if priorFailureCategory == core.WorkOrderFailureTransientConnectivity {
		if err = tx.QueryRow(ctx, `SELECT COALESCE((payload_json->>'consecutive_transient_failures')::integer, 0) FROM events WHERE workspace_id=$1 AND task_id=$2 AND job_id=$3 AND kind IN ('work_order.child_failed','work_order.stalled') ORDER BY at DESC, id DESC LIMIT 1`, workspace(ctx), order.TaskID, order.JobID).Scan(&priorTransientFailures); errors.Is(err, pgx.ErrNoRows) {
			priorTransientFailures = 0
		} else if err != nil {
			return core.WorkOrder{}, err
		}
	}
	lifecycleCommand := core.WorkOrderCmdRecover
	eventKind := "work_order.recovered"
	if priorState == core.WorkOrderQueued {
		// Resetting retry metadata on an already-queued order is not a lifecycle
		// transition. Keep the historical event kind without mislabeling this
		// operator action as W14.
		lifecycleCommand = ""
		eventKind = "work_order.redispatched"
	}
	if lifecycleCommand != "" {
		if _, transitionErr := core.TransitionWorkOrder(priorState, lifecycleCommand); transitionErr != nil {
			return core.WorkOrder{}, transitionErr
		}
	}
	if len(refreeze) != 0 && refreeze[0] != nil {
		change := refreeze[0]
		var priorJSON []byte
		if err = tx.QueryRow(ctx, `SELECT setup_contract FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), order.TaskID).Scan(&priorJSON); err != nil {
			return core.WorkOrder{}, err
		}
		var priorContract config.ExecutionSetup
		_ = json.Unmarshal(priorJSON, &priorContract)
		if _, err = tx.Exec(ctx, `UPDATE tasks SET setup_contract=$1,updated_at=$2 WHERE workspace_id=$3 AND id=$4`, setupContractJSON(change.Setup), now, workspace(ctx), order.TaskID); err != nil {
			return core.WorkOrder{}, err
		}
		order.ExecutionTimeoutText = change.ExecutionTimeoutText
		if !reflect.DeepEqual(priorContract, change.Setup) {
			actor := store.ActorFromContext(ctx)
			q := s.queries.WithTx(tx)
			if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "task.setup.refrozen", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"prior": priorContract, "new": change.Setup, "request_id": requestID, "work_order_id": order.ID, "actor": actor.ID}), At: now}); err != nil {
				return core.WorkOrder{}, err
			}
		}
	}
	order, err = scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state='queued',claimant_id='',session_id='',attempt_id='',client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',execution_started_at=NULL,execution_deadline=NULL,last_attempt_outcome='',retry_suppressed=false,retry_suppression_reason='',automatic_retry_count=0,next_retry_at=NULL,queue_entered_at=$1,queue_deadline=$2,redispatch_count=redispatch_count+1,operator_direction=$3,`+clearExecutionPinsSQL+`,execution_timeout=$4,updated_at=$1 WHERE workspace_id=$5 AND id=$6 AND state IN ('queued','stale','timed_out') RETURNING `+workOrderColumns, now, now.Add(queueTimeout), direction, order.ExecutionTimeoutText, workspace(ctx), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrder{}, fmt.Errorf("work order %s changed during recovery", id)
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO work_order_recoveries (workspace_id,work_order_id,request_id,created_at) VALUES ($1,$2,$3,$4)`, workspace(ctx), id, requestID, now); err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if order.Stage == core.StageImplement {
		if err = repinTaskDesignContextTx(ctx, tx, q, workspace(ctx), order.TaskID, now); err != nil {
			return core.WorkOrder{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: eventKind, Payload: core.JSONPayload(map[string]any{"attempt_id": priorAttemptID, "workspace_id": workspace(ctx), "work_order_id": id, "request_id": requestID, "prior_state": priorState, "prior_outcome": prior, "new_state": order.State, "command": lifecycleCommand, "reason": "operator recovery", "direction": direction, "failure_category": priorFailureCategory, "consecutive_transient_failures": priorTransientFailures, "next_retry_at": priorNextRetryAt}), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func (s *Store) ListElapsedWorkOrderTaskIDs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.boundary.Query(ctx, `SELECT DISTINCT task_id FROM work_orders
		WHERE workspace_id=$1 AND (
			(state IN ('queued','claimed') AND execution_deadline IS NOT NULL AND execution_deadline <= $2)
			OR (state='claimed' AND lease_expires_at IS NOT NULL AND lease_expires_at <= $2)
			OR (state='queued' AND execution_started_at IS NULL AND queue_blocked_at IS NULL AND queue_deadline <= $2)
		) ORDER BY task_id`, workspace(ctx), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var taskID string
		if err = rows.Scan(&taskID); err != nil {
			return nil, err
		}
		result = append(result, taskID)
	}
	return result, rows.Err()
}

func (s *Store) ApplyWorkOrderClock(ctx context.Context, lease taskops.TaskLease, taskID string, now time.Time) (int, error) {
	if !lease.ValidFor(taskID) {
		return 0, fmt.Errorf("work-order lifecycle mutation requires a valid taskops lease")
	}
	count := 0
	err := s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		key := "conveyor:task-operation:" + workspace(ctx) + ":" + taskID
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND state IN ('queued','claimed') FOR UPDATE", workspace(ctx), taskID)
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
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		var dependencyBlocked bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM task_dependencies edge
			JOIN tasks dependency ON dependency.workspace_id=edge.workspace_id
				AND dependency.id=edge.depends_on_task_id
			WHERE edge.workspace_id=$1 AND edge.task_id=$2 AND dependency.state<>'merged'
		)`, workspace(ctx), taskID).Scan(&dependencyBlocked); err != nil {
			return err
		}
		for _, order := range orders {
			if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && order.State == core.WorkOrderQueued {
				if dependencyBlocked {
					if order.QueueBlockedAt.IsZero() {
						if _, err = tx.Exec(ctx, `UPDATE work_orders SET queue_blocked_at=$1,updated_at=$1
							WHERE workspace_id=$2 AND id=$3 AND queue_blocked_at IS NULL`,
							now, workspace(ctx), order.ID); err != nil {
							return err
						}
					}
					continue
				}
				if !order.QueueBlockedAt.IsZero() {
					order.QueueDeadline = order.QueueDeadline.Add(now.Sub(order.QueueBlockedAt))
					if _, err = tx.Exec(ctx, `UPDATE work_orders
						SET queue_deadline=$1,queue_blocked_at=NULL,updated_at=$2
						WHERE workspace_id=$3 AND id=$4`,
						order.QueueDeadline, now, workspace(ctx), order.ID); err != nil {
						return err
					}
					order.QueueBlockedAt = time.Time{}
				}
			}
			if !order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now) {
				if _, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderCmdTimeout, "work_order.timed_out", now); err != nil {
					return err
				}
				count++
				continue
			}
			if order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.After(now) {
				if _, err = s.expireWorkOrderClaimTx(ctx, tx, order, now); err != nil {
					return err
				}
				count++
				continue
			}
			if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
				order.QueueBlockedAt.IsZero() && !order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
				if _, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderCmdMarkStale, "work_order.stale", now); err != nil {
					return err
				}
				count++
			}
		}
		return nil
	})
	return count, err
}

func (s *Store) expireWorkOrderClaimTx(ctx context.Context, tx pgx.Tx, order core.WorkOrder, now time.Time) (core.WorkOrder, error) {
	if _, transitionErr := core.TransitionWorkOrder(order.State, core.WorkOrderCmdExpire); transitionErr != nil {
		return core.WorkOrder{}, transitionErr
	}
	attemptID := order.AttemptID
	taskRunClaim := core.IsTaskRunClaimantID(order.ClaimantID)
	updated, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state='queued',claimant_id='',session_id='',attempt_id='',last_attempt_id=$1,client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',execution_started_at=NULL,execution_deadline=NULL,`+clearExecutionPinsSQL+`,last_attempt_outcome=$2,next_retry_at=NULL,retry_suppressed=$3,updated_at=$4 WHERE workspace_id=$5 AND id=$6 AND state='claimed' RETURNING `+workOrderColumns, attemptID, core.WorkOrderOutcomeExpired, !taskRunClaim, now, workspace(ctx), order.ID))
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if err = insertEvent(ctx, q, core.Event{TaskID: updated.TaskID, JobID: updated.JobID, Kind: "work_order.expired", Payload: core.JSONPayload(map[string]any{"attempt_id": attemptID, "outcome": updated.LastAttemptOutcome, "release_cause": core.WorkOrderReleaseCauseLeaseLoss, "retry_suppressed": updated.RetrySuppressed}), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	return updated, nil
}

func (s *Store) transitionWorkOrderTx(ctx context.Context, tx pgx.Tx, order core.WorkOrder, command core.WorkOrderCommand, kind string, now time.Time) (core.WorkOrder, error) {
	state, transitionErr := core.TransitionWorkOrder(order.State, command)
	if transitionErr != nil {
		return core.WorkOrder{}, transitionErr
	}
	attemptID := ""
	var row pgx.Row
	if state == core.WorkOrderTimedOut && order.State == core.WorkOrderClaimed {
		attemptID = order.AttemptID
		row = tx.QueryRow(ctx, `UPDATE work_orders SET state=$1,claimant_id='',session_id='',attempt_id='',last_attempt_id=$2,
			client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',
			updated_at=$3 WHERE workspace_id=$4 AND id=$5 RETURNING `+workOrderColumns,
			state, attemptID, now, workspace(ctx), order.ID)
	} else {
		row = tx.QueryRow(ctx, `UPDATE work_orders SET state=$1, lease_expires_at=NULL,
			updated_at=$2 WHERE workspace_id=$3 AND id=$4 RETURNING `+workOrderColumns,
			state, now, workspace(ctx), order.ID)
	}
	updated, err := scanWorkOrder(row)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if state == core.WorkOrderTimedOut {
		endedAt := now
		if !order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now) {
			endedAt = order.ExecutionDeadline
		}
		if _, err = tx.Exec(ctx, `UPDATE jobs SET state='failed', ended_at=COALESCE(ended_at,$1),
			updated_at=$2 WHERE id=$3`, endedAt, now, order.JobID); err != nil {
			return core.WorkOrder{}, err
		}
	}
	q := s.queries.WithTx(tx)
	eventOrder := updated
	eventOrder.AttemptID = attemptID
	if err = insertEvent(ctx, q, core.Event{TaskID: updated.TaskID, JobID: updated.JobID, Kind: kind, Payload: core.JSONPayload(eventOrder), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	return updated, nil
}

func (s *Store) UpdateWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, order core.WorkOrder, commands ...core.WorkOrderCommand) error {
	var lifecycleErr error
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var err error
		lifecycleErr, err = s.updateWorkOrderCommandTx(ctx, tx, q, lease, order, commands...)
		return err
	})
	if err != nil {
		return err
	}
	return lifecycleErr
}

func (s *Store) updateWorkOrderCommandTx(ctx context.Context, tx pgx.Tx, q *db.Queries, lease taskops.TaskLease, order core.WorkOrder, commands ...core.WorkOrderCommand) (lifecycleErr error, err error) {
	err = func() error {
		taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), order.ID)
		if err != nil {
			return err
		}
		if err = lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
			return err
		}
		current, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), order.ID))
		if err != nil {
			return notFound(err, "work order %s", order.ID)
		}
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
		if command == core.WorkOrderCmdSubmitForReview && order.Stage == core.StageImplement && order.HeadSHA != "" {
			if err := s.supersedeVerificationTx(ctx, tx, order.TaskID, order.HeadSHA); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET reviewed_head_sha=$1 WHERE workspace_id=$2 AND id=$3`, order.HeadSHA, workspace(ctx), order.TaskID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE work_orders SET head_sha=$1 WHERE workspace_id=$2 AND id=$3`, order.HeadSHA, workspace(ctx), order.ID); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE work_orders SET state=$1, claimant_id=$2, session_id=$3, attempt_id=$4,
			client_token_hash=$5, agent=$6, model=$7, lease_expires_at=$8,
			model_enforcement=$9, queue_entered_at=$10, queue_deadline=$11, execution_started_at=$12,
			execution_deadline=$13, last_attempt_id=$14, last_attempt_outcome=$15, last_failure_category=$16, last_failure_message=$17, last_failure_detail=$18,
			last_failure_exit_status=$19, last_failure_at=$20, automatic_retry_count=$21,
			next_retry_at=$22, retry_suppressed=$23, retry_suppression_reason=$24, redispatch_count=$25, progress=$26,
			cost_usd=$27, tokens_in=$28, tokens_out=$29, usage_reported=$30, self_reported=$31,
			operator_direction=$32, continuation_session_id=$33, continuation_attempt_id=$34,
			continuation_harness=$35, continuation_launch_environment=$36,
			rate_limit=$37, rate_limit_observed_at=$38, updated_at=now()
			WHERE workspace_id=$39 AND id=$40`, order.State, order.ClaimantID, order.SessionID, order.AttemptID,
			order.ClientTokenHash, order.Agent, order.Model, nullableTimeValue(order.LeaseExpiresAt),
			order.ModelEnforcement,
			order.QueueEnteredAt, order.QueueDeadline, nullableTimeValue(order.ExecutionStartedAt),
			nullableTimeValue(order.ExecutionDeadline), order.LastAttemptID, order.LastAttemptOutcome, order.LastFailureCategory, order.LastFailureMessage, order.LastFailureDetail,
			order.LastFailureExitStatus, nullableTimeValue(order.LastFailureAt), order.AutomaticRetryCount,
			nullableTimeValue(order.NextRetryAt), order.RetrySuppressed, order.RetrySuppressionReason, order.RedispatchCount, order.Progress,
			order.CostUSD, order.TokensIn, order.TokensOut, order.UsageReported, order.SelfReported,
			order.OperatorDirection, order.ContinuationSessionID, order.ContinuationAttemptID, order.ContinuationHarness, order.ContinuationLaunchEnvironment,
			rateLimitJSON(order.RateLimit), nullableTimeValue(order.RateLimitObservedAt), workspace(ctx), order.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("work order %s not found", order.ID)
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order)}); err != nil {
			return err
		}
		if current.State != core.WorkOrderCompleted && order.State == core.WorkOrderCompleted {
			return retireWorkOrderSiblingsTx(ctx, tx, q, workspace(ctx), order, "stage completed", now, false)
		}
		return nil
	}()
	return lifecycleErr, err
}

func updateRequiresClaim(next, current core.WorkOrderState) bool {
	if next == core.WorkOrderClaimed {
		return true
	}
	return current != next && (next == core.WorkOrderSubmitted || next == core.WorkOrderCompleted)
}

// AcceptReviewDecision commits the durable verdict, routing decision, and
// eligible GitHub publication job as one transaction. A retry therefore sees
// either the entire accepted decision or none of it.
func (s *Store) AcceptReviewDecisionCommand(ctx context.Context, lease taskops.TaskLease, decision core.ReviewDecision) error {
	if !lease.ValidFor(decision.TaskID) {
		return fmt.Errorf("review lifecycle mutation requires a valid taskops lease")
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), decision.TaskID); err != nil {
			return err
		}
		lockKey := fmt.Sprintf("conveyor:review:%s:%s:%d", workspace(ctx), decision.TaskID, decision.ReviewRound)
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockKey); err != nil {
			return err
		}
		var completed, accepted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM events e JOIN tasks t ON t.id=e.task_id
			WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='review.completed'
				AND e.payload_json->>'review_work_order_id'=$3
		), EXISTS (
			SELECT 1 FROM events e JOIN tasks t ON t.id=e.task_id
			WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='review.accepted'
				AND e.payload_json->>'review_work_order_id'=$3
		)`, workspace(ctx), decision.TaskID, decision.ReviewWorkOrderID).Scan(&completed, &accepted); err != nil {
			return err
		}
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: decision.TaskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", decision.TaskID)
		}
		if err := requireVerifyReviewTx(ctx, tx, taskFromDB(before)); err != nil {
			return err
		}
		lookup := store.ExecutionDocumentLookup(func(ctx context.Context, id string, version int) (core.SpecVersion, bool, error) {
			var spec core.SpecVersion
			err := tx.QueryRow(ctx, `SELECT s.content,s.approved FROM task_specs s
JOIN tasks t ON t.id=s.task_id
WHERE t.workspace_id=$1 AND s.task_id=$2 AND (($3::integer=0 AND s.approved) OR ($3::integer>0 AND s.version=$3))
ORDER BY s.version DESC LIMIT 1`, workspace(ctx), id, version).Scan(&spec.Content, &spec.Approved)
			if errors.Is(err, pgx.ErrNoRows) {
				return core.SpecVersion{}, false, nil
			}
			return spec, err == nil, err
		})
		verificationState := store.VerificationReviewState{}
		if taskFromDB(before).SetupContract.VerifyStage {
			records, e := q.ListVerificationRecords(ctx, workspace(ctx), decision.TaskID)
			if e != nil {
				return e
			}
			reviewOrder, e := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2", workspace(ctx), decision.ReviewWorkOrderID))
			if e != nil {
				return e
			}
			verificationState = store.VerificationReviewState{Rows: verificationRows(records), Order: reviewOrder}
		}
		if err := store.ValidateReviewAcceptance(ctx, lookup, taskFromDB(before), &decision, verificationState); err != nil {
			return err
		}

		job, err := q.GetJob(ctx, db.GetJobParams{ID: decision.JobID, WorkspaceID: workspace(ctx)})
		if err != nil || job.TaskID != decision.TaskID {
			return fmt.Errorf("job %s does not belong to task %s in workspace %s", decision.JobID, decision.TaskID, workspace(ctx))
		}
		var order core.WorkOrder
		if decision.ClaimSession != "" {
			order, err = scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), decision.ReviewWorkOrderID))
			if err != nil || order.TaskID != decision.TaskID || order.JobID != decision.JobID || order.Stage != core.StageReview {
				return fmt.Errorf("review work order %s does not belong to task %s in workspace %s", decision.ReviewWorkOrderID, decision.TaskID, workspace(ctx))
			}
		}
		if accepted {
			if decision.ClaimSession != "" {
				return s.settleAcceptedReviewTx(ctx, tx, q, order, jobFromDB(job), time.Now().UTC())
			}
			return nil
		}
		if decision.ClaimSession != "" {
			if order.State != core.WorkOrderClaimed || order.SessionID != decision.ClaimSession || !order.LeaseExpiresAt.After(time.Now()) {
				return store.ErrWorkOrderClaimLost
			}
			if core.TaskState(before.State) == core.TaskQueued {
				if err = insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{
					"from": core.TaskQueued, "to": core.TaskRunning, "command": core.TaskOrderClaim, "claim_authority_work_order_id": order.ID,
				})}); err != nil {
					return err
				}
				before.State = string(core.TaskRunning)
			}
		}
		if !completed {
			if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.completed", Payload: reviewDecisionPayload(decision)}); err != nil {
				return err
			}
		}
		if decision.PublicationEligible {
			if err := s.queueReviewPublicationTx(ctx, tx, q, reviewPublicationFromDecision(decision)); err != nil {
				return err
			}
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.accepted", Payload: core.JSONPayload(map[string]any{"review_work_order_id": decision.ReviewWorkOrderID, "review_round": decision.ReviewRound, "review_seat": decision.ReviewSeat})}); err != nil {
			return err
		}
		if decision.ClaimSession != "" {
			if err := s.settleAcceptedReviewTx(ctx, tx, q, order, jobFromDB(job), time.Now().UTC()); err != nil {
				return err
			}
		}
		reviews, required, err := completedReviewRoundTx(ctx, tx, workspace(ctx), decision.TaskID, decision.ReviewRound, decision.ReviewWorkOrderID)
		if err != nil {
			return err
		}
		if len(reviews) < required {
			return insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, Kind: "review.round_pending", Payload: core.JSONPayload(map[string]any{"review_round": decision.ReviewRound, "completed": len(reviews), "required": required})})
		}
		aggregate := aggregateReviewRoundResult(decision.ReviewRound, reviews)
		if err = insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.round_completed", Payload: core.JSONPayload(aggregate)}); err != nil {
			return err
		}

		command, next, recovery := core.TaskGateMerge, core.Stage(""), core.StageImplement
		autoApprove := false
		if aggregate.Verdict == "changes_requested" {
			count, countErr := q.CountEvents(ctx, db.CountEventsParams{TaskID: nullableText(decision.TaskID), Kind: "pipeline.bounced", WorkspaceID: workspace(ctx)})
			if countErr != nil {
				return countErr
			}
			// The check-in comparison uses bounces since the last human
			// intervention, not the lifetime count; the
			// recorded count in the event payload stays lifetime.
			window, windowErr := q.CountEventsSinceHumanIntervention(ctx, db.CountEventsSinceHumanInterventionParams{TaskID: nullableText(decision.TaskID), Kind: "pipeline.bounced", WorkspaceID: workspace(ctx)})
			if windowErr != nil {
				return windowErr
			}
			count++
			window++
			actorID := fmt.Sprintf("review:round:%d", decision.ReviewRound)
			plan := store.PlanChangesRequested(store.ChangesRequestedInput{
				TaskID: decision.TaskID, JobID: decision.JobID, ActorID: actorID, ActorRole: core.ActorAgent,
				ReasonCode: aggregate.ReasonCode, Feedback: aggregate.Feedback, Source: "mcp-review-panel",
				Count: int(count), Window: int(window), MaxBounces: decision.MaxBounces, ReviewRound: decision.ReviewRound,
				Reviews: aggregate.Reviews, Requeue: core.TaskStageAdvance, EnforceLimit: true,
			})
			if _, err = q.InsertIntervention(ctx, interventionInsertParams(plan.Intervention)); err != nil {
				return err
			}
			for _, event := range plan.Events {
				if err = insertEvent(ctx, q, event); err != nil {
					return err
				}
			}
			command, next, recovery = plan.Command, plan.NextStage, plan.Recovery
		} else if decision.ReviewKind == "refresh" || (decision.PolicyVersion > 0 && !decision.MergeApproval) || (decision.PolicyVersion == 0 && decision.Level == core.L0) {
			autoApprove, recovery = true, ""
		}
		fromState := core.TaskState(before.State)
		state, transitionErr := core.TransitionTask(fromState, command)
		if transitionErr != nil {
			return transitionErr
		}
		if autoApprove {
			approved, approveErr := core.TransitionTask(state, core.TaskInterventionApproveReview)
			if approveErr != nil {
				return approveErr
			}
			// Auto-approval currently projects running -> awaiting_human with
			// gate.merge even though the merge gate is off. The table has no direct
			// running -> approved edge; keep this explicit gap workaround visible
			// until a table amendment supplies the intended command.
			if err = insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": command})}); err != nil {
				return err
			}
			fromState, state = state, approved
			command = core.TaskInterventionApproveReview
		}
		nonAdvancingRefresh := store.NonAdvancingRefreshBinding(decision, aggregate.ApprovedHeadSHA)
		if nonAdvancingRefresh {
			if err = insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.refresh_binding_not_advanced", Payload: core.JSONPayload(map[string]any{
				"review_work_order_id": decision.ReviewWorkOrderID, "review_round": decision.ReviewRound,
				"baseline_sha": decision.BaselineSHA, "head_sha": decision.HeadSHA, "bound_sha": aggregate.ApprovedHeadSHA,
			})}); err != nil {
				return err
			}
		}
		if aggregate.Verdict == "approve" && aggregate.ApprovedHeadSHA != "" {
			if state == core.TaskApproved {
				query := `UPDATE tasks SET reviewed_head_sha=$1,updated_at=now() WHERE workspace_id=$2 AND id=$3`
				if !nonAdvancingRefresh {
					query = `UPDATE tasks SET reviewed_head_sha=$1,approved_head_sha=$1,approval_stale=false,refresh_baseline_sha='',refresh_head_sha='',refresh_review_scope='',updated_at=now() WHERE workspace_id=$2 AND id=$3`
				}
				if _, err = tx.Exec(ctx, query, aggregate.ApprovedHeadSHA, workspace(ctx), decision.TaskID); err != nil {
					return err
				}
			} else if _, err = tx.Exec(ctx, `UPDATE tasks SET reviewed_head_sha=$1,updated_at=now() WHERE workspace_id=$2 AND id=$3`, aggregate.ApprovedHeadSHA, workspace(ctx), decision.TaskID); err != nil {
				return err
			}
		}

		if _, err := q.UpdateTaskTransition(ctx, db.UpdateTaskTransitionParams{
			ID: decision.TaskID, WorkspaceID: workspace(ctx), State: string(state),
			NextStage: string(next), RecoveryStage: string(recovery),
		}); err != nil {
			return err
		}
		// When autoApprove is true, this second projection records an intervention
		// command without a human intervention. It is the paired gap workaround for
		// the absent running -> approved table edge.
		if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": command})}); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{
			"from_stage": before.NextStage, "next_stage": next, "recovery_stage": recovery, "state": state,
			"review_round": decision.ReviewRound,
		})}); err != nil {
			return err
		}
		if state == core.TaskQueued {
			if _, err := s.enqueueTaskTx(ctx, tx, decision.TaskID, before.WorkspaceID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) settleAcceptedReviewTx(ctx context.Context, tx pgx.Tx, q *db.Queries, order core.WorkOrder, job core.Job, now time.Time) error {
	if order.State != core.WorkOrderCompleted {
		next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdSubmitReviewVerdict)
		if err != nil {
			return err
		}
		order.State, order.Claimable, order.OperatorDirection, order.UpdatedAt = next, false, "", now
		order.ContinuationSessionID, order.ContinuationAttemptID = "", ""
		order.ContinuationHarness, order.ContinuationLaunchEnvironment = "", ""
		tag, err := tx.Exec(ctx, `UPDATE work_orders SET state=$1,operator_direction='',continuation_session_id='',continuation_attempt_id='',continuation_harness='',continuation_launch_environment='',updated_at=$2
			WHERE workspace_id=$3 AND id=$4 AND state=$5`, order.State, now, workspace(ctx), order.ID, core.WorkOrderClaimed)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("work order %s changed while accepting its review verdict", order.ID)
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order), At: now}); err != nil {
			return err
		}
	}
	if job.State != core.JobDone {
		if err := store.ValidateJobTransition(job.State, core.JobDone); err != nil {
			return err
		}
		job.State, job.EndedAt = core.JobDone, now
		job.TokensIn, job.TokensOut = order.TokensIn, order.TokensOut
		if cost := order.HistoricalCostUSD(); cost != nil {
			job.CostUSD = cost
		}
		if _, err := q.UpdateJob(ctx, jobUpdateParams(job, workspace(ctx))); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: job.TaskID, JobID: job.ID, Kind: "job.updated", Payload: core.JSONPayload(job), At: now}); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `UPDATE work_orders
		SET continuation_session_id='',continuation_attempt_id='',continuation_harness='',continuation_launch_environment='',updated_at=$1
		WHERE workspace_id=$2 AND task_id=$3 AND stage='implement' AND state='submitted'
			AND (continuation_session_id<>'' OR continuation_attempt_id<>'' OR continuation_harness<>'' OR continuation_launch_environment<>'')
		RETURNING `+workOrderColumns, now, workspace(ctx), order.TaskID)
	if err != nil {
		return err
	}
	// pgx refuses another statement on the transaction connection while this
	// result set is open, so drain it before appending events.
	var cleared []core.WorkOrder
	for rows.Next() {
		implementation, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		cleared = append(cleared, implementation)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, implementation := range cleared {
		if err = insertEvent(ctx, q, core.Event{TaskID: implementation.TaskID, JobID: implementation.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(implementation), At: now}); err != nil {
			return err
		}
	}
	return nil
}

type completedReviewRecord struct {
	ReviewWorkOrderID string `json:"review_work_order_id"`
	Verdict           string `json:"verdict"`
	ReasonCode        string `json:"reason_code"`
	Summary           string `json:"summary"`
	Feedback          string `json:"feedback"`
	ReviewerModel     string `json:"reviewer_model"`
	ReviewRound       int    `json:"review_round"`
	ReviewSeat        int    `json:"review_seat"`
	RequiredModel     string `json:"required_model"`
	RequiredHarness   string `json:"required_harness"`
	RequiredEffort    string `json:"required_effort"`
	ModelEnforcement  string `json:"model_enforcement"`
	ReviewedCommitSHA string `json:"reviewed_commit_sha"`
}

type reviewRoundResultRecord struct {
	ReviewRound     int                     `json:"review_round"`
	Verdict         string                  `json:"verdict"`
	ReasonCode      string                  `json:"reason_code"`
	Summary         string                  `json:"summary"`
	Feedback        string                  `json:"feedback,omitempty"`
	Reviews         []completedReviewRecord `json:"reviews"`
	ApprovedHeadSHA string                  `json:"approved_head_sha,omitempty"`
}

func completedReviewRoundTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID string, round int, workOrderID string) ([]completedReviewRecord, int, error) {
	superseded, err := supersededReviewWorkOrdersTx(ctx, tx, workspaceID, taskID)
	if err != nil {
		return nil, 0, err
	}
	required := 1
	if round > 0 {
		rows, countErr := tx.Query(ctx, `SELECT id FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review' AND review_round=$3`, workspaceID, taskID, round)
		if countErr != nil {
			return nil, 0, countErr
		}
		required = 0
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, 0, err
			}
			if !superseded[id] {
				required++
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, 0, err
		}
		rows.Close()
		if required == 0 {
			required = 1
		}
	}
	query := `SELECT e.payload_json FROM events e JOIN tasks t ON t.id=e.task_id WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='review.completed' AND COALESCE((e.payload_json->>'review_round')::integer,0)=$3`
	args := []any{workspaceID, taskID, round}
	if round == 0 {
		query += ` AND e.payload_json->>'review_work_order_id'=$4`
		args = append(args, workOrderID)
	}
	query += ` ORDER BY COALESCE((e.payload_json->>'review_seat')::integer,0), e.id`
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var reviews []completedReviewRecord
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return nil, 0, err
		}
		var review completedReviewRecord
		if err = json.Unmarshal(payload, &review); err != nil {
			return nil, 0, err
		}
		if !superseded[review.ReviewWorkOrderID] {
			reviews = append(reviews, review)
		}
	}
	return reviews, required, rows.Err()
}

func aggregateReviewRoundResult(round int, reviews []completedReviewRecord) reviewRoundResultRecord {
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].ReviewSeat < reviews[j].ReviewSeat })
	if round == 0 && len(reviews) == 1 {
		review := reviews[0]
		result := reviewRoundResultRecord{ReviewRound: round, Verdict: review.Verdict, ReasonCode: review.ReasonCode, Summary: review.Summary, Feedback: review.Feedback, Reviews: reviews}
		if review.Verdict == "approve" {
			result.ApprovedHeadSHA = review.ReviewedCommitSHA
		}
		return result
	}
	result := reviewRoundResultRecord{ReviewRound: round, Verdict: "approve", ReasonCode: "approved", Summary: "All review panel seats approved.", Reviews: reviews}
	var feedback []string
	for _, review := range reviews {
		if review.Verdict == "changes_requested" {
			result.Verdict, result.ReasonCode, result.Summary = "changes_requested", "panel_changes_requested", "The review panel requested changes."
		}
		if strings.TrimSpace(review.Feedback) != "" {
			feedback = append(feedback, fmt.Sprintf("Seat %d (%s, %s): %s", review.ReviewSeat, review.RequiredModel, review.ModelEnforcement, strings.TrimSpace(review.Feedback)))
		}
	}
	if result.Verdict == "approve" && len(reviews) > 0 {
		result.ApprovedHeadSHA = reviews[0].ReviewedCommitSHA
		for _, review := range reviews[1:] {
			if review.ReviewedCommitSHA != result.ApprovedHeadSHA {
				result.Verdict, result.ReasonCode, result.Summary, result.ApprovedHeadSHA = "changes_requested", "review_head_mismatch", "Review seats evaluated different pull-request heads.", ""
				break
			}
		}
	}
	result.Feedback = strings.Join(feedback, "\n")
	return result
}

func reviewDecisionPayload(decision core.ReviewDecision) []byte {
	return core.JSONPayload(map[string]any{
		"review_work_order_id": decision.ReviewWorkOrderID, "verdict": decision.Verdict,
		"reason_code": decision.ReasonCode, "summary": decision.Summary, "feedback": decision.Feedback,
		"reviewed_commit_sha": decision.ReviewedCommitSHA, "reviewer": decision.Reviewer,
		"evidence_ids":            decision.EvidenceIDs,
		"requirement_citations":   decision.RequirementCitations,
		"done_criteria_coverage":  decision.DoneCriteriaAssessment,
		"governance_assessment":   decision.GovernanceAssessment,
		"verification_assessment": decision.VerificationAssessment,
		"reviewer_model":          decision.ReviewerModel, "reviewer_session": decision.ReviewerSession,
		"same_model_as_implementer": decision.SameModelAsImplementer,
		"review_round":              decision.ReviewRound, "review_seat": decision.ReviewSeat,
		"review_kind": decision.ReviewKind, "review_scope": decision.ReviewScope,
		"baseline_sha": decision.BaselineSHA, "head_sha": decision.HeadSHA,
		"required_model": decision.RequiredModel, "required_harness": decision.RequiredHarness,
		"required_effort":      decision.RequiredEffort,
		"model_enforcement":    decision.ModelEnforcement,
		"publication_eligible": decision.PublicationEligible,
	})
}

func scanWorkOrder(row interface{ Scan(...any) error }) (core.WorkOrder, error) {
	var order core.WorkOrder
	var stage, state string
	var harnessConfig, checkpoint, rateLimit, servedRequirementSnapshot, governanceSnapshot []byte
	var lease, queueEntered, queueDeadline, queueBlockedAt, executionStarted, executionDeadline, lastFailureAt, nextRetryAt, rateLimitObservedAt pgtype.Timestamptz
	err := row.Scan(&order.ID, &order.TaskID, &order.JobID, &stage, &state, &order.ClaimantID,
		&order.SessionID, &order.AttemptID, &order.ClientTokenHash, &order.Agent, &order.Model, &order.WorkerID, &lease,
		&order.ReviewRound, &order.ReviewSeat, &order.RequiredModel, &order.RequiredHarness, &order.RequiredEffort, &harnessConfig, &order.ExecutionTimeoutText, &order.ModelEnforcement,
		&order.ReasonCode, &order.ReviewKind, &order.ReviewScope, &order.BaselineSHA, &order.HeadSHA, &order.VerificationContextID,
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

// clearExecutionPinsSQL resets every harness, model, and effort pin on an
// order re-entering the queue, including a legacy server-pinned harness snapshot.
// Review round and seat stay untouched (req-worker AC-2.2, AC-2.3; DEC-56).
const clearExecutionPinsSQL = `required_model='',required_harness='',required_effort='',required_harness_config='{}'::jsonb`

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

func specFromDB(spec db.TaskSpec) core.SpecVersion {
	return core.SpecVersion{
		TaskID: spec.TaskID, Version: int(spec.Version), Content: spec.Content,
		AcceptanceCount: int(spec.AcceptanceCount), Acceptance: append([]byte(nil), spec.Acceptance...),
		Decomposition: append([]byte(nil), spec.Decomposition...), Approved: spec.Approved,
		Agent: spec.Agent, Model: spec.Model,
		CreatedAt: spec.CreatedAt.Time, ApprovedAt: nullableTime(spec.ApprovedAt),
	}
}

func firstOrderDirectionTx(ctx context.Context, tx pgx.Tx, order *core.WorkOrder) error {
	var direction string
	err := tx.QueryRow(ctx, `SELECT intake_operator_direction FROM tasks WHERE workspace_id=$1 AND id=$2 AND NOT EXISTS(SELECT 1 FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND id<>$3)`, workspace(ctx), order.TaskID, order.ID).Scan(&direction)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if direction == "" {
		return nil
	}
	order.OperatorDirection = direction
	_, err = tx.Exec(ctx, `UPDATE work_orders SET operator_direction=$3 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), order.ID, direction)
	return err
}
