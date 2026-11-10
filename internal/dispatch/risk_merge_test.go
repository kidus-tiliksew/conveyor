package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
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
			// The hold is the operator's: the merge gate is on (DEC-124(2)).
			action := riskNextAction(t, ctx, st, task.ID)
			if action.Actor != nextaction.ActorOperator || action.Kind != nextaction.KindMergeExecution {
				t.Fatalf("held task next action %s/%s, want operator/merge_execution", action.Actor, action.Kind)
			}
			if merge, ok := choiceOf(action, "merge"); !ok || !merge.Allowed {
				t.Fatalf("merge choice=%+v ok=%t", merge, ok)
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

func riskNextAction(t *testing.T, ctx context.Context, st store.Store, id string) nextaction.NextAction {
	t.Helper()
	action, err := store.ReadNextAction(ctx, st, id, store.NextActionOptions{Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true, core.CapabilityRequestChanges: true}})
	if err != nil {
		t.Fatal(err)
	}
	return action
}

func choiceOf(action nextaction.NextAction, id string) (nextaction.Choice, bool) {
	for _, choice := range action.Choices {
		if choice.ID == id {
			return choice, true
		}
	}
	return nextaction.Choice{}, false
}

func raiseTier(t *testing.T, ctx context.Context, st store.Store, id string, tier riskpolicy.Tier) {
	t.Helper()
	// The tier route raises under the task side-effect lock every merge act
	// holds (internal/httpapi/risk.go).
	if err := st.WithTaskSideEffectLock(ctx, id, func(locked context.Context) error {
		_, err := st.RaiseTaskRiskTier(locked, core.RiskRating{TaskID: id, Tier: tier, Reason: "raised to " + string(tier), ActorID: gateOperator.ID}, store.RiskSeatSettings{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A gate-off enforcing task that approved at low and waits on a pending check
// is raised to medium by a person. When the check passes, nothing merges it
// automatically: the merge gate is on (DEC-124(2)), so the next action names
// the operator's merge, and that merge lands the head
// (req-risk-based-review REQ-8, AC-8.2; component-next-action row 7).
func TestRaiseAfterApprovalHandsTheMergeToAnOperator(t *testing.T) {
	ctx, st, task, d, forge := riskApprovedTask(t, "risk-raise-after-approval", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	pending := githubtrigger.CheckStatus{Checks: []githubtrigger.Check{{Name: "e2e", State: githubtrigger.CheckPending}}}
	passed := githubtrigger.CheckStatus{Checks: []githubtrigger.Check{{Name: "e2e", State: githubtrigger.CheckPassed}}}
	checks := pending
	d.ReadCommitChecks = func(context.Context, string, string) (githubtrigger.CheckStatus, error) { return checks, nil }

	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, ErrCheckStatusPending) || forge.merges != 0 {
		t.Fatalf("pending check: err=%v merges=%d", err, forge.merges)
	}
	if action := riskNextAction(t, ctx, st, task.ID); action.Actor != nextaction.ActorFactory || action.Kind != nextaction.KindMerging {
		t.Fatalf("low tier with a pending check: next action %s/%s, want factory/merging", action.Actor, action.Kind)
	}

	raiseTier(t, ctx, st, task.ID, riskpolicy.TierMedium)
	checks = passed
	if err := d.ApplyChecksDelivery(ctx, "acme/app", gateHead, nil); err != nil {
		t.Fatalf("checks delivery: %v", err)
	}
	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, store.ErrRiskApprovalsRequired) {
		t.Fatalf("automatic merge after the raise err=%v, want the risk hold", err)
	}
	if _, err := d.ReconcileMergeReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskApproved || current.MergeApproval || forge.merges != 0 {
		t.Fatalf("held task=%+v merges=%d, want approved, frozen gate off, unmerged", current, forge.merges)
	}
	action := riskNextAction(t, ctx, st, task.ID)
	if action.Actor != nextaction.ActorOperator || action.Kind != nextaction.KindMergeExecution || !action.Judgment {
		t.Fatalf("held task next action %s/%s, want operator/merge_execution", action.Actor, action.Kind)
	}
	merge, ok := choiceOf(action, "merge")
	if !ok || !merge.Allowed || merge.Path != "/v1/tasks/"+task.ID+"/merge" {
		t.Fatalf("merge choice=%+v ok=%t, want an allowed operator merge", merge, ok)
	}
	// The operator's merge is the one approval medium requires.
	if err := d.MergeApprovedTask(ctx, task, gateOperator); err != nil || forge.merges != 1 {
		t.Fatalf("operator merge err=%v merges=%d", err, forge.merges)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskMerged {
		t.Fatalf("task state=%s, want merged", current.State)
	}
}

// At two or more required approvals the held task still names the operator,
// and the merge choice is withheld with the route's reason rather than
// offered and refused (component-next-action invariant 7).
func TestHeldTaskNeedingTwoApprovalsWithholdsTheMerge(t *testing.T) {
	ctx, st, task, _, _ := riskApprovedTask(t, "risk-held-two", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	raiseTier(t, ctx, st, task.ID, riskpolicy.TierHigh)
	action := riskNextAction(t, ctx, st, task.ID)
	if action.Actor != nextaction.ActorOperator || action.Kind != nextaction.KindMergeExecution {
		t.Fatalf("next action %s/%s", action.Actor, action.Kind)
	}
	if _, ok := choiceOf(action, "merge"); ok {
		t.Fatal("the merge choice is offered at two required approvals")
	}
	refused := false
	for _, refusal := range action.Details.Refusals {
		refused = refused || (refusal.Choice == "merge" && strings.Contains(refusal.Text, "2 human approvals"))
	}
	if !refused {
		t.Fatalf("refusals=%+v, want the merge refusal naming 2 approvals", action.Details.Refusals)
	}
	if _, ok := choiceOf(action, "request_changes"); !ok {
		t.Fatal("request changes is not offered")
	}

	// The merge gate of an enforcing task withholds the merge the same way.
	gctx, gst, gate, _, _ := riskGateFixture(t, "risk-gate-two", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierHigh, 0})
	if gateAction := riskNextAction(t, gctx, gst, gate.ID); gateAction.Kind != nextaction.KindMergeApproval {
		t.Fatalf("gate next action kind=%s", gateAction.Kind)
	} else if _, ok := choiceOf(gateAction, "merge"); ok {
		t.Fatal("the merge gate offers the merge at two required approvals")
	}
}

// A raise and a merge act serialize on the task side-effect lock. A merge
// that holds the lock first lands under the tier it read, and the raise
// waits for it. A raise that holds the lock first is read by the merge,
// which then refuses (feature-risk-based-review RR-6).
func TestRaiseAndMergeSerialize(t *testing.T) {
	t.Run("MergeFirst", func(t *testing.T) {
		ctx, st, task, d, forge := riskApprovedTask(t, "risk-serial-merge-first", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
		entered, release := make(chan struct{}), make(chan struct{})
		forge.answer = func(pr *githubtrigger.PullRequest) error {
			close(entered)
			<-release
			pr.Merged, pr.State, pr.MergeCommitSHA, pr.MergedBy = true, "closed", "landed", "conveyor-app"
			return nil
		}
		mergeDone := make(chan error, 1)
		go func() { mergeDone <- d.MergeApprovedTask(ctx, task, gateOperator) }()
		<-entered
		raised := make(chan error, 1)
		go func() {
			raised <- st.WithTaskSideEffectLock(ctx, task.ID, func(locked context.Context) error {
				_, err := st.RaiseTaskRiskTier(locked, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "late raise", ActorID: gateOperator.ID}, store.RiskSeatSettings{})
				return err
			})
		}()
		select {
		case err := <-raised:
			t.Fatalf("the raise finished while the merge held the lock: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		close(release)
		if err := <-mergeDone; err != nil {
			t.Fatalf("merge err=%v", err)
		}
		if err := <-raised; err != nil {
			t.Fatalf("raise err=%v", err)
		}
		if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskMerged || forge.merges != 1 {
			t.Fatalf("task=%s merges=%d", current.State, forge.merges)
		}
	})
	t.Run("RaiseFirst", func(t *testing.T) {
		ctx, st, task, d, forge := riskApprovedTask(t, "risk-serial-raise-first", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
		locked, raised := make(chan struct{}), make(chan error, 1)
		go func() {
			raised <- st.WithTaskSideEffectLock(ctx, task.ID, func(lockedCtx context.Context) error {
				close(locked)
				time.Sleep(150 * time.Millisecond)
				_, err := st.RaiseTaskRiskTier(lockedCtx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "early raise", ActorID: gateOperator.ID}, store.RiskSeatSettings{})
				return err
			})
		}()
		<-locked
		err := d.MergeApprovedTask(ctx, task, gateOperator)
		if raiseErr := <-raised; raiseErr != nil {
			t.Fatalf("raise err=%v", raiseErr)
		}
		var refusal *store.RiskApprovalsRequiredError
		if !errors.As(err, &refusal) || refusal.Required != 2 || forge.merges != 0 {
			t.Fatalf("merge after the raise err=%v merges=%d, want a refusal naming 2 approvals", err, forge.merges)
		}
		if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskApproved {
			t.Fatalf("task state=%s, want approved", current.State)
		}
	})
}
