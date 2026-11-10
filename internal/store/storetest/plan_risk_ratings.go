package storetest

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// planRiskPlan is a plan whose Risks section states tier with reason, or no
// tier when tier is empty.
func planRiskPlan(tier riskpolicy.Tier, reason string) string {
	risks := "- None."
	if tier != "" {
		risks = "- None.\n\nRisk tier: " + string(tier) + " — " + reason
	}
	return "## Approach\nRate the plan.\n\n## Files touched\n- internal/store/risk.go\n\n## Ordering\n1. Parse, then rate.\n\n## Risks\n" + risks + "\n\n## Done criteria\n- The plan is rated."
}

// runPlanRiskRatings proves on every backend that a plan version's stated
// tier becomes a rating with source plan in the transaction that approves
// it: once per version, never for an unapproved version or a task whose
// frozen risk mode is off, never lowered by a later lower plan, and serialized
// with the task's other rating writes under concurrency
// (req-risk-based-review AC-4.3, AC-4.5, AC-4.6, AC-1.1, AC-2.2;
// feature-risk-based-review RR-4; component-risk-review invariants 4 and 8).
func runPlanRiskRatings(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	enforce := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}
	person := core.AuthenticatedCredential{ID: "pat_plan_risk", OwnerUserID: "usr_plan_risk_filer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	personCtx := store.WithActor(store.WithCredential(ctx, person), store.Actor{ID: store.UserActorID(person.OwnerUserID), Role: core.ActorUser})
	repo := "conveyor"
	if x.Config != nil && len(x.Config.Repos) > 0 {
		repo = x.Config.Repos[0].Name
	}
	newTask := func(policy *riskpolicy.Policy) core.Task {
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Source: "api", Repo: repo, Title: id, Body: "Plan risk " + id, BaseBranch: "main",
			Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: core.StageTriage, PlanState: core.PlanNeeded, PolicyVersion: 1,
			CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: policy}
		return task
	}
	fileTask := func(t *testing.T, policy *riskpolicy.Policy) core.Task {
		t.Helper()
		task := newTask(policy)
		requireOK(t, st.CreateTask(ctx, task))
		return task
	}
	submit := func(t *testing.T, taskID, content string) core.SpecVersion {
		t.Helper()
		version, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: content, Acceptance: []byte(`[]`), Decomposition: []byte(`[]`)})
		requireOK(t, err)
		return version
	}
	ratings := func(t *testing.T, taskID string) []core.RiskRating {
		t.Helper()
		listed, err := st.ListTaskRiskRatings(ctx, taskID)
		requireOK(t, err)
		return listed
	}
	planRatings := func(t *testing.T, taskID string) []core.RiskRating {
		t.Helper()
		var plans []core.RiskRating
		for _, rating := range ratings(t, taskID) {
			if rating.Source == core.RiskSourcePlan {
				plans = append(plans, rating)
			}
		}
		return plans
	}
	ratedEvents := func(t *testing.T, taskID string) int {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		count := 0
		for _, event := range events {
			if event.Kind == core.RiskRatedEvent {
				count++
			}
		}
		return count
	}
	planState := func(t *testing.T, taskID string) core.PlanState {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		return task.PlanState
	}

	t.Run("ApprovalRecordsPlanRatingOnce", func(t *testing.T) {
		task := fileTask(t, &record)
		plan := submit(t, task.ID, planRiskPlan(riskpolicy.TierHigh, "Approval touches three stores."))
		if listed := ratings(t, task.ID); len(listed) != 0 {
			t.Fatalf("unapproved plan recorded ratings %+v", listed)
		}
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, plan.Version))
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, plan.Version))
		listed := planRatings(t, task.ID)
		if len(listed) != 1 {
			t.Fatalf("plan ratings=%+v, want one after a repeated approval", listed)
		}
		got := listed[0]
		if got.Key != core.PlanRiskKey(plan.Version) || got.PlanVersion != plan.Version || got.Tier != riskpolicy.TierHigh ||
			got.Reason != "Approval touches three stores." || got.Seq != 1 || got.CreatedAt.IsZero() {
			t.Fatalf("plan rating=%+v", got)
		}
		if events := ratedEvents(t, task.ID); events != 1 {
			t.Fatalf("risk events=%d, want 1", events)
		}
		if state := planState(t, task.ID); state != core.PlanCompleted {
			t.Fatalf("plan state=%s, want completed from the approval alone", state)
		}
	})

	t.Run("LaterLowerPlanKeepsTheHigherTier", func(t *testing.T) {
		task := fileTask(t, &enforce)
		path, created, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: task.ID, Key: core.PathRiskKey("head-1"), Source: core.RiskSourcePathRule,
			Tier: riskpolicy.TierMedium, HeadSHA: "head-1", BaseSHA: "base", DeclarationState: riskpolicy.DeclarationAbsent, Reason: "matched path rules"})
		requireOK(t, err)
		if !created {
			t.Fatal("path rating not recorded")
		}
		first := submit(t, task.ID, planRiskPlan(riskpolicy.TierHigh, "First plan is high."))
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, first.Version))
		second := submit(t, task.ID, planRiskPlan(riskpolicy.TierLow, "Second plan is low."))
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, second.Version))
		unapproved := submit(t, task.ID, planRiskPlan(riskpolicy.TierMedium, "Never approved."))
		listed := ratings(t, task.ID)
		if len(listed) != 3 || listed[0].Key != path.Key || listed[1].Key != core.PlanRiskKey(first.Version) || listed[2].Key != core.PlanRiskKey(second.Version) {
			t.Fatalf("ratings=%+v, want the path rating and both approved plans", listed)
		}
		for _, rating := range listed {
			if rating.Key == core.PlanRiskKey(unapproved.Version) {
				t.Fatalf("unapproved version %d recorded %+v", unapproved.Version, rating)
			}
		}
		setter, ok := core.RiskTierOf(listed)
		if !ok || setter.Tier != riskpolicy.TierHigh || setter.Source != core.RiskSourcePlan || setter.PlanVersion != first.Version {
			t.Fatalf("task tier set by %+v, want the first approved plan's high", setter)
		}
		var planTiers []riskpolicy.Tier
		for _, rating := range listed {
			if rating.Source == core.RiskSourcePlan {
				planTiers = append(planTiers, rating.Tier)
			}
		}
		if tier := riskpolicy.TaskTier(riskpolicy.TierInput{PathMinimum: path.Tier, PlanTiers: planTiers}); tier != riskpolicy.TierHigh {
			t.Fatalf("TaskTier=%s, want high", tier)
		}
		projection := store.ProjectTaskRisk(task, listed, 1)
		if projection == nil || projection.Tier != riskpolicy.TierHigh || projection.Source != core.RiskSourcePlan {
			t.Fatalf("projection=%+v", projection)
		}
	})

	t.Run("PlanWithoutTierAndOffTaskRecordNothing", func(t *testing.T) {
		untiered := fileTask(t, &record)
		plan := submit(t, untiered.ID, planRiskPlan("", ""))
		requireOK(t, st.ApproveSpecVersion(ctx, untiered.ID, plan.Version))
		if listed := ratings(t, untiered.ID); len(listed) != 0 {
			t.Fatalf("plan with no tier recorded %+v", listed)
		}
		if state := planState(t, untiered.ID); state != core.PlanCompleted {
			t.Fatalf("plan state=%s, want completed", state)
		}
		off := fileTask(t, nil)
		offPlan := submit(t, off.ID, planRiskPlan(riskpolicy.TierHigh, "Off task."))
		requireOK(t, st.ApproveSpecVersion(ctx, off.ID, offPlan.Version))
		if listed := ratings(t, off.ID); len(listed) != 0 {
			t.Fatalf("off task recorded %+v", listed)
		}
		if events := ratedEvents(t, off.ID); events != 0 {
			t.Fatalf("off task risk events=%d", events)
		}
	})

	t.Run("ApprovedIntakePlanRatedInCreation", func(t *testing.T) {
		approved := newTask(&record)
		approved.PlanState = ""
		approved.IntakePlan = &core.IntakePlan{Content: planRiskPlan(riskpolicy.TierMedium, "Filed approved."), Approved: true}
		requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, approved, nil, store.TaskContextInput{}))
		listed := ratings(t, approved.ID)
		if len(listed) != 1 || listed[0].Key != core.PlanRiskKey(core.IntakePlanVersion) || listed[0].Tier != riskpolicy.TierMedium ||
			listed[0].Reason != "Filed approved." || listed[0].PlanVersion != core.IntakePlanVersion {
			t.Fatalf("approved intake plan ratings=%+v", listed)
		}
		if events := ratedEvents(t, approved.ID); events != 1 {
			t.Fatalf("approved intake risk events=%d", events)
		}
		pending := newTask(&record)
		pending.PlanState = ""
		pending.IntakePlan = &core.IntakePlan{Content: planRiskPlan(riskpolicy.TierHigh, "Filed for approval.")}
		requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, pending, nil, store.TaskContextInput{}))
		if listed := ratings(t, pending.ID); len(listed) != 0 {
			t.Fatalf("unapproved intake plan recorded %+v", listed)
		}
		requireOK(t, st.ApproveSpecVersion(ctx, pending.ID, core.IntakePlanVersion))
		if listed := planRatings(t, pending.ID); len(listed) != 1 || listed[0].Tier != riskpolicy.TierHigh {
			t.Fatalf("gate-approved intake plan ratings=%+v", listed)
		}
	})

	// Concurrent approvals of one version and concurrent path ratings
	// serialize under the task lock on every backend: each write succeeds,
	// the version is rated once, and sequences run 1..n with one event each.
	t.Run("ConcurrentApprovalsAndRatingsSerialize", func(t *testing.T) {
		const rounds, approvers, paths = 10, 4, 4
		for round := range rounds {
			task := fileTask(t, &record)
			plan := submit(t, task.ID, planRiskPlan(riskpolicy.TierMedium, fmt.Sprintf("Round %d plan.", round)))
			start := make(chan struct{})
			results := make(chan error, approvers+paths)
			var wg sync.WaitGroup
			for range approvers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results <- st.ApproveSpecVersion(ctx, task.ID, plan.Version)
				}()
			}
			for i := range paths {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					head := fmt.Sprintf("head-%d-%d", round, i)
					_, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: task.ID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule,
						Tier: riskpolicy.TierLow, HeadSHA: head, BaseSHA: "base", DeclarationState: riskpolicy.DeclarationAbsent, Reason: "no sensitive path rule matched"})
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatalf("round %d: concurrent write err=%v", round, err)
				}
			}
			listed := ratings(t, task.ID)
			if len(listed) != 1+paths {
				t.Fatalf("round %d: ratings=%+v, want one plan rating and %d path ratings", round, listed, paths)
			}
			plans := 0
			for i, rating := range listed {
				if rating.Seq != i+1 {
					t.Fatalf("round %d: rating %d has seq %d", round, i, rating.Seq)
				}
				if rating.Source == core.RiskSourcePlan {
					plans++
				}
			}
			if plans != 1 {
				t.Fatalf("round %d: %d plan ratings", round, plans)
			}
			if events := ratedEvents(t, task.ID); events != len(listed) {
				t.Fatalf("round %d: %d events for %d ratings", round, events, len(listed))
			}
		}
	})
}
