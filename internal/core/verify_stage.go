package core

import "fmt"

// VerifyStageHead binds verification to the submitted revision, including refresh
// submissions (DEC-43; feature-verification-kit-execution VK-2).
func VerifyStageHead(task Task) string {
	if task.ApprovalStale && task.RefreshHeadSHA != "" {
		return task.RefreshHeadSHA
	}
	return task.ReviewedHeadSHA
}

// VerifyReviewReady requires a completed order carrying its sealed context.
// Backend review acceptance rechecks the referenced result and pins (VK-7).
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
	if order.Stage == StageVerify {
		if !task.SetupContract.VerifyStage || task.NextStage != StageVerify || order.HeadSHA == "" || order.HeadSHA != VerifyStageHead(task) {
			return fmt.Errorf("verify order %s does not match task policy, stage, or submitted head", order.ID)
		}
	}
	return nil
}

// ConflictingExecutorClaims preserves same-stage exclusivity and prevents an
// implementer and verifier from executing against one task concurrently (VK-2).
func ConflictingExecutorClaims(a, b WorkOrder) bool {
	if a.ID == b.ID || a.TaskID != b.TaskID || b.State != WorkOrderClaimed {
		return false
	}
	if a.Stage == b.Stage {
		return a.Stage != StageReview
	}
	return (a.Stage == StageVerify && b.Stage == StageImplement) || (a.Stage == StageImplement && b.Stage == StageVerify)
}
