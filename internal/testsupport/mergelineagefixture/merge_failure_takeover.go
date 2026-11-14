package mergelineagefixture

// Merge failure takeover (task 261114-38bea5): a failed merge stops being the
// task's forge failure once the factory or an agent takes it over, and a
// failure nothing answered stays the operator's with its text
// (req-operator-next-action AC-1.1, AC-1.7; DEC-117(3); component-next-action;
// component-submission-merge). Every case reaches its state through the real
// dispatcher and store commands with a scripted forge, on each backend.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	takeoverFirstHead  = "takeover-head-1"
	takeoverSecondHead = "takeover-head-2"
	baseModified       = "Base branch was modified"
)

// RunMergeFailureTakeover runs every merge failure takeover case on stores
// from open.
func RunMergeFailureTakeover(t *testing.T, open func(*testing.T) store.Store) {
	t.Helper()
	t.Run("conflict fix takes over a failed merge", func(t *testing.T) { runConflictFixTakeover(t, open(t)) })
	t.Run("submitted head takes over a failed gate merge", func(t *testing.T) { runSubmittedHeadTakeover(t, open(t)) })
	t.Run("reconciled merge answers a failed merge", func(t *testing.T) { runReconciledTakeover(t, open(t)) })
}

type takeover struct {
	ctx         context.Context
	st          store.Store
	d           *dispatch.Dispatcher
	task        core.Task
	operator    core.Actor
	operatorCtx context.Context

	head      string
	mergeable string
	mergeErr  error
	merged    bool
	mergedAt  string
}

func newTakeover(t *testing.T, st store.Store, gate bool) *takeover {
	t.Helper()
	if _, durable := st.(store.IdentityProvisioner); !durable {
		if backend, ok := st.(store.Backend); ok {
			st = checkIdentityBackend{backend}
		} else {
			st = checkIdentityStore{st}
		}
	}
	workspace := "takeover-" + core.NewTaskID()
	routes := map[string]config.StageRoute{
		"spec":      {Execution: config.ExecutionMCP},
		"implement": {Execution: config.ExecutionMCP},
		"review":    {Execution: config.ExecutionMCP},
	}
	settings := config.ContextualExecutionSettings{
		ControlPlane: config.ControlPlaneSettings{Triage: config.ModelTimeoutSettings{Model: "control", TimeoutText: "20m"}},
	}
	policy := config.PolicyContract{
		MaxBounces:    3,
		StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "2h", Review: "45m", Verify: "1h"},
		Review:        config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
		RefreshReview: config.RefreshReviewDelta,
	}
	cfg := (&config.Config{
		Workspace: workspace, Repos: []config.Repo{{Name: repository, URL: "https://github.com/" + githubSlug, GitHub: githubSlug, Base: "main"}},
		WorkOrderQueueTimeout: time.Hour, WorkOrderQueueTimeoutText: "1h", MaxBounces: 3, Recovery: config.DefaultRecoveryPolicy(),
		ExecutionSettings: &settings,
		Routing:           config.Routing{Triage: config.TriageRoute{Model: "control", TimeoutText: "20m", Timeout: 20 * time.Minute}, Stages: routes},
		Review:            config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
	}).WithPolicy(policy)
	if control, durable := st.(store.WorkspaceControlStore); durable {
		if _, err := control.CreateWorkspace(store.WithActor(t.Context(), store.Actor{ID: "test", Role: core.ActorHuman}), workspace, "Merge takeover "+workspace, cfg); err != nil {
			t.Fatal(err)
		}
	}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	id := "takeover-" + core.NewTaskID()
	task := core.Task{
		ID: id, Workspace: workspace, Repo: repository, Title: "Merge takeover " + id, BaseBranch: "main", Branch: "conveyor/" + id,
		State: core.TaskQueued, NextStage: core.StageImplement, MergeApproval: gate, SetupName: "backend", SetupContract: policy, PolicyVersion: 1,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	x := &takeover{ctx: ctx, st: st, d: dispatch.New(st, cfg, nil), task: task, mergeable: "MERGEABLE"}
	x.d.ReadCommitChecks = dispatch.NoChecksReportedForTest
	x.d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		if x.merged {
			return githubtrigger.PullRequest{Number: number, URL: pullURL(), State: "closed", Merged: true, BaseSHA: "base", HeadSHA: x.mergedAt, HeadRef: task.Branch, MergeCommitSHA: landed, MergedBy: "operator"}, nil
		}
		return githubtrigger.PullRequest{Number: number, URL: pullURL(), State: "open", Mergeable: x.mergeable, BaseSHA: "base", HeadSHA: x.head, HeadRef: task.Branch}, nil
	}
	x.d.RequestMerge = func(context.Context, string, int) error {
		if x.mergeErr != nil {
			return x.mergeErr
		}
		x.merged, x.mergedAt = true, x.head
		return nil
	}
	operatorID := checkOperatorID
	if identities, durable := st.(store.IdentityProvisioner); durable {
		user, err := identities.ProvisionIdentityUser(ctx, "operator-"+core.NewTaskID()+"@example.com", "Takeover Operator")
		if err != nil {
			t.Fatal(err)
		}
		operatorID = user.ID
	}
	x.operator = core.Actor{ID: store.UserActorID(operatorID), Role: core.ActorUser}
	x.operatorCtx = store.WithActor(ctx, x.operator)
	return x
}

func (x *takeover) current(t *testing.T) core.Task {
	t.Helper()
	task, err := x.st.GetTask(x.ctx, x.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (x *takeover) order(t *testing.T, id string) core.WorkOrder {
	t.Helper()
	order, err := x.st.GetWorkOrder(x.ctx, id)
	if err != nil {
		t.Fatalf("work order %s: %v", id, err)
	}
	return order
}

// dispatchNow runs the dispatcher's stage pass, which mints the next order.
func (x *takeover) dispatchNow(t *testing.T) {
	t.Helper()
	if err := x.d.DispatchNow(x.ctx, x.task.ID); err != nil {
		t.Fatal(err)
	}
}

func (x *takeover) claim(t *testing.T, id string) core.WorkOrder {
	t.Helper()
	claimed, err := storetest.ClaimWorkOrder(x.ctx, x.st, id, core.WorkOrderClaim{
		ClaimantID: "takeover-worker", SessionID: id + "-session", ClientToken: id + "-token",
		Lease: time.Hour, ExecutionTimeout: time.Hour, Agent: "claude-code", Model: "claude-opus-5-5",
	})
	if err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	return claimed
}

// submit records the implementation submission and advances the task to
// review as the work-order service does: a conflict fix of an approved head
// first stales that approval toward the new head.
func (x *takeover) submit(t *testing.T, order core.WorkOrder, head string) {
	t.Helper()
	before := x.current(t)
	order.State, order.HeadSHA = core.WorkOrderSubmitted, head
	if _, err := taskops.ExecuteWorkOrder(x.ctx, x.st, order.TaskID, core.WorkOrderCmdSubmitForReview, func(lease taskops.TaskLease) (core.Job, error) {
		return x.st.SubmitImplementationCommand(x.ctx, lease, store.ImplementationSubmission{Order: order, EndedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("submit %s: %v", order.ID, err)
	}
	if order.ReasonCode == "merge-conflict" && before.ApprovedHeadSHA != "" {
		if _, err := x.st.MarkTaskApprovalStale(x.ctx, x.task.ID, before.ApprovedHeadSHA, head, config.RefreshReviewDelta, "merge-conflict"); err != nil {
			t.Fatal(err)
		}
	}
	if before.State == core.TaskQueued {
		if _, err := taskops.New(x.st).Perform(x.ctx, x.task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := taskops.New(x.st).Perform(x.ctx, x.task.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
		t.Fatalf("advance to review: %v", err)
	}
	x.head = head
}

// approve claims the round's seat and accepts its approval at head.
func (x *takeover) approve(t *testing.T, round int, head string) {
	t.Helper()
	id := fmt.Sprintf("%s-review-%d-seat-1", x.task.ID, round)
	claimed := x.claim(t, id)
	claimed.State = core.WorkOrderCompleted
	if err := storetest.UpdateWorkOrder(x.ctx, x.st, claimed, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
		t.Fatal(err)
	}
	if err := taskops.New(x.st).AcceptReviewDecision(x.ctx, core.ReviewDecision{
		TaskID: x.task.ID, JobID: claimed.JobID, ReviewWorkOrderID: claimed.ID, ReviewRound: round, ReviewSeat: 1,
		Verdict: "approve", ReasonCode: "verified", Summary: "approve",
		HeadSHA: head, ReviewedCommitSHA: head, PolicyVersion: 1, MergeApproval: x.task.MergeApproval, MaxBounces: 3,
	}); err != nil {
		t.Fatalf("accept approval of round %d: %v", round, err)
	}
	// With the merge gate on, the task waits for the operator's merge, which
	// binds the approval to the reviewed head.
	task := x.current(t)
	want, bound := core.TaskApproved, task.ApprovedHeadSHA
	if x.task.MergeApproval {
		want, bound = core.TaskAwaiting, task.ReviewedHeadSHA
	}
	if task.State != want || bound != head {
		t.Fatalf("after approval task = %s head %q, want %s at %s", task.State, bound, want, head)
	}
}

// approvedAtFirstHead submits the first head and approves it in review
// round 1.
func (x *takeover) approvedAtFirstHead(t *testing.T) {
	t.Helper()
	x.dispatchNow(t)
	x.submit(t, x.claim(t, x.task.ID+"-implement-1"), takeoverFirstHead)
	x.dispatchNow(t)
	x.approve(t, 1, takeoverFirstHead)
}

func (x *takeover) mergeAutomatically(t *testing.T, refused bool) {
	t.Helper()
	err := x.d.MergeApprovedTaskAutomatically(x.ctx, x.current(t))
	if refused != (err != nil) {
		t.Fatalf("automatic merge err = %v, want refused=%t", err, refused)
	}
}

// requireAction reads the next action and requires its actor, kind, and,
// when order is set, the order it names.
func (x *takeover) requireAction(t *testing.T, actor, kind, order string) nextaction.NextAction {
	t.Helper()
	// The read is the operator's, who holds the capability the choices name.
	action, err := store.ReadNextAction(x.ctx, x.st, x.task.ID, store.NextActionOptions{Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true}})
	if err != nil {
		t.Fatal(err)
	}
	named := ""
	if action.CurrentOrder != nil {
		named = action.CurrentOrder.ID
	}
	if action.Actor != actor || action.Kind != kind || (order != "" && named != order) {
		t.Fatalf("next action = %s/%s order %q, want %s/%s order %q", action.Actor, action.Kind, named, actor, kind, order)
	}
	return action
}

// requireForgeFailure requires that every read of the task's forge failure
// agrees: the full and compact ledger folds and the single-task and
// workspace activity markers. want is the surface and a fragment of the
// detail, or nil for no failure.
func (x *takeover) requireForgeFailure(t *testing.T, label string, want *store.ForgeFailure) {
	t.Helper()
	full, err := x.st.ListEvents(x.ctx, x.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := x.st.ListCompactEvents(x.ctx, x.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	single, err := x.st.ListActivityMarkersForTasks(x.ctx, []string{x.task.ID})
	if err != nil {
		t.Fatal(err)
	}
	all, err := x.st.ListActivityMarkers(x.ctx)
	if err != nil {
		t.Fatal(err)
	}
	marker := func(markers []store.ActivityMarker) *store.ForgeFailure {
		for _, m := range markers {
			if m.TaskID == x.task.ID {
				return m.ForgeFailure
			}
		}
		t.Fatalf("%s: no activity marker for %s", label, x.task.ID)
		return nil
	}
	reads := map[string]*store.ForgeFailure{
		"full ledger":      store.LatestForgeFailure(full),
		"compact ledger":   store.LatestForgeFailure(compact),
		"single marker":    marker(single),
		"workspace marker": marker(all),
	}
	for name, got := range reads {
		if want == nil {
			if got != nil {
				t.Fatalf("%s: %s forge failure = %+v, want none", label, name, got)
			}
			continue
		}
		if got == nil || got.Surface != want.Surface || !strings.Contains(got.Detail, want.Detail) {
			t.Fatalf("%s: %s forge failure = %+v, want %s naming %q", label, name, got, want.Surface, want.Detail)
		}
		if !reflect.DeepEqual(got, reads["full ledger"]) {
			t.Fatalf("%s: %s forge failure %+v differs from the full ledger %+v", label, name, got, reads["full ledger"])
		}
	}
}

func mergeFailure(detail string) *store.ForgeFailure {
	return &store.ForgeFailure{Surface: "GitHub merge", Detail: detail}
}

// runConflictFixTakeover is the reported sequence: an approved review, a
// merge GitHub refused because the base moved, a blocked merge, a dispatched
// conflict fix, and its claim. The claimed fix is the next action, then the
// refresh review, and the confirmed merge ends the task. A refused merge of
// the refreshed head is the operator's again.
func runConflictFixTakeover(t *testing.T, st store.Store) {
	x := newTakeover(t, st, false)
	x.approvedAtFirstHead(t)

	x.mergeErr = githubtrigger.StatusError(405, errors.New(baseModified))
	x.mergeAutomatically(t, true)
	// The approved task with the merge gate off stays the factory's merge;
	// the failure and its text stay on every read (component-next-action
	// row order is unchanged).
	x.requireForgeFailure(t, "unanswered merge failure", mergeFailure(baseModified))
	x.requireAction(t, nextaction.ActorFactory, nextaction.KindMerging, "")

	x.mergeErr, x.mergeable = nil, "CONFLICTING"
	x.mergeAutomatically(t, false)
	fix := x.task.ID + "-implement-2"
	if order := x.order(t, fix); order.ReasonCode != "merge-conflict" || order.State != core.WorkOrderQueued {
		t.Fatalf("conflict fix = %s/%s, want a queued merge-conflict order", order.State, order.ReasonCode)
	}
	if blocked := countKindIn(t, x.st, x.ctx, x.task.ID, "merge.blocked"); blocked != 1 {
		t.Fatalf("merge.blocked events = %d, want 1", blocked)
	}
	x.requireForgeFailure(t, "queued conflict fix", nil)
	x.requireAction(t, nextaction.ActorAgent, nextaction.KindQueued, fix)

	claimed := x.claim(t, fix)
	x.requireForgeFailure(t, "claimed conflict fix", nil)
	action := x.requireAction(t, nextaction.ActorAgent, nextaction.KindWorking, fix)
	for _, secondary := range action.Details.Secondary {
		if secondary.Kind == nextaction.KindForgeFailure {
			t.Fatalf("claimed conflict fix keeps a forge failure statement: %+v", action.Details.Secondary)
		}
	}

	x.mergeable = "MERGEABLE"
	x.submit(t, claimed, takeoverSecondHead)
	x.dispatchNow(t)
	refresh := x.task.ID + "-review-2-seat-1"
	x.requireForgeFailure(t, "submitted conflict fix", nil)
	x.requireAction(t, nextaction.ActorAgent, nextaction.KindQueued, refresh)

	x.approve(t, 2, takeoverSecondHead)
	x.mergeErr = githubtrigger.StatusError(405, errors.New("Required status check is expected"))
	x.mergeAutomatically(t, true)
	x.requireForgeFailure(t, "later merge failure", mergeFailure("Required status check is expected"))

	x.mergeErr = nil
	x.mergeAutomatically(t, false)
	if got := x.current(t).State; got != core.TaskMerged {
		t.Fatalf("task = %s, want merged", got)
	}
	x.requireForgeFailure(t, "confirmed merge", nil)
	x.requireAction(t, nextaction.ActorNone, nextaction.KindTerminal, "")
}

// runSubmittedHeadTakeover: with the merge gate on, the operator's merge is
// refused and the operator requests changes. While the new implement order is
// worked, nothing has answered the failure, so it stays the operator's with
// its text. The order's submitted head answers it, and the review waits.
func runSubmittedHeadTakeover(t *testing.T, st store.Store) {
	x := newTakeover(t, st, true)
	x.approvedAtFirstHead(t)

	x.mergeErr = githubtrigger.StatusError(405, errors.New(baseModified))
	if _, err := x.d.MergeAtGate(x.operatorCtx, x.current(t), x.operator); !errors.Is(err, dispatch.ErrGateMergeRefused) {
		t.Fatalf("gate merge err = %v, want %v", err, dispatch.ErrGateMergeRefused)
	}
	x.requireForgeFailure(t, "refused gate merge", mergeFailure(baseModified))
	x.requireAction(t, nextaction.ActorOperator, nextaction.KindMergeApproval, "")

	if _, err := taskops.New(x.st).RequestChanges(x.operatorCtx, taskops.RequestChanges{TaskID: x.task.ID, Feedback: "rebase on main", MaxBounces: 3}); err != nil {
		t.Fatalf("request changes: %v", err)
	}
	x.dispatchNow(t)
	next := x.task.ID + "-implement-2"
	claimed := x.claim(t, next)
	x.requireForgeFailure(t, "claimed replacement order", mergeFailure(baseModified))
	action := x.requireAction(t, nextaction.ActorOperator, nextaction.KindForgeFailure, "")
	if action.Summary == "" {
		t.Fatalf("forge failure statement carries no summary: %+v", action)
	}
	x.requireRetryRoundRefused(t, action)

	x.submit(t, claimed, takeoverSecondHead)
	x.dispatchNow(t)
	x.requireForgeFailure(t, "submitted replacement head", nil)
	x.requireAction(t, nextaction.ActorAgent, nextaction.KindQueued, x.task.ID+"-review-2-seat-1")
}

// runReconciledTakeover: GitHub refuses the merge, then reports the pull
// request merged at the approved head; the reconciled merge answers the
// failure.
func runReconciledTakeover(t *testing.T, st store.Store) {
	x := newTakeover(t, st, false)
	x.approvedAtFirstHead(t)

	x.mergeErr = githubtrigger.StatusError(405, errors.New(baseModified))
	x.mergeAutomatically(t, true)
	x.requireForgeFailure(t, "unanswered merge failure", mergeFailure(baseModified))

	x.merged, x.mergedAt = true, takeoverFirstHead
	x.mergeAutomatically(t, false)
	if got := x.current(t).State; got != core.TaskMerged {
		t.Fatalf("task = %s, want merged", got)
	}
	if reconciled := countKindIn(t, x.st, x.ctx, x.task.ID, "merge.reconciled"); reconciled != 1 {
		t.Fatalf("merge.reconciled events = %d, want 1", reconciled)
	}
	x.requireForgeFailure(t, "reconciled merge", nil)
	x.requireAction(t, nextaction.ActorNone, nextaction.KindTerminal, "")
}

// requireRetryRoundRefused records the reported choice mismatch without
// changing it (task 261114-38bea5 plan step 6): the forge failure row offers
// Retry round as an allowed act, and the work-order service that
// POST /v1/tasks/{id}/review-round/retry calls refuses it, because a failed
// merge leaves no timed-out review round to retry (req-operator-next-action
// AC-1.4; component-next-action).
func (x *takeover) requireRetryRoundRefused(t *testing.T, action nextaction.NextAction) {
	t.Helper()
	offered := false
	for _, choice := range action.Choices {
		if choice.ID == "retry_round" {
			offered = choice.Allowed
		}
	}
	if !offered {
		t.Fatalf("forge failure choices = %+v, want the reported allowed retry_round", action.Choices)
	}
	service := &workorder.Service{Store: x.st, Dispatcher: x.d}
	_, err := service.RetryReviewRound(x.operatorCtx, x.task.ID, "takeover-retry", "the merge failed")
	if !errors.Is(err, store.ErrReviewRetryConflict) {
		t.Fatalf("retry round err = %v, want %v", err, store.ErrReviewRetryConflict)
	}
	t.Logf("retry_round offered as allowed; the route refuses it: %v", err)
}
