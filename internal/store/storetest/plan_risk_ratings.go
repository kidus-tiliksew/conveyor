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

// planRiskPlan is a plan whose Risks section holds a line in the retired
// tier format, or no such line when tier is empty.
func planRiskPlan(tier riskpolicy.Tier, reason string) string {
	risks := "- None."
	if tier != "" {
		risks = "- None.\n\nRisk tier: " + string(tier) + " — " + reason
	}
	return "## Approach\nRate the plan.\n\n## Files touched\n- internal/store/risk.go\n\n## Ordering\n1. Parse, then rate.\n\n## Risks\n" + risks + "\n\n## Done criteria\n- The plan is rated."
}

// runPlanRiskRatings proves on every backend that no plan text records a
// rating: approving a version, or filing an approved intake plan, whose text
// holds a tier line records nothing, and a rating recorded earlier from a
// plan's tier line stays readable with source plan and keeps counting toward
// the task's tier, which a later approval never lowers. Approvals and path
// ratings still serialize under the task lock (req-risk-based-review AC-4.3,
// AC-4.6, AC-5.5, AC-5.6, AC-1.1, AC-2.2; feature-risk-based-review RR-13;
// component-risk-review invariants 4 and 8).
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

	t.Run("ApprovalRecordsNoRatingFromPlanText", func(t *testing.T) {
		for _, policy := range []*riskpolicy.Policy{&record, &enforce, nil} {
			task := fileTask(t, policy)
			plan := submit(t, task.ID, planRiskPlan(riskpolicy.TierHigh, "Approval touches three stores."))
			requireOK(t, st.ApproveSpecVersion(ctx, task.ID, plan.Version))
			requireOK(t, st.ApproveSpecVersion(ctx, task.ID, plan.Version))
			if listed := ratings(t, task.ID); len(listed) != 0 {
				t.Fatalf("approval of a plan with a tier line recorded %+v", listed)
			}
			if events := ratedEvents(t, task.ID); events != 0 {
				t.Fatalf("risk events=%d, want 0", events)
			}
			if state := planState(t, task.ID); state != core.PlanCompleted {
				t.Fatalf("plan state=%s, want completed from the approval alone", state)
			}
		}
	})

	t.Run("RecordedPlanRatingsKeepCounting", func(t *testing.T) {
		task := fileTask(t, &enforce)
		path, created, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: task.ID, Key: core.PathRiskKey("head-1"), Source: core.RiskSourcePathRule,
			Tier: riskpolicy.TierMedium, HeadSHA: "head-1", BaseSHA: "base", DeclarationState: riskpolicy.DeclarationAbsent, Reason: "matched path rules"})
		requireOK(t, err)
		if !created {
			t.Fatal("path rating not recorded")
		}
		first := submit(t, task.ID, planRiskPlan(riskpolicy.TierHigh, "First plan is high."))
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, first.Version))
		// A rating recorded from a plan's tier line before risk orders existed
		// stays readable with source plan and keeps counting (AC-5.6).
		historical, created, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: task.ID, Key: core.PlanRiskKey(first.Version), Source: core.RiskSourcePlan,
			Tier: riskpolicy.TierHigh, PlanVersion: first.Version, Reason: "First plan is high."})
		requireOK(t, err)
		if !created {
			t.Fatal("historical plan rating not recorded")
		}
		second := submit(t, task.ID, planRiskPlan(riskpolicy.TierLow, "Second plan is low."))
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, second.Version))
		listed := ratings(t, task.ID)
		if len(listed) != 2 || listed[0].Key != path.Key || listed[1].Key != historical.Key || listed[1].Source != core.RiskSourcePlan || listed[1].PlanPending {
			t.Fatalf("ratings=%+v, want the path rating and the readable plan rating", listed)
		}
		setter, ok := core.RiskTierOf(listed)
		if !ok || setter.Tier != riskpolicy.TierHigh || setter.Source != core.RiskSourcePlan || setter.PlanVersion != first.Version {
			t.Fatalf("task tier set by %+v, want the recorded plan rating's high", setter)
		}
		projection := store.ProjectTaskRisk(task, listed, 1)
		if projection == nil || projection.Tier != riskpolicy.TierHigh || projection.Source != core.RiskSourcePlan {
			t.Fatalf("projection=%+v", projection)
		}
	})

	t.Run("ApprovedIntakePlanRecordsNothing", func(t *testing.T) {
		approved := newTask(&record)
		approved.PlanState = ""
		approved.IntakePlan = &core.IntakePlan{Content: planRiskPlan(riskpolicy.TierMedium, "Filed approved."), Approved: true}
		requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, approved, nil, store.TaskContextInput{}))
		if listed := ratings(t, approved.ID); len(listed) != 0 {
			t.Fatalf("approved intake plan ratings=%+v", listed)
		}
		if events := ratedEvents(t, approved.ID); events != 0 {
			t.Fatalf("approved intake risk events=%d", events)
		}
		if state := planState(t, approved.ID); state != core.PlanCompleted {
			t.Fatalf("approved intake plan state=%s", state)
		}
	})

	// Concurrent approvals of one version and concurrent path ratings
	// serialize under the task lock on every backend: each write succeeds,
	// the plan text records no rating, and sequences run 1..n with one event
	// each.
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
			if len(listed) != paths {
				t.Fatalf("round %d: ratings=%+v, want %d path ratings", round, listed, paths)
			}
			for i, rating := range listed {
				if rating.Seq != i+1 || rating.Source != core.RiskSourcePathRule {
					t.Fatalf("round %d: rating %d = %+v", round, i, rating)
				}
			}
			if events := ratedEvents(t, task.ID); events != len(listed) {
				t.Fatalf("round %d: %d events for %d ratings", round, events, len(listed))
			}
			if state := planState(t, task.ID); state != core.PlanCompleted {
				t.Fatalf("round %d: plan state=%s", round, state)
			}
		}
	})
}
