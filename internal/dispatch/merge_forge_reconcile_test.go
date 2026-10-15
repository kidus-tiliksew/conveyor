package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func forgeReconcileCtx() context.Context {
	return store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
}

func forbidMergeCalls(t *testing.T, d *Dispatcher) {
	t.Helper()
	d.RequestMerge = func(context.Context, string, int) error {
		t.Fatal("RequestMerge called")
		return nil
	}
	d.RequestMergeWithCredential = func(context.Context, string, int, string) error {
		t.Fatal("RequestMergeWithCredential called")
		return nil
	}
}

func completeVerificationAtHead(t *testing.T, st store.Store, ctx context.Context, taskID, head string) {
	t.Helper()
	jobID := taskID + "-verify"
	if err := st.CreateJob(ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageVerify, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: jobID, JobID: jobID, TaskID: taskID, Stage: core.StageVerify, HeadSHA: head, CreatedAt: time.Now().UTC()}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "verify-session", ClientToken: "verify-token", WorkerID: "worker-verify", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderCompleted
	claimed.HeadSHA = head
	claimed.VerificationContextID = "sealed-context"
	if err := storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitVerification); err != nil {
		t.Fatal(err)
	}
}

func openPullRequest(t *testing.T, st store.Store, ctx context.Context, taskID string, number int, head string) {
	t.Helper()
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{
		"repository": "acme/app", "number": number, "head_sha": head,
	})}); err != nil {
		t.Fatal(err)
	}
}

func mergedPull(task core.Task, number int, head, commit, mergedBy string) githubtrigger.PullRequest {
	return githubtrigger.PullRequest{
		Number: number, URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", number),
		State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: head, HeadRef: task.Branch,
		MergeCommitSHA: commit, MergedBy: mergedBy, BaseSHA: "base",
	}
}

func newForgeDispatcher(st store.Store) *Dispatcher {
	return New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
}

func enterMergeGate(t *testing.T, st store.Store, ctx context.Context, id, reviewed, approved string, mergeApproval bool) core.Task {
	t.Helper()
	task := core.Task{
		ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id,
		State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: reviewed, ApprovedHeadSHA: approved,
		MergeApproval: mergeApproval, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	out, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Task.State != core.TaskAwaiting || out.Task.GateKind != core.GateKindMergeApproval || out.Task.ReviewedHeadSHA != reviewed {
		t.Fatalf("gate task=%+v", out.Task)
	}
	return out.Task
}

func reconciledPayload(t *testing.T, st store.Store, ctx context.Context, taskID string) map[string]any {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	count := 0
	for _, event := range events {
		if event.Kind != "merge.reconciled" {
			continue
		}
		count++
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("merge.reconciled events=%d", count)
	}
	return payload
}

// A merged task accepts its own recorded merge.confirmed after the approval
// moved; a different merge commit or pull request does not match
// (component-submission-merge "Observed merges"; AC-3.5).
func TestObservedMergeOfMergedTaskAcceptsRecordedMergeConfirmed(t *testing.T) {
	ctx, st := forgeReconcileCtx(), store.NewMemory()
	task := core.Task{ID: "merged", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/merged", State: core.TaskMerged, ApprovedHeadSHA: "later", ReviewedHeadSHA: "later", CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	openPullRequest(t, st, ctx, task.ID, 12, "opened-head")
	// Only the first merge.confirmed names this repository, pull request, and
	// head; the others differ in exactly one identity field.
	for _, confirmed := range []map[string]any{
		{"repository": "acme/app", "pull_request": 12, "head_sha": "merged-head", "merge_commit_sha": "landed"},
		{"repository": "acme/other", "pull_request": 12, "head_sha": "opened-head", "merge_commit_sha": "landed"},
		{"repository": "acme/app", "pull_request": 13, "head_sha": "opened-head", "merge_commit_sha": "landed"},
	} {
		if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(confirmed)}); err != nil {
			t.Fatal(err)
		}
	}
	seeded, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := len(seeded)
	d := newForgeDispatcher(st)
	forbidMergeCalls(t, d)
	cases := []struct {
		name string
		pr   githubtrigger.PullRequest
		want bool
	}{
		{"recorded merge", mergedPull(task, 12, "merged-head", "landed", "conveyor"), true},
		{"other merge commit", mergedPull(task, 12, "merged-head", "other", "conveyor"), false},
		{"opened head without its merge.confirmed", mergedPull(task, 12, "opened-head", "landed", "conveyor"), false},
	}
	for _, tc := range cases {
		accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", task.ID, tc.pr)
		if err != nil || accepted != tc.want {
			t.Fatalf("%s: accepted=%t err=%v, want %t", tc.name, accepted, err, tc.want)
		}
	}
	if events, err := st.ListEvents(ctx, task.ID); err != nil || len(events) != before {
		t.Fatalf("merged-task reconcile wrote events %d -> %d err=%v", before, len(events), err)
	}
}

func assertNoApprovalBound(t *testing.T, st store.Store, ctx context.Context, taskID string) {
	t.Helper()
	if count, _ := st.CountEvents(ctx, taskID, "approval.bound"); count != 0 {
		t.Fatalf("approval.bound events=%d", count)
	}
	current, err := st.GetTask(ctx, taskID)
	if err != nil || current.ApprovedHeadSHA != "" {
		t.Fatalf("approved head=%q err=%v", current.ApprovedHeadSHA, err)
	}
}

func assertStillAtMergeGate(t *testing.T, st store.Store, ctx context.Context, taskID string) {
	t.Helper()
	current, err := st.GetTask(ctx, taskID)
	if err != nil || current.State != core.TaskAwaiting || current.GateKind != core.GateKindMergeApproval {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	for _, kind := range []string{"merge.reconciled", "merge.failed"} {
		if count, _ := st.CountEvents(ctx, taskID, kind); count != 0 {
			t.Fatalf("%s events=%d", kind, count)
		}
	}
}

func TestObservedMergeAtMergeGateReconcilesToMerged(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	task := enterMergeGate(t, st, ctx, "gate-merged", "reviewed-head", "", true)
	openPullRequest(t, st, ctx, task.ID, 12, "reviewed-head")
	d := newForgeDispatcher(st)
	forbidMergeCalls(t, d)
	pr := mergedPull(task, 12, "reviewed-head", "landed", "github-operator")
	accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", task.ID, pr)
	if err != nil || !accepted {
		t.Fatalf("accepted=%t err=%v", accepted, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || current.GateKind != "" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	payload := reconciledPayload(t, st, ctx, task.ID)
	if payload["result"] != "merged_outside_conveyor" || payload["merged_by"] != "github-operator" {
		t.Fatalf("payload=%v", payload)
	}
	repeat, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", task.ID, pr)
	if err != nil || !repeat {
		t.Fatalf("repeat accepted=%t err=%v", repeat, err)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "merge.reconciled"); count != 1 {
		t.Fatalf("repeat events=%d", count)
	}
	assertNoApprovalBound(t, st, ctx, task.ID)

	t.Run("merged without a merge commit waits at the gate", func(t *testing.T) {
		pending := enterMergeGate(t, st, ctx, "gate-no-commit", "pending-head", "", true)
		openPullRequest(t, st, ctx, pending.ID, 15, "pending-head")
		accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", pending.ID, mergedPull(pending, 15, "pending-head", "", "github-operator"))
		if err != nil || accepted {
			t.Fatalf("accepted=%t err=%v", accepted, err)
		}
		assertStillAtMergeGate(t, st, ctx, pending.ID)
	})

	t.Run("old approval does not hide the reviewed head", func(t *testing.T) {
		variant := enterMergeGate(t, st, ctx, "gate-new-head", "new-reviewed", "old-approved", true)
		openPullRequest(t, st, ctx, variant.ID, 13, "new-reviewed")
		accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", variant.ID, mergedPull(variant, 13, "new-reviewed", "landed-new", "github-operator"))
		if err != nil || !accepted {
			t.Fatalf("accepted=%t err=%v", accepted, err)
		}
		current, err := st.GetTask(ctx, variant.ID)
		if err != nil || current.State != core.TaskMerged {
			t.Fatalf("task=%+v err=%v", current, err)
		}
	})

	t.Run("verify stage does not block an observed merge", func(t *testing.T) {
		verified := core.Task{
			ID: "gate-verified", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/gate-verified",
			State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "verified-head", MergeApproval: true,
			SetupContract: config.ExecutionSetup{VerifyStage: true}, CreatedAt: time.Now().UTC(),
		}
		if err := st.CreateTask(ctx, verified); err != nil {
			t.Fatal(err)
		}
		out, err := taskops.New(st).Perform(ctx, verified.ID, taskops.Command{Kind: core.TaskGateMerge})
		if err != nil {
			t.Fatal(err)
		}
		completeVerificationAtHead(t, st, ctx, verified.ID, "verified-head")
		openPullRequest(t, st, ctx, verified.ID, 14, "verified-head")
		accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", verified.ID, mergedPull(out.Task, 14, "verified-head", "landed-verified", "github-operator"))
		if err != nil || !accepted {
			t.Fatalf("accepted=%t err=%v", accepted, err)
		}
	})
}

// enterEventHeadMergeGate parks a merge gate whose reviewed head is recorded
// only on the review event, leaving ReviewedHeadSHA empty.
func enterEventHeadMergeGate(t *testing.T, st store.Store, ctx context.Context, id string, number int) core.Task {
	t.Helper()
	task := enterMergeGate(t, st, ctx, id, "", "", true)
	if err := st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"verdict": "approve", "reviewed_commit_sha": "event-head"})}); err != nil {
		t.Fatal(err)
	}
	openPullRequest(t, st, ctx, id, number, "event-head")
	return task
}

func TestObservedMergeAtEventOnlyReviewedHead(t *testing.T) {
	t.Run("repeat observation stays reconciled", func(t *testing.T) {
		ctx := forgeReconcileCtx()
		st := store.NewMemory()
		task := enterEventHeadMergeGate(t, st, ctx, "event-head-repeat", 31)
		d := newForgeDispatcher(st)
		forbidMergeCalls(t, d)
		pr := mergedPull(task, 31, "event-head", "landed", "github-operator")
		for attempt := range 2 {
			accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", task.ID, pr)
			if err != nil || !accepted {
				t.Fatalf("attempt %d accepted=%t err=%v", attempt, accepted, err)
			}
		}
		current, err := st.GetTask(ctx, task.ID)
		if err != nil || current.State != core.TaskMerged || current.ReviewedHeadSHA != "" {
			t.Fatalf("task=%+v err=%v", current, err)
		}
		reconciledPayload(t, st, ctx, task.ID)
		assertNoApprovalBound(t, st, ctx, task.ID)
	})
	t.Run("readiness recovery then monitor observation", func(t *testing.T) {
		ctx := forgeReconcileCtx()
		st := store.NewMemory()
		task := enterEventHeadMergeGate(t, st, ctx, "event-head-tick", 32)
		d := newForgeDispatcher(st)
		forbidMergeCalls(t, d)
		pr := mergedPull(task, 32, "event-head", "landed", "github-operator")
		d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) { return pr, nil }
		if reconciled, err := d.ReconcileMergeReadiness(ctx); err != nil || reconciled != 1 {
			t.Fatalf("reconciled=%d err=%v", reconciled, err)
		}
		accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", task.ID, pr)
		if err != nil || !accepted {
			t.Fatalf("accepted=%t err=%v", accepted, err)
		}
		reconciledPayload(t, st, ctx, task.ID)
		assertNoApprovalBound(t, st, ctx, task.ID)
	})
}

func TestObservedMergeAtOtherAwaitingGateIsIgnored(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	d := newForgeDispatcher(st)
	forbidMergeCalls(t, d)
	for _, kind := range []core.GateKind{core.GateKindJobFailure, core.GateKindPlanApproval, core.GateKind("")} {
		t.Run(string(kind), func(t *testing.T) {
			id := "other-" + string(kind)
			task := core.Task{
				ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id,
				State: core.TaskAwaiting, GateKind: kind, ReviewedHeadSHA: "reviewed-head", MergeApproval: true, CreatedAt: time.Now().UTC(),
			}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			openPullRequest(t, st, ctx, id, 21, "reviewed-head")
			accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", id, mergedPull(task, 21, "reviewed-head", "landed", "github-operator"))
			if err != nil || accepted {
				t.Fatalf("accepted=%t err=%v", accepted, err)
			}
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return mergedPull(task, 21, "reviewed-head", "landed", "github-operator"), nil
			}
			if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State == "MERGED" {
				t.Fatalf("readiness=%+v err=%v", readiness, err)
			}
			if reconciled, err := d.ReconcileMergeReadiness(ctx); err != nil || reconciled != 0 {
				t.Fatalf("sweep reconciled=%d err=%v", reconciled, err)
			}
			assertNoApprovalBound(t, st, ctx, id)
			current, err := st.GetTask(ctx, id)
			if err != nil || current.State != core.TaskAwaiting || current.GateKind != kind {
				t.Fatalf("task=%+v err=%v", current, err)
			}
			if count, _ := st.CountEvents(ctx, id, "merge.reconciled"); count != 0 {
				t.Fatalf("events=%d", count)
			}
			if count, _ := st.CountEvents(ctx, id, "merge.failed"); count != 0 {
				t.Fatalf("drift events=%d", count)
			}
		})
	}
}

func TestObservedMergeAtDifferentHeadRecordsDrift(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	d := newForgeDispatcher(st)
	forbidMergeCalls(t, d)
	assertDrift := func(t *testing.T, id, expected string, pr githubtrigger.PullRequest) {
		t.Helper()
		accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", id, pr)
		if err != nil || accepted {
			t.Fatalf("accepted=%t err=%v", accepted, err)
		}
		repeat, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", id, pr)
		if err != nil || repeat {
			t.Fatalf("repeat accepted=%t err=%v", repeat, err)
		}
		events, err := st.ListEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		failures := 0
		for _, event := range events {
			if event.Kind != "merge.failed" {
				continue
			}
			failures++
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["reason_code"] != "merged_head_mismatch" || payload["merged_head_sha"] != pr.HeadSHA || payload["expected_head_sha"] != expected {
				t.Fatalf("payload=%v", payload)
			}
			if !strings.Contains(fmt.Sprint(payload["error"]), pr.HeadSHA) || !strings.Contains(fmt.Sprint(payload["error"]), expected) {
				t.Fatalf("error=%v", payload["error"])
			}
		}
		if failures != 1 {
			t.Fatalf("drift events=%d", failures)
		}
		if got := store.LatestForgeFailure(events); got == nil || got.Category != "merged_head_mismatch" {
			t.Fatalf("forge failure=%+v", got)
		}
	}
	t.Run("merge gate", func(t *testing.T) {
		task := enterMergeGate(t, st, ctx, "drift-gate", "reviewed-head", "", true)
		openPullRequest(t, st, ctx, task.ID, 31, "other-head")
		before := task.State
		assertDrift(t, task.ID, "reviewed-head", mergedPull(task, 31, "other-head", "landed-other", "github-operator"))
		current, err := st.GetTask(ctx, task.ID)
		if err != nil || current.State != before {
			t.Fatalf("state=%s err=%v", current.State, err)
		}
	})
	t.Run("approved", func(t *testing.T) {
		task := core.Task{
			ID: "drift-approved", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/drift-approved",
			State: core.TaskApproved, ReviewedHeadSHA: "reviewed-head", ApprovedHeadSHA: "reviewed-head", CreatedAt: time.Now().UTC(),
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		openPullRequest(t, st, ctx, task.ID, 32, "other-head")
		assertDrift(t, task.ID, "reviewed-head", mergedPull(task, 32, "other-head", "landed-other", "github-operator"))
		current, err := st.GetTask(ctx, task.ID)
		if err != nil || current.State != core.TaskApproved {
			t.Fatalf("state=%s err=%v", current.State, err)
		}
	})
	t.Run("already merged writes nothing", func(t *testing.T) {
		task := core.Task{
			ID: "drift-merged", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/drift-merged",
			State: core.TaskMerged, ReviewedHeadSHA: "reviewed-head", ApprovedHeadSHA: "reviewed-head", CreatedAt: time.Now().UTC(),
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		openPullRequest(t, st, ctx, task.ID, 33, "other-head")
		accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "acme/app", task.ID, mergedPull(task, 33, "other-head", "landed-other", "github-operator"))
		if err != nil || accepted {
			t.Fatalf("accepted=%t err=%v", accepted, err)
		}
		if count, _ := st.CountEvents(ctx, task.ID, "merge.failed"); count != 0 {
			t.Fatalf("drift events=%d", count)
		}
	})
}

func TestReconcileMergeReadinessReconcilesGateOnApprovedMergedPR(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 41, State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: "approved-head", MergeCommitSHA: "landed", MergedBy: "github-operator"}, nil
	}
	forbidMergeCalls(t, d)
	reconciled, err := d.ReconcileMergeReadiness(ctx)
	if err != nil || reconciled != 1 || views != 1 {
		t.Fatalf("reconciled=%d views=%d err=%v", reconciled, views, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	if reconciledPayload(t, st, ctx, task.ID)["result"] != "merged_outside_conveyor" {
		t.Fatal("result missing")
	}
}

func TestReconcileMergeReadinessReconcilesMergeGateAwaitingMergedPR(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	task := enterMergeGate(t, st, ctx, "tick-gate", "reviewed-head", "", true)
	d := newForgeDispatcher(st)
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 51, State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: "reviewed-head", MergeCommitSHA: "landed", MergedBy: "github-operator"}, nil
	}
	forbidMergeCalls(t, d)
	reconciled, err := d.ReconcileMergeReadiness(ctx)
	if err != nil || reconciled != 1 || views != 1 {
		t.Fatalf("reconciled=%d views=%d err=%v", reconciled, views, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || current.GateKind != "" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	if reconciledPayload(t, st, ctx, task.ID)["result"] != "merged_outside_conveyor" {
		t.Fatal("result missing")
	}
	assertNoApprovalBound(t, st, ctx, task.ID)

	t.Run("merged without a merge commit reads UNKNOWN", func(t *testing.T) {
		ctx := forgeReconcileCtx()
		st := store.NewMemory()
		task := enterMergeGate(t, st, ctx, "tick-no-commit", "reviewed-head", "", true)
		d := newForgeDispatcher(st)
		d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{Number: 52, State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: "reviewed-head", MergedBy: "github-operator"}, nil
		}
		forbidMergeCalls(t, d)
		readiness, err := d.ReadMergeReadiness(ctx, task)
		if err != nil || readiness.State != "UNKNOWN" {
			t.Fatalf("readiness=%+v err=%v", readiness, err)
		}
		if reconciled, err := d.ReconcileMergeReadiness(ctx); err != nil || reconciled != 0 {
			t.Fatalf("reconciled=%d err=%v", reconciled, err)
		}
		assertStillAtMergeGate(t, st, ctx, task.ID)
	})
}

func TestReadMergeReadinessMergedPRAtOtherHeadRecordsDriftNoRefresh(t *testing.T) {
	assertNoRefresh := func(t *testing.T, current core.Task, st store.Store, ctx context.Context, boundBefore int) {
		t.Helper()
		if current.ApprovalStale || current.State == core.TaskQueued {
			t.Fatalf("refreshed task=%+v", current)
		}
		if bound, _ := st.CountEvents(ctx, current.ID, "approval.bound"); bound != boundBefore {
			t.Fatalf("approval.bound=%d before=%d", bound, boundBefore)
		}
		if blocked, _ := st.CountEvents(ctx, current.ID, "merge.blocked"); blocked != 0 {
			t.Fatalf("merge.blocked=%d", blocked)
		}
		orders, err := st.ListTaskWorkOrders(ctx, current.ID)
		if err != nil || len(orders) != 0 {
			t.Fatalf("orders=%+v err=%v", orders, err)
		}
	}
	t.Run("approved", func(t *testing.T) {
		ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
		if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
			t.Fatal(err)
		}
		d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{Number: 61, State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: "other-head", MergeCommitSHA: "landed"}, nil
		}
		forbidMergeCalls(t, d)
		boundBefore, _ := st.CountEvents(ctx, task.ID, "approval.bound")
		readiness, err := d.ReadMergeReadiness(ctx, task)
		if err != nil || readiness.State != "MERGED_HEAD_MISMATCH" {
			t.Fatalf("readiness=%+v err=%v", readiness, err)
		}
		if count, _ := st.CountEvents(ctx, task.ID, "merge.failed"); count != 1 {
			t.Fatalf("drift events=%d", count)
		}
		current, err := st.GetTask(ctx, task.ID)
		if err != nil || current.State != core.TaskApproved {
			t.Fatalf("task=%+v err=%v", current, err)
		}
		assertNoRefresh(t, current, st, ctx, boundBefore)
		repeat, err := d.ReadMergeReadiness(ctx, task)
		if err != nil || repeat.State != "MERGED_HEAD_MISMATCH" {
			t.Fatalf("repeat=%+v err=%v", repeat, err)
		}
		if count, _ := st.CountEvents(ctx, task.ID, "merge.failed"); count != 1 {
			t.Fatalf("repeat drift events=%d", count)
		}
	})
	t.Run("merge gate", func(t *testing.T) {
		ctx := forgeReconcileCtx()
		st := store.NewMemory()
		task := enterMergeGate(t, st, ctx, "readiness-drift", "reviewed-head", "", true)
		d := newForgeDispatcher(st)
		d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{Number: 62, State: "closed", Merged: true, Mergeable: "CONFLICTING", HeadSHA: "other-head", MergeCommitSHA: "landed"}, nil
		}
		forbidMergeCalls(t, d)
		readiness, err := d.ReadMergeReadiness(ctx, task)
		if err != nil || readiness.State != "MERGED_HEAD_MISMATCH" {
			t.Fatalf("readiness=%+v err=%v", readiness, err)
		}
		current, err := st.GetTask(ctx, task.ID)
		if err != nil || current.State != core.TaskAwaiting {
			t.Fatalf("task=%+v err=%v", current, err)
		}
		assertNoRefresh(t, current, st, ctx, 0)
	})
}

func TestReadMergeReadinessAwaitingGateIsReadOnlyWhenUnmerged(t *testing.T) {
	for _, pr := range []githubtrigger.PullRequest{
		{Number: 71, State: "open", Mergeable: "CONFLICTING", HeadSHA: "other-head"},
		{Number: 72, State: "open", Mergeable: "MERGEABLE", HeadSHA: "other-head"},
	} {
		t.Run(pr.Mergeable, func(t *testing.T) {
			ctx := forgeReconcileCtx()
			st := store.NewMemory()
			task := enterMergeGate(t, st, ctx, "readonly-"+pr.Mergeable, "reviewed-head", "", true)
			d := newForgeDispatcher(st)
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) { return pr, nil }
			forbidMergeCalls(t, d)
			readiness, err := d.ReadMergeReadiness(ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			// A head other than the reviewed head is STALE before any
			// mergeability answer (DEC-64(3)).
			if readiness.State != "STALE" {
				t.Fatalf("readiness=%+v", readiness)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil || current.State != core.TaskAwaiting || current.GateKind != core.GateKindMergeApproval || current.ApprovalStale {
				t.Fatalf("task=%+v err=%v", current, err)
			}
			for _, kind := range []string{"approval.bound", "merge.blocked", "merge.failed", "merge.reconciled"} {
				if count, _ := st.CountEvents(ctx, task.ID, kind); count != 0 {
					t.Fatalf("%s events=%d", kind, count)
				}
			}
			orders, err := st.ListTaskWorkOrders(ctx, task.ID)
			if err != nil || len(orders) != 0 {
				t.Fatalf("orders=%+v err=%v", orders, err)
			}
		})
	}
}

func TestReconcileMergeReadinessSkipsNonMergeAwaitingGates(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	task := core.Task{
		ID: "job-failure-merged", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/job-failure-merged",
		State: core.TaskAwaiting, GateKind: core.GateKindJobFailure, ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	d := newForgeDispatcher(st)
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return mergedPull(task, 81, "reviewed-head", "landed", "github-operator"), nil
	}
	forbidMergeCalls(t, d)
	reconciled, err := d.ReconcileMergeReadiness(ctx)
	if err != nil || reconciled != 0 || views != 0 {
		t.Fatalf("reconciled=%d views=%d err=%v", reconciled, views, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskAwaiting {
		t.Fatalf("task=%+v err=%v", current, err)
	}
}

func TestReconcileMergeReadinessGateOnNotStarvedByErrors(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	now := time.Now().UTC()
	gateOff := core.Task{
		ID: "gate-off-error", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/gate-off-error",
		State: core.TaskApproved, ApprovedHeadSHA: "off-head", ReviewedHeadSHA: "off-head", CreatedAt: now,
	}
	viewErrTask := core.Task{
		ID: "gate-on-view-error", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/gate-on-view-error",
		State: core.TaskApproved, MergeApproval: true, ApprovedHeadSHA: "view-head", ReviewedHeadSHA: "view-head", CreatedAt: now.Add(-time.Minute),
	}
	success := core.Task{
		ID: "gate-on-success", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/gate-on-success",
		State: core.TaskApproved, MergeApproval: true, ApprovedHeadSHA: "success-head", ReviewedHeadSHA: "success-head", CreatedAt: now.Add(-2 * time.Minute),
	}
	for _, task := range []core.Task{gateOff, viewErrTask, success} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	d := newForgeDispatcher(st)
	d.ViewPullRequest = func(_ context.Context, _ string, branch string) (githubtrigger.PullRequest, error) {
		switch branch {
		case gateOff.Branch:
			return githubtrigger.PullRequest{Number: 91, State: "open", Mergeable: "MERGEABLE", HeadSHA: "off-head"}, nil
		case viewErrTask.Branch:
			return githubtrigger.PullRequest{}, errors.New("gate-on view failed")
		case success.Branch:
			return githubtrigger.PullRequest{Number: 93, State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: "success-head", MergeCommitSHA: "landed", MergedBy: "github-operator"}, nil
		default:
			t.Fatalf("unexpected branch %s", branch)
			return githubtrigger.PullRequest{}, nil
		}
	}
	gateOffMerges := 0
	d.RequestMerge = func(_ context.Context, _ string, number int) error {
		gateOffMerges++
		if number != 91 {
			t.Fatalf("RequestMerge number=%d", number)
		}
		return errors.New("gate-off merge failed")
	}
	d.RequestMergeWithCredential = func(context.Context, string, int, string) error {
		t.Fatal("RequestMergeWithCredential called")
		return nil
	}
	_, err := d.ReconcileMergeReadiness(ctx)
	if err != nil || gateOffMerges != 1 {
		t.Fatalf("error=%v gate-off merges=%d", err, gateOffMerges)
	}
	current, getErr := st.GetTask(ctx, success.ID)
	if getErr != nil || current.State != core.TaskMerged {
		t.Fatalf("success task=%+v err=%v", current, getErr)
	}
}

func TestReconcileMergeReadinessGateOnSweepDoesNotDispatchOrRefresh(t *testing.T) {
	for _, pr := range []githubtrigger.PullRequest{
		{Number: 101, State: "open", Mergeable: "CONFLICTING", HeadSHA: "approved-head"},
		{Number: 102, State: "open", Mergeable: "MERGEABLE", HeadSHA: "moved-head"},
	} {
		t.Run(pr.Mergeable, func(t *testing.T) {
			ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewNone, true)
			if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
				t.Fatal(err)
			}
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) { return pr, nil }
			forbidMergeCalls(t, d)
			if reconciled, err := d.ReconcileMergeReadiness(ctx); err != nil || reconciled != 0 {
				t.Fatalf("reconciled=%d err=%v", reconciled, err)
			}
			orders, err := st.ListTaskWorkOrders(ctx, task.ID)
			if err != nil || len(orders) != 0 {
				t.Fatalf("orders=%+v err=%v", orders, err)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil || current.State != core.TaskApproved || current.ApprovalStale || current.ApprovedHeadSHA != "approved-head" {
				t.Fatalf("task=%+v err=%v", current, err)
			}
			if count, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); count != 0 {
				t.Fatalf("merge.blocked events=%d", count)
			}
		})
	}
}

func TestApproveAtMergeGateOnMergedPullRequestEndsMerged(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	task := enterMergeGate(t, st, ctx, "approve-merged", "reviewed-head", "", true)
	d := newForgeDispatcher(st)
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return mergedPull(task, 111, "reviewed-head", "landed", "github-operator"), nil
	}
	forbidMergeCalls(t, d)
	if err := d.HandleIntervention(ctx, task, core.Job{}, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || current.ApprovedHeadSHA != "reviewed-head" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	if reconciledPayload(t, st, ctx, task.ID)["result"] != "merged_outside_conveyor" {
		t.Fatal("result missing")
	}

	t.Run("already merged before the lock", func(t *testing.T) {
		merged := core.Task{
			ID: "approve-already-merged", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/approve-already-merged",
			State: core.TaskMerged, ReviewedHeadSHA: "reviewed-head", ApprovedHeadSHA: "reviewed-head", MergeApproval: true, CreatedAt: time.Now().UTC(),
		}
		if err := st.CreateTask(ctx, merged); err != nil {
			t.Fatal(err)
		}
		err := d.HandleIntervention(ctx, merged, core.Job{}, core.Intervention{TaskID: merged.ID, Action: core.InterventionApprove, ReasonCode: "approved"})
		var invalid *core.ErrInvalidTransition
		if !errors.As(err, &invalid) {
			t.Fatalf("err=%v", err)
		}
		for _, kind := range []string{"intervention.approve", "intervention.approve_review"} {
			if count, _ := st.CountEvents(ctx, merged.ID, kind); count != 0 {
				t.Fatalf("%s events=%d", kind, count)
			}
		}
		if count, _ := st.CountEvents(ctx, merged.ID, "approval.bound"); count != 0 {
			t.Fatalf("approval events=%d", count)
		}
	})
}

func TestApproveAtMergeGateUnmergedOrUnreadableStaysApproved(t *testing.T) {
	cases := []struct {
		name string
		pr   githubtrigger.PullRequest
		err  error
	}{
		{name: "open", pr: githubtrigger.PullRequest{Number: 121, State: "open", Mergeable: "MERGEABLE", HeadSHA: "reviewed-head"}},
		{name: "unreadable", err: errors.New("github unavailable")},
		{name: "no-commit", pr: githubtrigger.PullRequest{Number: 123, State: "closed", Merged: true, HeadSHA: "reviewed-head"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := forgeReconcileCtx()
			st := store.NewMemory()
			task := enterMergeGate(t, st, ctx, "approve-"+test.name, "reviewed-head", "", true)
			d := newForgeDispatcher(st)
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return test.pr, test.err
			}
			forbidMergeCalls(t, d)
			if err := d.HandleIntervention(ctx, task, core.Job{}, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
				t.Fatal(err)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil || current.State != core.TaskApproved || current.ApprovedHeadSHA != "reviewed-head" {
				t.Fatalf("task=%+v err=%v", current, err)
			}
			if count, _ := st.CountEvents(ctx, task.ID, "merge.reconciled"); count != 0 {
				t.Fatalf("reconciled events=%d", count)
			}
		})
	}
}

func TestGateOffRunningAlreadyMergedUsesWriter(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	task := core.Task{
		ID: "running-merged", Workspace: "test", Repo: "app", Branch: "conveyor/running-merged",
		State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, ApprovedHeadSHA: "accepted-head", CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"verdict": "approve", "approved_head_sha": "accepted-head"})}); err != nil {
		t.Fatal(err)
	}
	d := newForgeDispatcher(st)
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 44, State: "closed", Merged: true, HeadSHA: "accepted-head", MergedBy: "github-operator"}, nil
	}
	forbidMergeCalls(t, d)
	reconciled, err := d.ReconcileMergeReadiness(ctx)
	if err != nil || reconciled != 1 {
		t.Fatalf("reconciled=%d err=%v", reconciled, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	if reconciledPayload(t, st, ctx, task.ID)["result"] != "already_merged" {
		t.Fatal("already_merged result missing")
	}
}

func TestReadMergeReadinessMergedTaskDoesNotRewriteDrift(t *testing.T) {
	ctx := forgeReconcileCtx()
	st := store.NewMemory()
	task := core.Task{
		ID: "already-merged", Workspace: "test", Repo: "app", Branch: "conveyor/already-merged",
		State: core.TaskMerged, ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "merged_head_mismatch", "pull_request": 8, "merged_head_sha": "other", "merge_commit_sha": "landed"})}); err != nil {
		t.Fatal(err)
	}
	d := newForgeDispatcher(st)
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 8, State: "closed", Merged: true, HeadSHA: "other", MergeCommitSHA: "landed"}, nil
	}
	readiness, err := d.ReadMergeReadiness(ctx, task)
	if err != nil || readiness.State != "MERGED" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "merge.failed"); count != 1 {
		t.Fatalf("drift events=%d", count)
	}
}
