package store

import (
	"context"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// req-verification-kits REQ-4/REQ-7; feature-verification-kit-execution VK-2/VK-7 and component-task-lifecycle.

type VerificationCompletion struct {
	Task         core.Task
	Order        core.WorkOrder
	Job          core.Job
	Events       []core.Event
	Intervention *core.Intervention
}

// PrepareVerificationCompletion returns the entire lifecycle write set. Backend
// adapters commit it together with the sealed context, under the same lock.
func PrepareVerificationCompletion(ctx context.Context, task core.Task, order core.WorkOrder, job core.Job, rows []VerificationRow, events []core.Event, c VerificationCommand, now time.Time) (VerificationCompletion, error) {
	var out VerificationCompletion
	if c.Submission == nil || !task.SetupContract.VerifyStage || order.Stage != core.StageVerify || order.HeadSHA == "" || order.HeadSHA != core.VerifyStageHead(task) || task.NextStage != core.StageVerify || task.ID != order.TaskID || job.ID != order.JobID || job.TaskID != task.ID {
		return out, ErrVerificationState
	}
	row, ok := verificationFind(rows, "verification_contexts", c.ContextID)
	if !ok {
		return out, ErrVerificationAccess
	}
	vc := verificationDecode[VerificationContext](row)
	if err := VerifyVerificationAuthority(VerificationCommand{Kind: VerificationCreateContext, Context: &vc}, VerificationAuthoritySnapshot{Task: task}, order); err != nil {
		return out, err
	}
	for _, other := range rows {
		if other.Table != "verification_contexts" || other.TaskID != task.ID {
			continue
		}
		newer := verificationDecode[VerificationContext](other)
		if newer.CreatedAt.After(vc.CreatedAt) {
			return out, ErrVerificationState
		}
	}
	actor := ActorFromContext(ctx)
	originalAttempt, originalSession := order.AttemptID, order.SessionID
	from := task.State
	if from == core.TaskQueued {
		state, err := core.TransitionTask(from, core.TaskOrderClaim)
		if err != nil {
			return out, err
		}
		out.Events = append(out.Events, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": from, "to": state, "command": core.TaskOrderClaim})})
		from, task.State = state, state
	}
	command := core.TaskStageAdvance
	next, recovery := core.StageReview, core.Stage("")
	orderCommand := core.WorkOrderCmdSubmitVerification
	jobCommand := "job.complete"
	switch c.Submission.Outcome {
	case "feedback":
		count, window := 1, 1
		for _, e := range events {
			if e.Kind == "pipeline.bounced" {
				count++
				window++
			}
			if e.ActorRole == core.ActorUser && len(e.Kind) > 13 && e.Kind[:13] == "intervention." {
				window = 1
			}
		}
		plan := PlanChangesRequested(ChangesRequestedInput{TaskID: task.ID, JobID: job.ID, ActorID: actor.ID, ActorRole: actor.Role, ReasonCode: "verification-failed", Feedback: c.Submission.Feedback, Source: "verification", Count: count, Window: window, MaxBounces: task.SetupContract.MaxBounces, Requeue: core.TaskStageAdvance, EnforceLimit: true, At: now})
		command, next, recovery = plan.Command, plan.NextStage, plan.Recovery
		out.Events = append(out.Events, plan.Events...)
		out.Intervention = &plan.Intervention
	case "operator_action_required":
		// Ordinary checkpoint release preserves the verify route and suppresses
		// redispatch until authenticated recovery; it does not create review.
		orderCommand, jobCommand = core.WorkOrderCmdRelease, "job.fail"
		next, recovery = core.StageVerify, ""
		order.LastAttemptID, order.LastAttemptOutcome = order.AttemptID, "released"
		order.LastFailureMessage = core.WorkOrderReleaseReasonOperatorCheckpointReached
		order.LastFailureDetail, order.LastFailureCategory, order.LastFailureExitStatus = c.Submission.Feedback, "", nil
		order.LastFailureAt, order.NextRetryAt = now, time.Time{}
		queueWindow := order.QueueDeadline.Sub(order.QueueEnteredAt)
		if queueWindow <= 0 {
			queueWindow = 24 * time.Hour
		}
		order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueWindow)
		order.RetrySuppressed, order.RetrySuppressionReason = true, "operator checkpoint reached"
		order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: c.Submission.Feedback}
		clearActiveAttempt(&order)
	case "succeeded":
		order.VerificationContextID = c.ContextID
	default:
		return out, ErrVerificationInvalid
	}
	var err error
	order.State, err = core.TransitionWorkOrder(order.State, orderCommand)
	if err != nil {
		return out, err
	}
	job.State, err = core.TransitionJob(job.State, jobCommand)
	if err != nil {
		return out, err
	}
	task.State, err = core.TransitionTask(from, command)
	if err != nil {
		return out, err
	}
	task.NextStage, task.RecoveryStage = next, recovery
	order.Claimable, order.UpdatedAt, order.OperatorDirection = false, now, ""
	job.EndedAt = now
	if orderCommand == core.WorkOrderCmdRelease {
		job.State, err = core.TransitionJob(job.State, "job.retry")
		if err != nil {
			return out, err
		}
		job.StartedAt, job.EndedAt = time.Time{}, time.Time{}
	}
	kind := "work_order.updated"
	orderPayload := core.JSONPayload(order)
	if orderCommand == core.WorkOrderCmdRelease {
		orderPayload = core.JSONPayload(map[string]any{"attempt_id": originalAttempt, "session_id": originalSession, "reason": core.WorkOrderReleaseReasonOperatorCheckpointReached, "release_cause": core.WorkOrderReleaseCauseOperatorAction, "outcome": core.WorkOrderOutcomeReleased, "checkpoint": order.Checkpoint, "retry_suppressed": true, "suppression_reason": order.RetrySuppressionReason, "verification_context_id": c.ContextID})
		kind = "work_order.released"
	}
	out.Events = append(out.Events,
		core.Event{TaskID: task.ID, JobID: job.ID, Kind: kind, Payload: orderPayload},
		core.Event{TaskID: task.ID, JobID: job.ID, Kind: "job.updated", Payload: core.JSONPayload(job)},
		core.Event{TaskID: task.ID, JobID: job.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": from, "to": task.State, "command": command})},
		core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": core.StageVerify, "next_stage": next, "recovery_stage": recovery, "state": task.State})})
	for i := range out.Events {
		out.Events[i].ActorID, out.Events[i].ActorRole, out.Events[i].At = actor.ID, actor.Role, now
	}
	out.Task, out.Order, out.Job = task, order, job
	return out, nil
}
