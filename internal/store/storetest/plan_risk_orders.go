package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunPlanRiskOrderConformance proves on every backend that a plan version
// entering plan awaiting approval gets one risk order when the need decision
// calls for one, in the transaction of its entry: plan submission, intake
// with a plan for approval, and an attached plan; that a plan approved at
// intake, an off task, and a task at high get none; that a redirect, a later
// version, and an operator exit cancel the open order and approval does not;
// that under enforce approval waits for the order and changes nothing; and
// that a risk agent rating counts once its version is approved, in either
// order (req-risk-based-review AC-4.3, AC-4.7 to AC-4.9, AC-12.3, AC-1.6;
// req-task-plan-state AC-1.2, AC-1.3, AC-3.1, AC-3.2;
// feature-risk-based-review RR-13; component-risk-review invariants 12 and
// 13).
func RunPlanRiskOrderConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	person := core.AuthenticatedCredential{ID: "pat_plan_risk_orders", OwnerUserID: "usr_plan_risk_orders", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	personCtx := store.WithActor(store.WithCredential(ctx, person), store.Actor{ID: store.UserActorID(person.OwnerUserID), Role: core.ActorUser})
	repo := "conveyor"
	if x.Config != nil && len(x.Config.Repos) > 0 {
		repo = x.Config.Repos[0].Name
	}
	policy := func(mode riskpolicy.Mode) *riskpolicy.Policy {
		return &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}
	}
	newTask := func(mode riskpolicy.Mode, state core.TaskState, next core.Stage) core.Task {
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Source: "api", Repo: repo, Title: id, Body: "Plan risk orders " + id, BaseBranch: "main",
			Branch: "conveyor/task-" + id, State: state, NextStage: next, PlanState: core.PlanNeeded, PolicyVersion: 1,
			CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3}
		if mode != "" {
			task.SetupContract.Risk = policy(mode)
		}
		return task
	}
	// planningTask files a task whose plan order is running, so a plan
	// submission can take it to the plan approval gate.
	planningTask := func(t *testing.T, mode riskpolicy.Mode) core.Task {
		t.Helper()
		task := newTask(mode, core.TaskRunning, core.StageSpec)
		requireOK(t, st.CreateTask(ctx, task))
		return task
	}
	plan := planRiskPlan(riskpolicy.TierHigh, "Kept as plan text.")
	submit := func(taskID string) (core.Task, error) {
		outcome, err := taskops.New(st).Perform(ctx, taskID, taskops.Command{
			Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true,
			PlanState:   &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted},
			PlanVersion: &core.SpecVersion{Content: plan, Acceptance: []byte(`[]`), Decomposition: []byte(`[]`), Agent: "codex", Model: "planner"},
		})
		return outcome.Task, err
	}
	mustSubmit := func(t *testing.T, taskID string) {
		t.Helper()
		task, err := submit(taskID)
		requireOK(t, err)
		if task.State != core.TaskAwaiting || task.GateKind != core.GateKindPlanApproval || task.PlanState != core.PlanAwaitingApproval {
			t.Fatalf("submitted task=%s/%s plan %s", task.State, task.GateKind, task.PlanState)
		}
	}
	planOrders := func(t *testing.T, taskID string) []core.RiskOrder {
		t.Helper()
		orders, err := st.ListTaskRiskOrders(ctx, taskID)
		requireOK(t, err)
		var plans []core.RiskOrder
		for _, order := range orders {
			if order.Subject.PlanVersion > 0 {
				plans = append(plans, order)
			}
		}
		return plans
	}
	requireOrders := func(t *testing.T, taskID string, versions ...int) []core.RiskOrder {
		t.Helper()
		orders := planOrders(t, taskID)
		if len(orders) != len(versions) {
			t.Fatalf("plan risk orders=%+v, want versions %v", orders, versions)
		}
		for i, version := range versions {
			if orders[i].Subject.PlanVersion != version || orders[i].Order.Stage != core.StageRisk || orders[i].Order.TaskID != taskID {
				t.Fatalf("risk order %d=%+v, want plan version %d", i, orders[i], version)
			}
		}
		return orders
	}
	orderState := func(t *testing.T, id string) core.WorkOrder {
		t.Helper()
		order, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		return order
	}
	requireCancelled := func(t *testing.T, id, reason string) {
		t.Helper()
		if order := orderState(t, id); order.State != core.WorkOrderCancelled || order.RetrySuppressionReason != reason {
			t.Fatalf("risk order %s=%s/%q, want cancelled for %q", id, order.State, order.RetrySuppressionReason, reason)
		}
	}
	setPlanState := func(t *testing.T, taskID string, to core.PlanState) {
		t.Helper()
		_, err := taskops.ExecuteSetupChange(ctx, st, taskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
			return st.ChangeTaskPolicyCommand(ctx, lease, store.SetupChangeRequest{TaskID: taskID, RequestID: core.NewTaskID(), Reason: "operator plan act", Policy: &store.TaskPolicyChange{PlanState: &to}})
		})
		requireOK(t, err)
	}
	attach := func(t *testing.T, taskID, requestID string) store.AttachPlanResult {
		t.Helper()
		result, err := taskops.ExecuteSetupChange(ctx, st, taskID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
			return st.AttachTaskPlanCommand(ctx, lease, store.AttachPlanRequest{TaskID: taskID, RequestID: requestID, Reason: "operator plan", Plan: plan})
		})
		requireOK(t, err)
		return result
	}
	rate := func(t *testing.T, taskID, orderID, session string, tier riskpolicy.Tier) {
		t.Helper()
		_, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{ClaimantID: "run:plan-risk", SessionID: session, ClientToken: session + "-token",
			Agent: "claude-code", Model: "risk-model", Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		_, err = taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
			return st.CompleteRiskOrderCommand(ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: orderID,
				Claim: core.WorkOrderClaimIdentity{ClaimantID: "run:plan-risk", SessionID: session}, Tier: tier, Reason: "Rated from the plan."})
		})
		requireOK(t, err)
	}
	tier := func(t *testing.T, taskID string) riskpolicy.Tier {
		t.Helper()
		ratings, err := st.ListTaskRiskRatings(ctx, taskID)
		requireOK(t, err)
		if setter, ok := core.RiskTierOf(ratings); ok {
			return setter.Tier
		}
		return ""
	}

	t.Run("SubmissionCreatesOneOrderPerVersion", func(t *testing.T) {
		for _, mode := range []riskpolicy.Mode{riskpolicy.ModeRecord, riskpolicy.ModeEnforce} {
			task := planningTask(t, mode)
			mustSubmit(t, task.ID)
			orders := requireOrders(t, task.ID, 1)
			if orders[0].Order.State != core.WorkOrderQueued || !orders[0].Order.Claimable {
				t.Fatalf("%s: risk order=%+v, want queued and claimable", mode, orders[0])
			}
			latest, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
			requireOK(t, err)
			if !ok || latest.Version != 1 || latest.Content != plan || latest.Origin != core.PlanOriginPlanOrder || latest.Approved {
				t.Fatalf("%s: plan version=%+v ok=%t", mode, latest, ok)
			}
			if ratings, err := st.ListTaskRiskRatings(ctx, task.ID); err != nil || len(ratings) != 0 {
				t.Fatalf("%s: plan text recorded ratings %+v err=%v", mode, ratings, err)
			}
			// A replayed submission is refused by the lifecycle and records
			// no second version and no second order.
			if _, err := submit(task.ID); err == nil {
				t.Fatalf("%s: a second submission at the gate was accepted", mode)
			}
			requireOrders(t, task.ID, 1)
			if latest, _, _ := st.GetLatestSpecVersion(ctx, task.ID); latest.Version != 1 {
				t.Fatalf("%s: a refused submission recorded version %d", mode, latest.Version)
			}
		}
	})

	t.Run("RefusedSubmissionRecordsNothing", func(t *testing.T) {
		// The lifecycle refuses gate.spec from a parked task, so the
		// command's transaction must roll back the version and the order.
		if _, err := core.TransitionTask(core.TaskParked, core.TaskGateSpec); err == nil {
			t.Fatal("the lifecycle admits gate.spec from parked; choose another refused state")
		}
		task := newTask(riskpolicy.ModeEnforce, core.TaskParked, core.StageSpec)
		requireOK(t, st.CreateTask(ctx, task))
		before, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		if _, err := submit(task.ID); err == nil {
			t.Fatal("a submission from a parked task was accepted")
		}
		if _, ok, err := st.GetLatestSpecVersion(ctx, task.ID); err != nil || ok {
			t.Fatalf("a refused submission recorded a plan version: ok=%t err=%v", ok, err)
		}
		requireOrders(t, task.ID)
		after, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		if len(after) != len(before) {
			t.Fatalf("a refused submission appended events: %d -> %d", len(before), len(after))
		}
	})

	t.Run("OffAndHighCreateNone", func(t *testing.T) {
		off := planningTask(t, "")
		mustSubmit(t, off.ID)
		requireOrders(t, off.ID)
		offMode := planningTask(t, riskpolicy.ModeOff)
		mustSubmit(t, offMode.ID)
		requireOrders(t, offMode.ID)
		high := planningTask(t, riskpolicy.ModeEnforce)
		_, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: high.ID, Source: core.RiskSourcePerson, Tier: riskpolicy.TierHigh, Reason: "Operator raised it.", ActorID: "user:operator"}, store.RiskSeatSettings{})
		requireOK(t, err)
		mustSubmit(t, high.ID)
		requireOrders(t, high.ID)
		// With no order, enforce approval does not wait.
		requireOK(t, st.ApproveSpecVersion(ctx, high.ID, 1))
	})

	t.Run("IntakeCreatesOrderUnlessApproved", func(t *testing.T) {
		for _, mode := range []riskpolicy.Mode{riskpolicy.ModeRecord, riskpolicy.ModeEnforce} {
			pending := newTask(mode, core.TaskQueued, core.StageTriage)
			pending.PlanState = ""
			pending.IntakePlan = &core.IntakePlan{Content: plan}
			requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, pending, nil, store.TaskContextInput{}))
			if orders := requireOrders(t, pending.ID, core.IntakePlanVersion); orders[0].Order.State != core.WorkOrderQueued {
				t.Fatalf("%s: intake risk order=%+v", mode, orders[0])
			}
			approved := newTask(mode, core.TaskQueued, core.StageTriage)
			approved.PlanState = ""
			approved.IntakePlan = &core.IntakePlan{Content: plan, Approved: true}
			requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, approved, nil, store.TaskContextInput{}))
			requireOrders(t, approved.ID)
		}
		off := newTask(riskpolicy.ModeOff, core.TaskQueued, core.StageTriage)
		off.PlanState = ""
		off.IntakePlan = &core.IntakePlan{Content: plan}
		requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, off, nil, store.TaskContextInput{}))
		requireOrders(t, off.ID)
	})

	t.Run("AttachCreatesOrderAndReplaysWithoutDuplicate", func(t *testing.T) {
		for _, next := range []core.Stage{core.StageTriage, core.StageSpec} {
			task := newTask(riskpolicy.ModeEnforce, core.TaskQueued, next)
			requireOK(t, st.CreateTask(ctx, task))
			first := attach(t, task.ID, "attach-"+task.ID)
			if first.PlanVersion != 1 || first.Task.PlanState != core.PlanAwaitingApproval {
				t.Fatalf("attach from %s=%+v", next, first)
			}
			requireOrders(t, task.ID, 1)
			replay := attach(t, task.ID, "attach-"+task.ID)
			if replay.PlanVersion != 1 {
				t.Fatalf("attach replay=%+v", replay)
			}
			requireOrders(t, task.ID, 1)
		}
	})

	t.Run("RedirectCancelsTheOpenOrder", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeRecord)
		mustSubmit(t, task.ID)
		order := requireOrders(t, task.ID, 1)[0]
		// A claimed order is cancelled too, and its claim ends.
		_, err := ClaimWorkOrder(ctx, st, order.Order.ID, core.WorkOrderClaim{ClaimantID: "run:plan-risk", SessionID: "redirect-risk", ClientToken: "redirect-risk-token",
			Agent: "claude-code", Model: "risk-model", Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		_, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskInterventionRedirect, NextStage: core.StageSpec, ProjectStages: true,
			PlanState: &taskops.PlanStateMove{To: core.PlanNeeded, Cause: core.PlanCausePlanRedirected}})
		requireOK(t, err)
		requireCancelled(t, order.Order.ID, store.PlanRiskRedirectedReason)
		if cancelled := orderState(t, order.Order.ID); cancelled.SessionID != "" {
			t.Fatalf("cancelled risk order kept its claim: %+v", cancelled)
		}
	})

	t.Run("OperatorExitCancelsTheOpenOrder", func(t *testing.T) {
		for _, to := range []core.PlanState{core.PlanNeeded, core.PlanNotRequired} {
			task := planningTask(t, riskpolicy.ModeEnforce)
			mustSubmit(t, task.ID)
			order := requireOrders(t, task.ID, 1)[0]
			setPlanState(t, task.ID, to)
			requireCancelled(t, order.Order.ID, store.PlanRiskOperatorExitReason)
		}
	})

	t.Run("RecordApprovalKeepsTheOrderAndALaterVersionReplacesIt", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeRecord)
		mustSubmit(t, task.ID)
		first := requireOrders(t, task.ID, 1)[0]
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, 1))
		if order := orderState(t, first.Order.ID); order.State != core.WorkOrderQueued {
			t.Fatalf("record approval ended the risk order: %+v", order)
		}
		// Leaving planning completed cancels nothing; the later version that
		// enters plan awaiting approval replaces the earlier one.
		setPlanState(t, task.ID, core.PlanNeeded)
		if order := orderState(t, first.Order.ID); order.State != core.WorkOrderQueued {
			t.Fatalf("leaving planning completed ended the risk order: %+v", order)
		}
		attach(t, task.ID, "replace-"+task.ID)
		orders := requireOrders(t, task.ID, 1, 2)
		requireCancelled(t, first.Order.ID, store.PlanRiskReplacedReason)
		if orders[1].Order.State != core.WorkOrderQueued {
			t.Fatalf("version 2 risk order=%+v", orders[1])
		}
	})

	t.Run("EnforceApprovalWaitsForTheOrder", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeEnforce)
		mustSubmit(t, task.ID)
		order := requireOrders(t, task.ID, 1)[0]
		refuse := func(t *testing.T, state core.WorkOrderState) {
			t.Helper()
			before, err := st.ListEvents(ctx, task.ID)
			requireOK(t, err)
			err = st.ApproveSpecVersion(ctx, task.ID, 1)
			var refusal *store.PlanRiskRatingOpenError
			if !errors.As(err, &refusal) || refusal.WorkOrderID != order.Order.ID || refusal.PlanVersion != 1 || refusal.State != state || !errors.Is(err, store.ErrPlanRiskRatingOpen) {
				t.Fatalf("approval with a %s risk order: %v", state, err)
			}
			after, err := st.ListEvents(ctx, task.ID)
			requireOK(t, err)
			current, err := st.GetTask(ctx, task.ID)
			requireOK(t, err)
			if _, approved, _ := st.GetApprovedSpecVersion(ctx, task.ID); approved || len(after) != len(before) || current.PlanState != core.PlanAwaitingApproval {
				t.Fatalf("refused approval changed the task: approved=%t events %d -> %d plan %s", approved, len(before), len(after), current.PlanState)
			}
		}
		refuse(t, core.WorkOrderQueued)
		session := "enforce-risk"
		_, err := ClaimWorkOrder(ctx, st, order.Order.ID, core.WorkOrderClaim{ClaimantID: "run:plan-risk", SessionID: session, ClientToken: session + "-token",
			Agent: "claude-code", Model: "risk-model", Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		refuse(t, core.WorkOrderClaimed)
		_, err = taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
			return st.CompleteRiskOrderCommand(ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: order.Order.ID,
				Claim: core.WorkOrderClaimIdentity{ClaimantID: "run:plan-risk", SessionID: session}, Tier: riskpolicy.TierMedium, Reason: "Rated from the plan."})
		})
		requireOK(t, err)
		// Rated before approval: the rating waits for the approval to count.
		if got := tier(t, task.ID); got != "" {
			t.Fatalf("an unapproved version set the tier %s", got)
		}
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, 1))
		if got := tier(t, task.ID); got != riskpolicy.TierMedium {
			t.Fatalf("tier after approval=%q, want medium", got)
		}

		// Cancellation also ends the wait.
		cancelled := planningTask(t, riskpolicy.ModeEnforce)
		mustSubmit(t, cancelled.ID)
		open := requireOrders(t, cancelled.ID, 1)[0]
		_, err = taskops.ExecuteWorkOrder(ctx, st, cancelled.ID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			order, _, cancelErr := st.CancelRiskOrderCommand(ctx, lease, store.RiskOrderCancelRequest{WorkOrderID: open.Order.ID, Reason: "operator ended the wait"})
			return order, cancelErr
		})
		requireOK(t, err)
		requireOK(t, st.ApproveSpecVersion(ctx, cancelled.ID, 1))
	})

	t.Run("RatingCountsWhenApprovedInEitherOrder", func(t *testing.T) {
		// Approval before rating, under record.
		task := planningTask(t, riskpolicy.ModeRecord)
		mustSubmit(t, task.ID)
		order := requireOrders(t, task.ID, 1)[0]
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, 1))
		if got := tier(t, task.ID); got != "" {
			t.Fatalf("tier before the rating=%q", got)
		}
		rate(t, task.ID, order.Order.ID, "after-approval", riskpolicy.TierMedium)
		if got := tier(t, task.ID); got != riskpolicy.TierMedium {
			t.Fatalf("tier after a rating of an approved version=%q, want medium", got)
		}
		// A later version rated lower and approved never lowers it.
		setPlanState(t, task.ID, core.PlanNeeded)
		attach(t, task.ID, "lower-"+task.ID)
		second := requireOrders(t, task.ID, 1, 2)[1]
		rate(t, task.ID, second.Order.ID, "lower-version", riskpolicy.TierLow)
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, 2))
		if got := tier(t, task.ID); got != riskpolicy.TierMedium {
			t.Fatalf("tier after a lower later version=%q, want medium", got)
		}
		// An unapproved version's rating never sets the minimum.
		unapproved := planningTask(t, riskpolicy.ModeRecord)
		mustSubmit(t, unapproved.ID)
		rate(t, unapproved.ID, requireOrders(t, unapproved.ID, 1)[0].Order.ID, "unapproved", riskpolicy.TierHigh)
		if got := tier(t, unapproved.ID); got != "" {
			t.Fatalf("an unapproved version set the tier %s", got)
		}
	})
}
