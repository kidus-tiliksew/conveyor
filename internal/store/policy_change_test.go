package store

import (
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// VK-7: changing the verify toggle must preserve the exact comparison.
func TestPolicyHandoffVerificationBinding(t *testing.T) {
	for _, stage := range []core.Stage{core.StageReview, core.StageVerify} {
		for _, scope := range []string{"", config.RefreshReviewDelta, config.RefreshReviewFull} {
			t.Run(string(stage)+"/"+scope, func(t *testing.T) {
				task := core.Task{ID: "task", State: core.TaskRunning, NextStage: stage, ReviewedHeadSHA: "head"}
				base, head := "base", "head"
				if scope != "" {
					task.ApprovalStale = true
					task.RefreshReviewScope = scope
					task.RefreshBaselineSHA = "approved"
					task.RefreshHeadSHA = "new"
					base, head = "approved", "new"
				}
				enabled := stage == core.StageReview
				prior := core.WorkOrder{ID: "prior", TaskID: task.ID, Stage: stage, State: core.WorkOrderQueued, HeadSHA: head, BaselineSHA: base, ReviewScope: scope}
				request := SetupChangeRequest{Policy: &TaskPolicyChange{VerifyStage: &enabled}}
				result, err := PlanTaskPolicyChange(task, []core.WorkOrder{prior}, request)
				if err != nil {
					t.Fatal(err)
				}
				if len(result.NewWorkOrders) != 1 {
					t.Fatalf("orders: %+v", result.NewWorkOrders)
				}
				got := result.NewWorkOrders[0]
				if got.HeadSHA != head || got.BaselineSHA != base || got.ReviewScope != scope {
					t.Fatalf("binding changed: %+v", got)
				}
				if stage == core.StageVerify && scope != "" && got.ReviewKind != "refresh" {
					t.Fatal("refresh review kind lost")
				}
				if scope == "" {
					for _, bad := range []core.WorkOrder{{}, {TaskID: task.ID, Stage: stage, State: core.WorkOrderQueued, HeadSHA: "old", BaselineSHA: base}, {TaskID: task.ID, Stage: stage, State: core.WorkOrderQueued, HeadSHA: head}} {
						if _, err := PlanTaskPolicyChange(task, []core.WorkOrder{bad}, request); !errors.Is(err, ErrSetupChangeConflict) {
							t.Fatalf("missing trusted binding accepted: %v", err)
						}
					}
					conflict := prior
					conflict.BaselineSHA = "other"
					if _, err := PlanTaskPolicyChange(task, []core.WorkOrder{prior, conflict}, request); !errors.Is(err, ErrSetupChangeConflict) {
						t.Fatalf("ambiguous binding accepted: %v", err)
					}
				} else {
					task.RefreshBaselineSHA = ""
					if _, err := PlanTaskPolicyChange(task, []core.WorkOrder{prior}, request); !errors.Is(err, ErrSetupChangeConflict) {
						t.Fatalf("missing refresh binding accepted: %v", err)
					}
				}
			})
		}
	}
}
