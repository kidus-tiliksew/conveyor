package core

import "fmt"

// VerifyStageHead binds verification to the submitted revision, including refresh
// submissions (DEC-43; component-verification-service).
func VerifyStageHead(task Task) string {
	if task.ApprovalStale && task.RefreshHeadSHA != "" {
		return task.RefreshHeadSHA
	}
	return task.ReviewedHeadSHA
}

// VerifyReviewReady requires a completed order carrying its sealed context.
// Backend review acceptance rechecks the referenced result and pins (component-verification-service).
func VerifyReviewReady(task Task, orders []WorkOrder) bool {
	if !task.SetupContract.VerifyStage {
		return true
	}
	head := VerifyStageHead(task)
	if head == "" {
		return false
	}
	for _, order := range orders {
		if order.TaskID == task.ID && order.Stage == StageVerify && order.State == WorkOrderCompleted && order.HeadSHA == head && order.VerificationContextID != "" {
			return true
		}
	}
	return false
}

func ValidateVerifyDispatch(task Task, order WorkOrder) error {
	if order.Stage != StageVerify {
		return nil
	}
	prefix := fmt.Sprintf("verify order %s cannot be claimed: ", order.ID)
	if !task.SetupContract.VerifyStage {
		return fmt.Errorf("%sverify stage is off in task %s's frozen policy", prefix, task.ID)
	}
	if task.NextStage != StageVerify {
		return fmt.Errorf("%stask %s next_stage is %s, not verify", prefix, task.ID, task.NextStage)
	}
	if order.HeadSHA == "" {
		return fmt.Errorf("%sthe order has no head", prefix)
	}
	want := VerifyStageHead(task)
	if order.HeadSHA != want {
		suffix := ""
		if task.ApprovalStale && task.RefreshHeadSHA != "" {
			suffix = " (the refresh head, because approval_stale is set)"
		}
		return fmt.Errorf("%sorder head %s differs from the verify head %s%s", prefix, order.HeadSHA, want, suffix)
	}
	return nil
}

// ConflictingExecutorClaims preserves same-stage exclusivity and prevents an
// implementer and verifier from executing against one task concurrently
// (component-task-lifecycle, executor exclusivity).
func ConflictingExecutorClaims(a, b WorkOrder) bool {
	if a.ID == b.ID || a.TaskID != b.TaskID || b.State != WorkOrderClaimed {
		return false
	}
	if a.Stage == b.Stage {
		return a.Stage != StageReview
	}
	return (a.Stage == StageVerify && b.Stage == StageImplement) || (a.Stage == StageImplement && b.Stage == StageVerify)
}
