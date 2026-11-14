package singlestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// TestSubmitRiskRatingCancelledInsideStoreWindowIntegration: the service that
// submit_risk_rating calls after it authorizes the session reads the risk order
// as claimed by that session. A cancellation or a raise to high then commits
// inside the store completion's read-to-write window, where the completion's
// conditional update changes no row. The service answers exactly
// ErrWorkOrderCancelled, never a lost claim, and nothing is recorded
// (req-risk-based-review AC-12.2, AC-12.3; component-persistence invariant 8).
func TestSubmitRiskRatingCancelledInsideStoreWindowIntegration(t *testing.T) {
	st := integrationStore(t)
	ws := "risk-window-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), ws), store.SystemActor())
	cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	for _, tc := range []struct {
		name string
		run  func(c context.Context, taskID, orderID string) error
	}{
		{"Cancellation", func(c context.Context, taskID, orderID string) error {
			_, err := taskops.ExecuteWorkOrder(c, st, taskID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (core.WorkOrder, error) {
				order, made, err := st.CancelRiskOrderCommand(c, lease, store.RiskOrderCancelRequest{WorkOrderID: orderID, Reason: "a later head was submitted"})
				if err == nil && !made {
					err = errors.New("the cancellation cancelled nothing")
				}
				return order, err
			})
			return err
		}},
		{"RaiseToHigh", func(c context.Context, taskID, _ string) error {
			_, err := st.RaiseTaskRiskTier(c, core.RiskRating{TaskID: taskID, Tier: riskpolicy.TierHigh, Reason: "touches billing", ActorID: "user:operator"}, store.RiskSeatSettings{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID := core.NewTaskID()
			task := core.Task{ID: taskID, Workspace: ws, Repo: "conveyor", Title: taskID, BaseBranch: "main", Branch: "conveyor/task-" + taskID,
				State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
			task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &record,
				StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "4h", Verify: "1h", Review: "45m"}}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			risk, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
				order, made, err := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: taskID, Subject: core.RiskOrderSubject{HeadSHA: "h-service", BaseSHA: "base-h-service"}})
				if err == nil && !made {
					err = errors.New("risk order was not created")
				}
				return order, err
			})
			if err != nil {
				t.Fatal(err)
			}
			claim := core.WorkOrderClaimIdentity{ClaimantID: "run:risk-owner", SessionID: "service-rater"}
			if _, err = storetest.ClaimWorkOrder(ctx, st, risk.ID, core.WorkOrderClaim{ClaimantID: claim.ClaimantID, SessionID: claim.SessionID, ClientToken: "service-rater-token",
				Agent: "claude-code", Model: "model-service-rater", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
				t.Fatal(err)
			}
			fired := 0
			var competitorErr, submitErr error
			if err = st.WithTaskSideEffectLock(ctx, taskID, func(lockCtx context.Context) error {
				hookCtx := store.WithRiskCompletionReadHook(lockCtx, func(id string, read core.WorkOrderState) {
					if id != risk.ID {
						return
					}
					fired++
					if read != core.WorkOrderClaimed {
						t.Errorf("completion read state %s, want claimed", read)
					}
					competitorErr = tc.run(lockCtx, taskID, risk.ID)
				})
				_, submitErr = service.SubmitRiskRating(hookCtx, claim, risk.ID, string(riskpolicy.TierLow), "inside the window")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if fired != 1 || competitorErr != nil {
				t.Fatalf("hook fired %d times, competitor err=%v", fired, competitorErr)
			}
			if !errors.Is(submitErr, store.ErrWorkOrderCancelled) || errors.Is(submitErr, store.ErrWorkOrderClaimLost) {
				t.Fatalf("SubmitRiskRating err=%v, want exactly %v", submitErr, store.ErrWorkOrderCancelled)
			}
			final, err := st.GetWorkOrder(ctx, risk.ID)
			if err != nil {
				t.Fatal(err)
			}
			ratings, err := st.ListTaskRiskRatings(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			for _, rating := range ratings {
				if rating.Source == core.RiskSourceRiskAgent {
					t.Fatalf("a cancelled order recorded a risk agent rating: %+v", rating)
				}
			}
			if final.State != core.WorkOrderCancelled {
				t.Fatalf("order after the cancellation=%+v", final)
			}
		})
	}
}
