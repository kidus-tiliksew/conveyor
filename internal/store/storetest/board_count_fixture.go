package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// SeedBoardCount adds complete groups of ten open tasks to the board.
func SeedBoardCount(t *testing.T, ctx context.Context, st store.Store, start, end int, now time.Time) {
	t.Helper()
	policy := &riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}
	var dependency string
	for i := start; i < end; i++ {
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: "board-count", Title: fmt.Sprintf("task %d", i), Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now}
		switch i % 10 {
		case 0:
			dependency = id
		case 1:
			task.State = core.TaskParked
		case 2:
			task.State, task.NextStage, task.ReviewedHeadSHA, task.MergeApproval = core.TaskRunning, core.StageReview, "reviewed", true
			task.SetupContract = config.PolicyContract{Risk: policy}
		case 3:
			task.State, task.NextStage = core.TaskRunning, core.StageSpec
			task.SetupContract = config.PolicyContract{Risk: policy}
		case 4:
			task.State = core.TaskApproved
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		switch i % 10 {
		case 0:
			job := core.Job{ID: id + "-implement", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
			order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: job.Stage, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(24 * time.Hour), CreatedAt: now}
			if _, err := CreateStageWorkOrder(ctx, st, job, order); err != nil {
				t.Fatal(err)
			}
			if _, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("board-author"), SessionID: id, ClientToken: id, Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
				t.Fatal(err)
			}
		case 1:
			if _, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: id, DependsOnTaskID: dependency, Reason: "board fixture", RequestID: "board-" + id}); err != nil {
				t.Fatal(err)
			}
		case 2:
			review := core.Job{ID: id + "-review", TaskID: id, Stage: core.StageReview, State: core.JobPending}
			if err := st.CreateJob(ctx, review); err != nil {
				t.Fatal(err)
			}
			if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: review.ID, JobID: review.ID, TaskID: id, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, HeadSHA: "reviewed", QueueEnteredAt: now, QueueDeadline: now.Add(24 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if _, err := ClaimWorkOrder(ctx, st, review.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("board-reviewer"), SessionID: id + "-review", ClientToken: id + "-review", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: id, Key: core.PlanRiskKey(1), Source: core.RiskSourcePlan, Tier: riskpolicy.TierMedium, PlanVersion: 1, Reason: "board fixture"}); err != nil {
				t.Fatal(err)
			}
			if _, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.RecordTaskApproval(ctx, core.TaskApproval{TaskID: id, Kind: core.TaskApprovalPerson, HeadSHA: "reviewed", ReviewRound: 1, UserID: "board-approver", Source: "approval"}); err != nil {
				t.Fatal(err)
			}
		case 3:
			spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: id, Content: "board plan", Acceptance: []byte(`[]`), Decomposition: []byte(`[]`)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = taskops.ExecuteWorkOrder(ctx, st, id, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
				order, _, err := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: id, Subject: core.RiskOrderSubject{PlanVersion: spec.Version}})
				return order, err
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true, PlanState: &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted}}); err != nil {
				t.Fatal(err)
			}
		case 4:
			if err := st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 1}), At: now}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
