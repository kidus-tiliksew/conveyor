package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Tests for the merge hold of enforcing tasks (req-risk-based-review AC-8.1,
// AC-8.2, AC-8.7; req-review-gates-evidence AC-1.1, AC-1.3;
// feature-risk-based-review RR-6).

type riskMergeCase struct {
	mode      riskpolicy.Mode
	tier      riskpolicy.Tier
	approvals int
}

func riskPolicyFor(mode riskpolicy.Mode) *riskpolicy.Policy {
	return &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}
}

// riskGateFixture is gateMergeFixture for a gate-off task whose frozen risk
// policy is mode, rated tier with a path minimum of approvals.
func riskGateFixture(t *testing.T, id string, tc riskMergeCase) (context.Context, store.Store, core.Task, *Dispatcher, *gateForge) {
	t.Helper()
	ctx := store.WithActor(forgeReconcileCtx(), gateOperator)
	st := mergeIdentityStore{store.NewMemory()}
	task := core.Task{ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskRunning, NextStage: core.StageReview,
		ReviewedHeadSHA: gateHead, PolicyVersion: 1, CreatedAt: time.Now().UTC(), SetupContract: config.PolicyContract{Risk: riskPolicyFor(tc.mode)}}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	recordPathRating(t, st, ctx, id, tc)
	out, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil || out.Task.State != core.TaskAwaiting || out.Task.GateKind != core.GateKindMergeApproval {
		t.Fatalf("gate task=%+v err=%v", out.Task, err)
	}
	openPullRequest(t, st, ctx, id, 7, gateHead)
	d := newForgeDispatcher(st)
	forge := &gateForge{pr: githubtrigger.PullRequest{Number: 7, State: "open", Mergeable: "MERGEABLE", HeadSHA: gateHead, HeadRef: task.Branch, URL: "https://github.test/acme/app/pull/7"}}
	d.ViewPullRequest, d.RequestMerge = forge.view, forge.merge
	return ctx, st, out.Task, d, forge
}

func recordPathRating(t *testing.T, st store.Store, ctx context.Context, id string, tc riskMergeCase) {
	t.Helper()
	if tc.mode == riskpolicy.ModeOff {
		return
	}
	if _, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: id, Key: core.PathRiskKey(gateHead), Source: core.RiskSourcePathRule, Tier: tc.tier,
		HeadSHA: gateHead, BaseSHA: "base", DeclarationState: riskpolicy.DeclarationPresent, Reason: "path rule", HumanApprovals: tc.approvals}); err != nil {
		t.Fatal(err)
	}
}

func TestMergeAtGateRefusesASingleMergeWhenTwoApprovalsAreRequired(t *testing.T) {
	for _, tc := range []struct {
		name string
		riskMergeCase
		required int
	}{
		{"HighTier", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierHigh, 0}, 2},
		{"PathRuleMinimum", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 3}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, task, d, forge := riskGateFixture(t, "risk-gate-"+tc.name, tc.riskMergeCase)
			before := len(eventKinds(t, st, ctx, task.ID))
			for attempt := 0; attempt < 2; attempt++ {
				_, err := d.MergeAtGate(ctx, task, gateOperator)
				var refusal *store.RiskApprovalsRequiredError
				if !errors.As(err, &refusal) || refusal.Required != tc.required || refusal.Automatic || !errors.Is(err, store.ErrRiskApprovalsRequired) {
					t.Fatalf("attempt %d err=%v, want a refusal naming %d approvals", attempt, err, tc.required)
				}
				// The route answers the refusal with its text (409), never a
				// store failure.
				if !MergeRouteRefusal(err) {
					t.Fatalf("MergeRouteRefusal(%v)=false", err)
				}
			}
			assertGatePending(t, st, ctx, task.ID)
			if after := len(eventKinds(t, st, ctx, task.ID)); after != before || forge.merges != 0 {
				t.Fatalf("refusals appended %d events and requested %d merges, want none", after-before, forge.merges)
			}
		})
	}
}

func TestMergeAtGateKeepsTheSingleActBelowTwoApprovals(t *testing.T) {
	for name, tc := range map[string]riskMergeCase{
		"EnforceMediumNeedsOne": {riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0},
		"EnforcePathRuleOne":    {riskpolicy.ModeEnforce, riskpolicy.TierLow, 1},
		"RecordHigh":            {riskpolicy.ModeRecord, riskpolicy.TierHigh, 2},
		"Off":                   {riskpolicy.ModeOff, "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, st, task, d, forge := riskGateFixture(t, "risk-gate-one-"+name, tc)
			out, err := d.MergeAtGate(ctx, task, gateOperator)
			if err != nil || out.State != core.TaskMerged || forge.merges != 1 {
				t.Fatalf("out=%+v err=%v merges=%d", out, err, forge.merges)
			}
			assertGateMergedBy(t, st, ctx, task.ID, "usr-approver")
		})
	}
}

// riskApprovedTask files a gate-off task already approved at gateHead.
func riskApprovedTask(t *testing.T, id string, tc riskMergeCase) (context.Context, store.Store, core.Task, *Dispatcher, *gateForge) {
	t.Helper()
	ctx := store.WithActor(forgeReconcileCtx(), gateOperator)
	st := mergeIdentityStore{store.NewMemory()}
	task := core.Task{ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskApproved,
		ReviewedHeadSHA: gateHead, ApprovedHeadSHA: gateHead, PolicyVersion: 1, CreatedAt: time.Now().UTC(), SetupContract: config.PolicyContract{Risk: riskPolicyFor(tc.mode)}}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	recordPathRating(t, st, ctx, id, tc)
	stored, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	openPullRequest(t, st, ctx, id, 7, gateHead)
	d := newForgeDispatcher(st)
	forge := &gateForge{pr: githubtrigger.PullRequest{Number: 7, State: "open", Mergeable: "MERGEABLE", HeadSHA: gateHead, HeadRef: task.Branch, URL: "https://github.test/acme/app/pull/7"}}
	d.ViewPullRequest, d.RequestMerge = forge.view, forge.merge
	return ctx, st, stored, d, forge
}

// A tier raised after acceptance holds every automatic merge: the merge is
// refused as ineligible, so no retry merges past it, and nothing merges.
func TestAutomaticMergeHoldsAnEnforcingTaskThatNeedsAPerson(t *testing.T) {
	for name, tc := range map[string]riskMergeCase{
		"Medium":          {riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0},
		"PathRuleMinimum": {riskpolicy.ModeEnforce, riskpolicy.TierLow, 1},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, st, task, d, forge := riskApprovedTask(t, "risk-auto-"+name, tc)
			err := d.MergeApprovedTaskAutomatically(ctx, task)
			var refusal *store.RiskApprovalsRequiredError
			if !errors.As(err, &refusal) || !refusal.Automatic || !errors.Is(err, errMergeIneligible) || retryableMergeError(err) {
				t.Fatalf("automatic merge err=%v, want a non-retryable risk hold", err)
			}
			if forge.merges != 0 {
				t.Fatalf("merges=%d, want 0", forge.merges)
			}
			if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskApproved {
				t.Fatalf("task state=%s", current.State)
			}
			// The gate-off readiness pass treats the hold as handled.
			if changed, err := d.gateOffMergeReadiness(ctx, task); err != nil || changed {
				t.Fatalf("readiness changed=%t err=%v", changed, err)
			}
		})
	}
}

// An operator's merge of an approved enforcing task is one person's act: it
// is refused when two or more approvals are required, and not held by risk
// at a count of one.
func TestOperatorMergeOfApprovedEnforcingTask(t *testing.T) {
	ctx, st, task, d, forge := riskApprovedTask(t, "risk-operator-high", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierHigh, 0})
	before := len(eventKinds(t, st, ctx, task.ID))
	err := d.MergeApprovedTask(ctx, task, gateOperator)
	var refusal *store.RiskApprovalsRequiredError
	if !errors.As(err, &refusal) || refusal.Required != 2 || refusal.Automatic || !MergeRouteRefusal(err) {
		t.Fatalf("operator merge err=%v, want a refusal naming 2 approvals", err)
	}
	if after := len(eventKinds(t, st, ctx, task.ID)); after != before || forge.merges != 0 {
		t.Fatalf("refusal appended %d events and requested %d merges", after-before, forge.merges)
	}

	ctx, _, task, d, _ = riskApprovedTask(t, "risk-operator-medium", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	if err := d.MergeApprovedTask(ctx, task, gateOperator); errors.Is(err, store.ErrRiskApprovalsRequired) {
		t.Fatalf("operator merge at one required approval was held by risk: %v", err)
	}
}

// A gate-off enforcing task held at the merge gate takes the gate-on
// readiness pass, so a merge on GitHub at the reviewed head is reconciled as
// the operator's merge there, and the sweep requests no merge itself.
func TestMergeReadinessReconcilesGateOffTaskAtTheMergeGate(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "risk-gate-sweep", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	if !mergeReadinessCanAct(task) {
		t.Fatal("a gate-off task at the merge gate is not a readiness candidate")
	}
	if _, err := d.ReconcileMergeReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskAwaiting || forge.merges != 0 {
		t.Fatalf("open pull request sweep: state=%s merges=%d", current.State, forge.merges)
	}
	forge.mu.Lock()
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "octocat"
	forge.mu.Unlock()
	if _, err := d.ReconcileMergeReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	if current, err := st.GetTask(ctx, task.ID); err != nil || current.State != core.TaskMerged || forge.merges != 0 {
		t.Fatalf("merged pull request sweep: task=%+v err=%v merges=%d", current, err, forge.merges)
	}
}
