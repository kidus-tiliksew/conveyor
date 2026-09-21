package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) completeVerificationTx(ctx context.Context, tx pgx.Tx, q *db.Queries, c store.VerificationCommand, order core.WorkOrder, rows []store.VerificationRow) error {
	task, err := q.GetTask(ctx, db.GetTaskParams{ID: order.TaskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return err
	}
	job, err := q.GetJob(ctx, db.GetJobParams{ID: order.JobID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return err
	}
	history, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(order.TaskID), WorkspaceID: workspace(ctx)})
	if err != nil {
		return err
	}
	events := make([]core.Event, len(history))
	for i, e := range history {
		events[i] = eventFromDB(e)
	}
	v, err := store.PrepareVerificationCompletion(ctx, taskFromDB(task), order, jobFromDB(job), rows, events, c, time.Now().UTC())
	if err != nil {
		return err
	}
	o := v.Order
	_, err = tx.Exec(ctx, `UPDATE work_orders SET last_failure_detail=$23,last_failure_category=$24,last_failure_exit_status=$25,last_failure_at=$26,next_retry_at=$27,queue_entered_at=$28,queue_deadline=$29,attempt_id=$17,agent=$18,model=$19,model_enforcement=$20,execution_started_at=$21,execution_deadline=$22,verification_context_id=$16,state=$1,claimant_id=$2,session_id=$3,client_token_hash=$4,worker_id=$5,lease_expires_at=$6,last_attempt_id=$7,last_attempt_outcome=$8,retry_suppressed=$9,retry_suppression_reason=$10,last_failure_message=$11,checkpoint=$12,operator_direction='',updated_at=$13 WHERE workspace_id=$14 AND id=$15`, o.State, o.ClaimantID, o.SessionID, o.ClientTokenHash, o.WorkerID, nullableTimeValue(o.LeaseExpiresAt), o.LastAttemptID, o.LastAttemptOutcome, o.RetrySuppressed, o.RetrySuppressionReason, o.LastFailureMessage, checkpointJSON(o.Checkpoint), o.UpdatedAt, workspace(ctx), o.ID, o.VerificationContextID, o.AttemptID, o.Agent, o.Model, o.ModelEnforcement, nullableTimeValue(o.ExecutionStartedAt), nullableTimeValue(o.ExecutionDeadline), o.LastFailureDetail, o.LastFailureCategory, o.LastFailureExitStatus, nullableTimeValue(o.LastFailureAt), nullableTimeValue(o.NextRetryAt), nullableTimeValue(o.QueueEnteredAt), nullableTimeValue(o.QueueDeadline))
	if err != nil {
		return err
	}
	if _, err = q.UpdateJob(ctx, jobUpdateParams(v.Job, workspace(ctx))); err != nil {
		return err
	}
	if _, err = q.UpdateTaskTransition(ctx, db.UpdateTaskTransitionParams{ID: v.Task.ID, WorkspaceID: workspace(ctx), State: string(v.Task.State), NextStage: string(v.Task.NextStage), RecoveryStage: string(v.Task.RecoveryStage)}); err != nil {
		return err
	}
	if v.Intervention != nil {
		if _, err = q.InsertIntervention(ctx, interventionInsertParams(*v.Intervention)); err != nil {
			return err
		}
	}
	for _, e := range v.Events {
		if err = insertEvent(ctx, q, e); err != nil {
			return err
		}
	}
	if v.Task.State == core.TaskQueued && !v.Order.RetrySuppressed {
		_, err = s.enqueueTaskTx(ctx, tx, v.Task.ID, workspace(ctx))
	}
	return err
}
