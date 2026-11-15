package workorder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Delta review rounds through the real submission service and dispatcher on
// a GitHub-backed repository (feature-delta-review-rounds DR-2 to DR-5;
// req-review-gates-evidence AC-12.2 to AC-12.6).

var deltaBase, deltaJudged, deltaHead = strings.Repeat("b", 40), strings.Repeat("j", 40), strings.Repeat("c", 40)

// failingSubmissionStore fails the next implementation completion after the
// submission recorded its comparison, as a partial submission does.
type failingSubmissionStore struct {
	store.Store
	fail *bool
}

func (s failingSubmissionStore) SubmitImplementationCommand(ctx context.Context, lease taskops.TaskLease, submission store.ImplementationSubmission) (core.Job, error) {
	if *s.fail {
		*s.fail = false
		return core.Job{}, errors.New("completion failed after the comparison was recorded")
	}
	return s.Store.SubmitImplementationCommand(ctx, lease, submission)
}

type deltaFixture struct {
	ctx        context.Context
	st         store.Store
	task       core.Task
	cfg        *config.Config
	dispatcher *dispatch.Dispatcher
	service    *Service
	implement  string
	session    int
	// head is the pull request head the forge reports.
	head string
	// ancestry answers the comparison of the judged head with the head.
	ancestry      githubtrigger.CompareAncestryResult
	ancestryErr   error
	ancestryCalls int
	failNext      bool
	// diffs and changed records every base and head the forge was asked for.
	diffs, changed []string
}

func newDeltaFixture(t *testing.T, policy string) *deltaFixture {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	memory := store.NewMemory()
	f := &deltaFixture{ctx: ctx, head: deltaJudged, ancestry: githubtrigger.CompareAncestryResult{Status: "ahead", AheadBy: 1}}
	f.st = failingSubmissionStore{Store: memory, fail: &f.failNext}
	f.task = core.Task{ID: "delta-task", Workspace: "demo", Repo: "app", Title: "Delta", Branch: "conveyor/delta-task", BaseBranch: "main", PolicyVersion: 1,
		State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
		SetupContract: config.PolicyContract{MaxBounces: 5, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}, StageTimeouts: config.PolicyStageTimeouts{Review: "1h"}, LaterRoundReview: policy}}
	if err := memory.CreateTask(ctx, f.task); err != nil {
		t.Fatal(err)
	}
	f.cfg = &config.Config{Workspace: "demo", MaxBounces: 5, WorkOrderQueueTimeout: time.Hour,
		Repos:  []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}},
		Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Execution: config.ExecutionMCP, TimeoutText: "1h"},
			"review":    {Execution: config.ExecutionMCP, TimeoutText: "1h"},
		}}}
	f.dispatcher = dispatch.New(memory, f.cfg, nil)
	f.dispatcher.DisableMemoryQueueForTest()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	f.service = &Service{Store: f.st, Dispatcher: f.dispatcher, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return f.cfg, nil },
		SubmissionPR: func(_ context.Context, _, branch string) (githubtrigger.SubmissionPullRequest, error) {
			pr := githubtrigger.SubmissionPullRequest{Number: 9, URL: "https://github.com/acme/app/pull/9"}
			pr.Head.Ref, pr.Head.SHA, pr.Base.Ref, pr.Base.SHA = branch, f.head, "main", deltaBase
			return pr, nil
		},
		ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
			return githubtrigger.ReviewTarget{Number: 9, BaseSHA: deltaBase, HeadSHA: f.head}, nil
		},
		SubmissionChangedPaths: func(_ context.Context, _ *config.Config, task core.Task) ([]string, error) {
			f.changed = append(f.changed, task.BaseBranch+"..."+task.ReviewedHeadSHA)
			return []string{"internal/a.go"}, nil
		},
		ReviewDiffBetween: func(_ context.Context, _, baseline, head string) (string, error) {
			f.diffs = append(f.diffs, baseline+"..."+head)
			return "diff " + baseline + "..." + head, nil
		},
		ReviewPRDescription: func(context.Context, string, string) (string, error) { return "", nil },
		SubmissionAncestry: func(_ context.Context, _, base, head string) (githubtrigger.CompareAncestryResult, error) {
			f.ancestryCalls++
			if base != deltaJudged || head != f.head {
				return githubtrigger.CompareAncestryResult{}, fmt.Errorf("compared %s...%s", base, head)
			}
			return f.ancestry, f.ancestryErr
		},
	}
	prepareSubmissionTest(f.service)
	job := core.Job{ID: f.task.ID + "-implement-1", TaskID: f.task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = memory.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(memory).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: f.task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	f.claim(t, job.ID)
	return f
}

func (f *deltaFixture) sessionID() string { return fmt.Sprintf("delta-implementer-%d", f.session) }

func (f *deltaFixture) claim(t *testing.T, id string) {
	t.Helper()
	f.session++
	if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, id, core.WorkOrderClaim{SessionID: f.sessionID(), ClientToken: "delta-token", ClaimantID: core.TaskRunClaimantID("usr-delta"), OwnerUserID: "usr-delta", Agent: "codex", Model: "implementer", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	f.implement = id
}

func (f *deltaFixture) submit() (map[string]any, error) {
	return f.service.SubmitForReview(f.ctx, f.implement, f.sessionID(), f.head)
}

func (f *deltaFixture) mustSubmit(t *testing.T) {
	t.Helper()
	if result, err := f.submit(); err != nil || result["await_review"] != true {
		t.Fatalf("submit=%+v err=%v", result, err)
	}
	if err := f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *deltaFixture) round(t *testing.T, round int) []core.WorkOrder {
	t.Helper()
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	seats := []core.WorkOrder{}
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound == round {
			seats = append(seats, order)
		}
	}
	if len(seats) == 0 {
		t.Fatalf("round %d has no seats: %+v", round, orders)
	}
	return seats
}

// bounce has every seat of round request changes on its head and claims the
// implement order the settled round dispatches.
func (f *deltaFixture) bounce(t *testing.T, round int) {
	t.Helper()
	applicable, citable := false, false
	for _, seat := range f.round(t, round) {
		session := "delta-review-" + seat.ID
		if _, err := f.service.Claim(f.ctx, seat.ID, core.WorkOrderClaim{SessionID: session, ClientToken: "token-" + seat.ID, ClaimantID: "reviewer-" + seat.ID, OwnerUserID: "usr-reviewer", Agent: "codex", Model: "reviewer", Lease: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.SubmitVerdict(f.ctx, seat.ID, session, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "revise", Feedback: "add a test for the empty input", GovernanceAssessment: &core.GovernanceAssessment{
			DesignApplicable: &applicable, DecisionCitable: &citable, CitedIDs: []string{}, UnknownIDs: []string{}, UngovernedIDs: []string{}, SupersededIDs: []string{}, Conflicts: []string{},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range orders {
		if order.Stage == core.StageImplement && order.State == core.WorkOrderQueued {
			f.claim(t, order.ID)
			return
		}
	}
	t.Fatalf("round %d bounce dispatched no implement order", round)
}

// judgedThenNextHead submits the judged head, bounces its full round, and
// moves the pull request to the next head.
func (f *deltaFixture) judgedThenNextHead(t *testing.T) {
	t.Helper()
	f.mustSubmit(t)
	for _, seat := range f.round(t, 1) {
		if seat.ReviewScope != "" || seat.BaselineSHA != deltaBase || seat.HeadSHA != deltaJudged {
			t.Fatalf("first round seat=%+v, want the full pair", seat)
		}
	}
	f.bounce(t, 1)
	f.head = deltaHead
}

func (f *deltaFixture) comparisons(t *testing.T) []store.SubmissionComparison {
	t.Helper()
	events, err := f.st.ListCompactEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.SubmissionComparison
	for i := range events {
		if comparison, ok := store.LatestSubmissionComparison(events[i : i+1]); ok {
			out = append(out, comparison)
		}
	}
	return out
}

func requireDeltaSeats(t *testing.T, seats []core.WorkOrder, scope, baseline, head string) {
	t.Helper()
	for _, seat := range seats {
		if seat.ReviewScope != scope || seat.BaselineSHA != baseline || seat.HeadSHA != head || seat.ReviewKind != "" {
			t.Fatalf("seat %s scope=%q baseline=%q head=%q kind=%q, want scope=%q baseline=%q head=%q", seat.ID, seat.ReviewScope, seat.BaselineSHA, seat.HeadSHA, seat.ReviewKind, scope, baseline, head)
		}
	}
}

// A head submitted after a reviewer's bounce under a frozen delta policy is
// reviewed from the judged head: the recorded comparison, the round's seats,
// and the seat's context state delta, carry the judged-head-to-head diff and
// the bounce history, while the changed paths, the admission diff, and the
// recorded base keep the observed base commit (AC-12.3 to AC-12.5; DR-5).
func TestDeltaReviewSubmissionAfterBounce(t *testing.T) {
	f := newDeltaFixture(t, config.LaterRoundReviewDelta)
	f.judgedThenNextHead(t)
	f.changed, f.diffs = nil, nil
	f.mustSubmit(t)
	if f.ancestryCalls != 1 {
		t.Fatalf("ancestry reads=%d, want 1", f.ancestryCalls)
	}
	recorded := f.comparisons(t)
	last := recorded[len(recorded)-1]
	if !last.Delta() || last.ReviewBaselineSHA != deltaJudged || last.BaseSHA != deltaBase || last.HeadSHA != deltaHead {
		t.Fatalf("recorded comparison=%+v", last)
	}
	// Whole-change readers keep the observed base commit (DR-5).
	if len(f.changed) == 0 || f.changed[0] != deltaBase+"..."+deltaHead || len(f.diffs) == 0 || f.diffs[0] != deltaBase+"..."+deltaHead {
		t.Fatalf("whole-change reads changed=%v diffs=%v", f.changed, f.diffs)
	}
	seats := f.round(t, 2)
	requireDeltaSeats(t, seats, store.ReviewScopeDelta, deltaJudged, deltaHead)
	session := "delta-review-" + seats[0].ID
	if _, err := f.service.Claim(f.ctx, seats[0].ID, core.WorkOrderClaim{SessionID: session, ClientToken: "token", ClaimantID: "reviewer", OwnerUserID: "usr-reviewer", Agent: "codex", Model: "reviewer", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	contextResult, err := f.service.Get(f.ctx, seats[0].ID, session)
	if err != nil {
		t.Fatal(err)
	}
	comparison := contextResult.ReviewComparison
	if comparison == nil || comparison.Scope != store.ReviewScopeDelta || comparison.BaselineSHA != deltaJudged || comparison.HeadSHA != deltaHead {
		t.Fatalf("seat comparison=%+v", comparison)
	}
	if contextResult.Diff != "diff "+deltaJudged+"..."+deltaHead {
		t.Fatalf("seat diff=%q", contextResult.Diff)
	}
	if len(contextResult.BounceHistory) == 0 || !strings.Contains(string(contextResult.BounceHistory[0]), "add a test for the empty input") {
		t.Fatalf("seat bounce_history=%s", contextResult.BounceHistory)
	}
	// The delta seat reads the delta round instruction (DR-5).
	if !strings.Contains(contextResult.RolePrompt, deltaRoundInstruction) || strings.Contains(contextResult.RolePrompt, "conveyor:delta-round") {
		t.Fatal("the delta seat's role lacks the delta round instruction or carries its marker")
	}
}

const deltaRoundInstruction = "a complete review panel already judged the baseline commit"

// seatRole claims seat and returns the role prompt its context serves.
func (f *deltaFixture) seatRole(t *testing.T, seat core.WorkOrder) string {
	t.Helper()
	session := "role-review-" + seat.ID
	if _, err := f.service.Claim(f.ctx, seat.ID, core.WorkOrderClaim{SessionID: session, ClientToken: "token", ClaimantID: "reviewer", OwnerUserID: "usr-reviewer", Agent: "codex", Model: "reviewer", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	contextResult, err := f.service.Get(f.ctx, seat.ID, session)
	if err != nil {
		t.Fatal(err)
	}
	return contextResult.RolePrompt
}

// A frozen full or absent policy keeps today's full rounds after a bounce and
// reads no ancestry (AC-12.3; AC-12.1).
func TestDeltaReviewFullPolicyUnchanged(t *testing.T) {
	for _, policy := range []string{"", config.LaterRoundReviewFull} {
		t.Run("policy="+policy, func(t *testing.T) {
			f := newDeltaFixture(t, policy)
			f.judgedThenNextHead(t)
			f.mustSubmit(t)
			if f.ancestryCalls != 0 {
				t.Fatalf("a full policy read ancestry %d times", f.ancestryCalls)
			}
			seats := f.round(t, 2)
			requireDeltaSeats(t, seats, "", deltaBase, deltaHead)
			if role := f.seatRole(t, seats[0]); strings.Contains(role, deltaRoundInstruction) || strings.Contains(role, "conveyor:delta-round") {
				t.Fatal("a full seat's role carries the delta round section")
			}
		})
	}
}

// A comparison read that fails, or reports the head anything but ahead with
// nothing behind, fixes full from the observed base and never refuses the
// submission (AC-12.3; DR-3).
func TestDeltaReviewAncestryFallback(t *testing.T) {
	for name, tc := range map[string]struct {
		ancestry githubtrigger.CompareAncestryResult
		err      error
	}{
		// A failed read that still carries an ahead answer gives full.
		"read fails": {ancestry: githubtrigger.CompareAncestryResult{Status: "ahead", AheadBy: 1}, err: githubtrigger.StatusError(502, errors.New("bad gateway"))},
		"diverged":   {ancestry: githubtrigger.CompareAncestryResult{Status: "diverged", AheadBy: 1, BehindBy: 1}},
		"behind":     {ancestry: githubtrigger.CompareAncestryResult{Status: "behind", BehindBy: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDeltaFixture(t, config.LaterRoundReviewDelta)
			f.judgedThenNextHead(t)
			f.ancestry, f.ancestryErr = tc.ancestry, tc.err
			f.mustSubmit(t)
			recorded := f.comparisons(t)
			if last := recorded[len(recorded)-1]; last.Delta() || last.ReviewBaselineSHA != deltaBase || !last.Fixed {
				t.Fatalf("fallback comparison=%+v", last)
			}
			requireDeltaSeats(t, f.round(t, 2), "", deltaBase, deltaHead)
		})
	}
}

// A submission that fails after recording its comparison and is retried keeps
// the scope recorded first, even after the policy changes to full and the
// ancestry read starts failing; a replay of the accepted submission writes no
// new comparison (DR-3; AC-12.8).
func TestDeltaReviewPartialRetryAndReplayKeepFirstScope(t *testing.T) {
	f := newDeltaFixture(t, config.LaterRoundReviewDelta)
	f.judgedThenNextHead(t)
	f.failNext = true
	if _, err := f.submit(); err == nil {
		t.Fatal("the failed completion was reported as a submission")
	}
	recorded := f.comparisons(t)
	if first := recorded[len(recorded)-1]; !first.Delta() || first.ReviewBaselineSHA != deltaJudged {
		t.Fatalf("partial submission comparison=%+v", first)
	}
	full := config.LaterRoundReviewFull
	user := store.WithActor(f.ctx, store.Actor{ID: "operator", Role: core.ActorUser})
	if _, err := taskops.ExecuteSetupChange(user, f.st, f.task.ID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
		return f.st.ChangeTaskPolicyCommand(user, lease, store.SetupChangeRequest{TaskID: f.task.ID, RequestID: "to-full", Reason: "compare whole changes", Policy: &store.TaskPolicyChange{LaterRoundReview: &full}})
	}); err != nil {
		t.Fatal(err)
	}
	f.ancestryErr = errors.New("forge unavailable")
	calls := f.ancestryCalls
	f.mustSubmit(t)
	if f.ancestryCalls != calls {
		t.Fatal("the retry read ancestry again instead of keeping the fixed scope")
	}
	recorded = f.comparisons(t)
	if retried := recorded[len(recorded)-1]; !retried.Delta() || retried.ReviewBaselineSHA != deltaJudged || retried.HeadSHA != deltaHead {
		t.Fatalf("retried comparison=%+v", retried)
	}
	requireDeltaSeats(t, f.round(t, 2), store.ReviewScopeDelta, deltaJudged, deltaHead)
	before := len(recorded)
	replay, err := f.submit()
	if err != nil || replay["replayed"] != true {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if after := len(f.comparisons(t)); after != before {
		t.Fatalf("replay recorded %d comparisons, want %d", after, before)
	}
}

// A stale approval keeps the refresh rules: a conflict-fix submission from the
// approved head records no review scope, so its rounds stay refresh rounds
// (AC-12.6).
func TestDeltaReviewStaleApprovalRecordsNoScope(t *testing.T) {
	f := newDeltaFixture(t, config.LaterRoundReviewDelta)
	f.judgedThenNextHead(t)
	task, err := f.st.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	task.ApprovedHeadSHA = deltaJudged
	order := core.WorkOrder{ReasonCode: "merge-conflict", BaselineSHA: deltaJudged}
	if !submissionMarksApprovalStale(task, order) {
		t.Fatal("a conflict fix from the approved head did not mark the approval stale")
	}
	fields, err := f.service.submissionReviewScope(f.ctx, task, order, "acme/app", deltaBase, deltaHead)
	if err != nil || fields != nil {
		t.Fatalf("stale submission fields=%v err=%v", fields, err)
	}
	task.ApprovalStale = true
	if fields, err = f.service.submissionReviewScope(f.ctx, task, core.WorkOrder{}, "acme/app", deltaBase, deltaHead); err != nil || fields != nil {
		t.Fatalf("submission while stale fields=%v err=%v", fields, err)
	}
	if f.ancestryCalls != 0 {
		t.Fatalf("a stale submission read ancestry %d times", f.ancestryCalls)
	}
}

// A retried round of a delta head keeps the delta scope, the judged head, and
// the head on every new seat (DR-4; AC-12.4).
func TestDeltaReviewRoundRetryKeepsPair(t *testing.T) {
	f := newDeltaFixture(t, config.LaterRoundReviewDelta)
	f.judgedThenNextHead(t)
	f.mustSubmit(t)
	seats := f.round(t, 2)
	requireDeltaSeats(t, seats, store.ReviewScopeDelta, deltaJudged, deltaHead)
	for _, seat := range seats {
		if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, seat.ID, core.WorkOrderClaim{SessionID: "timeout-" + seat.ID, ClientToken: "timeout", ClaimantID: "reviewer", Lease: time.Minute, ExecutionTimeout: time.Nanosecond}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := taskops.New(f.st).TickOrderClock(f.ctx, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	user := store.WithActor(f.ctx, store.Actor{ID: "operator", Role: core.ActorUser})
	result, err := f.service.RetryReviewRound(user, f.task.ID, "delta-retry", "seat timed out")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WorkOrders) == 0 {
		t.Fatalf("retry result=%+v", result)
	}
	requireDeltaSeats(t, result.WorkOrders, store.ReviewScopeDelta, deltaJudged, deltaHead)
	requireDeltaSeats(t, f.round(t, result.NewRound), store.ReviewScopeDelta, deltaJudged, deltaHead)
}

// A policy handoff during a delta round inherits the recorded delta pair
// instead of refusing (DR-4; AC-12.4).
func TestDeltaReviewPolicyHandoffKeepsPair(t *testing.T) {
	f := newDeltaFixture(t, config.LaterRoundReviewDelta)
	f.judgedThenNextHead(t)
	f.mustSubmit(t)
	user := store.WithActor(f.ctx, store.Actor{ID: "operator", Role: core.ActorUser})
	enabled := true
	result, err := taskops.ExecuteSetupChange(user, f.st, f.task.ID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
		return f.st.ChangeTaskPolicyCommand(user, lease, store.SetupChangeRequest{TaskID: f.task.ID, RequestID: "verify-on", Reason: "verify the fix", Policy: &store.TaskPolicyChange{VerifyStage: &enabled}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedWorkOrders) != 1 {
		t.Fatalf("handoff=%+v", result)
	}
	verify, err := f.st.GetWorkOrder(f.ctx, result.CreatedWorkOrders[0])
	if err != nil {
		t.Fatal(err)
	}
	if verify.Stage != core.StageVerify || verify.ReviewScope != store.ReviewScopeDelta || verify.BaselineSHA != deltaJudged || verify.HeadSHA != deltaHead {
		t.Fatalf("handoff verify order=%+v", verify)
	}
}

// A seat order's context reads its scope from the order: an empty scope reads
// as full, delta compares the judged head with the head, and a delta diff over
// the 25 MiB limit is refused (DR-4; component-submission-merge invariant 9).
func TestDeltaReviewSeatMaterialFromOrder(t *testing.T) {
	f := newDeltaFixture(t, config.LaterRoundReviewDelta)
	task := f.task
	for _, tc := range []struct {
		name, scope, baseline, want string
	}{
		{name: "EmptyScopeReadsFull", baseline: deltaBase, want: config.RefreshReviewFull},
		{name: "FullScope", scope: config.RefreshReviewFull, baseline: deltaBase, want: config.RefreshReviewFull},
		{name: "DeltaScope", scope: store.ReviewScopeDelta, baseline: deltaJudged, want: store.ReviewScopeDelta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := core.WorkOrder{Stage: core.StageReview, ReviewScope: tc.scope, BaselineSHA: tc.baseline, HeadSHA: deltaHead}
			material, err := f.service.reviewMaterialForOrder(f.ctx, order, task, nil)
			if err != nil {
				t.Fatal(err)
			}
			if material.comparison.Scope != tc.want || material.comparison.BaselineSHA != tc.baseline || material.diff != "diff "+tc.baseline+"..."+deltaHead {
				t.Fatalf("material=%+v diff=%q", material.comparison, material.diff)
			}
		})
	}
	f.service.ReviewDiffBetween = func(context.Context, string, string, string) (string, error) {
		return strings.Repeat("x", 25<<20+1), nil
	}
	if _, err := f.service.reviewMaterialForOrder(f.ctx, core.WorkOrder{Stage: core.StageReview, ReviewScope: store.ReviewScopeDelta, BaselineSHA: deltaJudged, HeadSHA: deltaHead}, task, nil); err == nil || !strings.Contains(err.Error(), "25 MiB") {
		t.Fatalf("oversized delta diff err=%v", err)
	}
}
