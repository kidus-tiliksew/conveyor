package store

import (
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Changing the verify toggle must preserve the exact comparison that sealed
// review acceptance checks (component-verification-service).
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
				result, err := PlanTaskPolicyChange(task, []core.WorkOrder{prior}, request, PolicyHandoffSeatInputs{})
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
						if _, err := PlanTaskPolicyChange(task, []core.WorkOrder{bad}, request, PolicyHandoffSeatInputs{}); !errors.Is(err, ErrSetupChangeConflict) {
							t.Fatalf("missing trusted binding accepted: %v", err)
						}
					}
					conflict := prior
					conflict.BaselineSHA = "other"
					if _, err := PlanTaskPolicyChange(task, []core.WorkOrder{prior, conflict}, request, PolicyHandoffSeatInputs{}); !errors.Is(err, ErrSetupChangeConflict) {
						t.Fatalf("ambiguous binding accepted: %v", err)
					}
				} else {
					task.RefreshBaselineSHA = ""
					if _, err := PlanTaskPolicyChange(task, []core.WorkOrder{prior}, request, PolicyHandoffSeatInputs{}); !errors.Is(err, ErrSetupChangeConflict) {
						t.Fatalf("missing refresh binding accepted: %v", err)
					}
				}
			})
		}
	}
	// The handoff review round resolves through RS-2 from the inputs the
	// backend loaded in its transaction (feature-review-seat-minimum RS-2).
	t.Run("ResolvedSeats", func(t *testing.T) {
		disabled := false
		task := core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head",
			SetupContract: config.ExecutionSetup{VerifyStage: true, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}}}
		verify := core.WorkOrder{ID: "verify", TaskID: task.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: "head", BaselineSHA: "base"}
		previous := core.WorkOrder{ID: "task-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.WorkOrderCompleted, ReviewRound: 1, ReviewSeat: 1, HeadSHA: "head", BaselineSHA: "base"}
		record := ReviewRoundSeats{TaskID: task.ID, ReviewRound: 1, SeatCount: 3, SeatCountSource: ReviewSeatSourcePlanned, MinimumSeatCount: 2, ImplementationWorkOrderID: "impl"}
		implement := core.WorkOrder{ID: "impl", TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderSubmitted}
		four, one := 4, 1
		for _, tc := range []struct {
			name     string
			orders   []core.WorkOrder
			seats    PolicyHandoffSeatInputs
			count    int
			source   string
			conflict bool
		}{
			{"Carried", []core.WorkOrder{verify, previous, implement}, PolicyHandoffSeatInputs{Records: []ReviewRoundSeats{record}}, 3, ReviewSeatSourceCarried, false},
			{"PlannedAbovePrevious", []core.WorkOrder{verify, previous, implement}, PolicyHandoffSeatInputs{Records: []ReviewRoundSeats{record}, PlannedSeatCount: &four}, 4, ReviewSeatSourcePlanned, false},
			{"Minimum", []core.WorkOrder{verify}, PolicyHandoffSeatInputs{}, 2, ReviewSeatSourceMinimum, false},
			{"DeclaredBelowFloor", []core.WorkOrder{verify, {ID: "impl-2", TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderSubmitted, DeclaredReviewSeats: &one}}, PolicyHandoffSeatInputs{}, 0, "", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				request := SetupChangeRequest{Policy: &TaskPolicyChange{VerifyStage: &disabled}, ReviewRoundSeats: &ReviewRoundSeatResolution{SeatCount: 9}}
				result, err := PlanTaskPolicyChange(task, tc.orders, request, tc.seats)
				if tc.conflict {
					if !errors.Is(err, ErrSetupChangeConflict) {
						t.Fatalf("below-floor handoff error=%v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				round := 1
				if len(tc.orders) > 1 {
					round = 2
				}
				if len(result.NewWorkOrders) != tc.count || result.ReviewRoundSeats == nil || result.ReviewRoundSeats.SeatCount != tc.count ||
					result.ReviewRoundSeats.Source != tc.source || result.ReviewRoundSeats.MinimumSeatCount != 2 || result.ResultingReviewRound != round {
					t.Fatalf("handoff orders=%d seats=%+v round=%d", len(result.NewWorkOrders), result.ReviewRoundSeats, result.ResultingReviewRound)
				}
				for i, order := range result.NewWorkOrders {
					if order.ReviewSeat != i+1 || order.ReviewRound != round || order.BaselineSHA != "base" || order.HeadSHA != "head" {
						t.Fatalf("handoff seat %d=%+v", i+1, order)
					}
				}
			})
		}
	})
}
