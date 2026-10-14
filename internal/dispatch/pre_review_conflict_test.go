package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// preReview drives a real implementation submission through the workorder
// service and the dispatcher with a stubbed GitHub pull request, so the
// pre-dispatch base check is observed exactly as production reaches it
// (feature-bounded-automatic-recovery AR-4; req-bounded-automatic-recovery AC-3.2).
type preReview struct {
	ctx        context.Context
	st         store.Store
	d          *dispatch.Dispatcher
	service    *workorder.Service
	task       core.Task
	reads      int
	mergeable  func(read int) string
	prHead     string
	viewErr    error
	submitHead string
}

func newPreReview(t *testing.T, verify bool) *preReview {
	t.Helper()
	f := &preReview{ctx: store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test"), st: store.NewMemory()}
	f.mergeable = func(int) string { return "MERGEABLE" }
	cfg := &config.Config{Workspace: "test", WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		"verify":    {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
	}}}
	cfg.Execution.VerifyStage = verify
	task := core.Task{ID: "pre-review", Workspace: "test", Repo: "app", Title: "Pre-review", Branch: "conveyor/pre-review", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, PolicyVersion: 1, SetupContract: cfg.FreezePolicy(), CreatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := f.st.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := f.st.CreateJob(f.ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(f.st).CreateWorkOrder(f.ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "token", ClaimantID: core.TaskRunClaimantID("usr-impl"), OwnerUserID: "usr-impl", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	f.d = dispatch.New(f.st, cfg, nil)
	f.d.DisableMemoryQueueForTest()
	f.d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		f.reads++
		if f.viewErr != nil {
			return githubtrigger.PullRequest{}, f.viewErr
		}
		return githubtrigger.PullRequest{Number: 7, State: "open", URL: "https://github.com/acme/app/pull/7", Mergeable: f.mergeable(f.reads), HeadSHA: f.prHead}, nil
	}
	bundle, err := pack.Load("")
	if err != nil {
		t.Fatal(err)
	}
	f.service = &workorder.Service{Store: f.st, Dispatcher: f.d, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		DiscoverDocumentationPolicy: func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
			return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "absent"}
		},
		SubmissionPR: func(_ context.Context, _ string, branch string) (githubtrigger.SubmissionPullRequest, error) {
			pr := githubtrigger.SubmissionPullRequest{Number: 7, URL: "https://github.com/acme/app/pull/7"}
			pr.Head.Ref, pr.Head.SHA, pr.Base.Ref, pr.Base.SHA = branch, f.submitHead, "main", "base-sha"
			return pr, nil
		},
		SubmissionPRWait:       func(context.Context, time.Duration) error { return nil },
		SubmissionChangedPaths: func(context.Context, *config.Config, core.Task) ([]string, error) { return []string{}, nil },
		ReconcileSubmissionPR:  func(context.Context, string, githubtrigger.SubmissionPullRequest, string) error { return nil },
		ReviewDiffBetween:      func(context.Context, string, string, string) (string, error) { return "diff", nil },
	}
	f.task = task
	return f
}

// submit records the claimed order's submission at head and runs the
// dispatcher for the advanced stage, as the queue would.
func (f *preReview) submit(t *testing.T, orderID, session, head string) {
	t.Helper()
	f.submitHead, f.prHead = head, head
	if _, err := f.service.SubmitForReview(f.ctx, orderID, session, head); err != nil {
		t.Fatal(err)
	}
	f.dispatch(t)
}

func (f *preReview) dispatch(t *testing.T) {
	t.Helper()
	if err := f.d.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *preReview) orders(t *testing.T, stage core.Stage, reason string) []core.WorkOrder {
	t.Helper()
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var matched []core.WorkOrder
	for _, order := range orders {
		if order.Stage == stage && order.ReasonCode == reason {
			matched = append(matched, order)
		}
	}
	return matched
}

func (f *preReview) blocked(t *testing.T) []map[string]any {
	t.Helper()
	events, err := f.st.ListEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []map[string]any
	for _, event := range events {
		if event.Kind != "merge.blocked" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func (f *preReview) current(t *testing.T) core.Task {
	t.Helper()
	task, err := f.st.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// assertRoutedToFix checks one conflict episode for head with a fix order
// baselined on that unreviewed head, no approval bound, and the submission
// stamped as checked.
func (f *preReview) assertRoutedToFix(t *testing.T, head string) core.WorkOrder {
	t.Helper()
	if review := f.orders(t, core.StageReview, ""); len(review) != 0 {
		t.Fatalf("review orders created for a conflicting head: %+v", review)
	}
	if verify := f.orders(t, core.StageVerify, ""); len(verify) != 0 {
		t.Fatalf("verify orders created for a conflicting head: %+v", verify)
	}
	fixes := f.orders(t, core.StageImplement, "merge-conflict")
	if len(fixes) != 1 || fixes[0].BaselineSHA != head {
		t.Fatalf("conflict-fix orders = %+v, want one baselined on %s", fixes, head)
	}
	blocked := f.blocked(t)
	if len(blocked) != 1 || blocked[0]["reason_code"] != "pre-review-conflict" || blocked[0]["new_head"] != head || blocked[0]["approved_head"] != "" || blocked[0]["work_order_id"] != f.task.ID+"-implement-1" {
		t.Fatalf("merge.blocked = %+v", blocked)
	}
	if task := f.current(t); task.ApprovedHeadSHA != "" || task.ApprovalStale {
		t.Fatalf("pre-review conflict bound an approval: %+v", task)
	}
	submission, err := f.st.GetWorkOrder(f.ctx, f.task.ID+"-implement-1")
	if err != nil {
		t.Fatal(err)
	}
	if submission.BaseConflictCheckedAt.IsZero() {
		t.Fatal("submission was not stamped as base-checked")
	}
	return fixes[0]
}

func TestPreReviewConflictDispatchesFixInsteadOfReview(t *testing.T) {
	f := newPreReview(t, false)
	f.mergeable = func(int) string { return "CONFLICTING" }
	f.submit(t, f.task.ID+"-implement-1", "implementer", "head-a")
	f.assertRoutedToFix(t, "head-a")
	if f.reads != 1 {
		t.Fatalf("reads = %d, want 1", f.reads)
	}
	// A redelivery of the same task neither re-reads nor opens a second episode.
	f.d.Enqueue(f.ctx, f.task.ID)
	f.dispatch(t)
	f.dispatch(t)
	f.assertRoutedToFix(t, "head-a")
	if f.reads != 1 {
		t.Fatalf("redelivery reads = %d, want 1", f.reads)
	}
}

func TestPreReviewConflictDispatchesFixInsteadOfVerify(t *testing.T) {
	f := newPreReview(t, true)
	f.mergeable = func(int) string { return "CONFLICTING" }
	f.submit(t, f.task.ID+"-implement-1", "implementer", "head-a")
	f.assertRoutedToFix(t, "head-a")
	if f.reads != 1 {
		t.Fatalf("reads = %d, want 1", f.reads)
	}
}

// TestPreReviewCheckRunsOncePerSubmission redelivers a clean head's review
// dispatch: the stamped submission is never read again.
func TestPreReviewCheckRunsOncePerSubmission(t *testing.T) {
	f := newPreReview(t, false)
	f.submit(t, f.task.ID+"-implement-1", "implementer", "head-a")
	rounds := len(f.orders(t, core.StageReview, ""))
	if rounds == 0 || f.reads != 1 {
		t.Fatalf("review orders = %d reads = %d, want a round after one read", rounds, f.reads)
	}
	f.dispatch(t)
	f.dispatch(t)
	if f.reads != 1 {
		t.Fatalf("redelivered dispatch re-read the pull request: reads = %d", f.reads)
	}
	if got := len(f.orders(t, core.StageReview, "")); got != rounds {
		t.Fatalf("review orders = %d after redelivery, want %d", got, rounds)
	}
	if blocked := f.blocked(t); len(blocked) != 0 {
		t.Fatalf("clean head recorded merge.blocked: %+v", blocked)
	}
}

func TestPreReviewUnknownDispatchesAsToday(t *testing.T) {
	t.Run("review", func(t *testing.T) {
		f := newPreReview(t, false)
		f.mergeable = func(int) string { return "UNKNOWN" }
		f.submit(t, f.task.ID+"-implement-1", "implementer", "head-a")
		if f.reads != 3 {
			t.Fatalf("reads = %d, want 3", f.reads)
		}
		review := f.orders(t, core.StageReview, "")
		if len(review) == 0 || review[0].HeadSHA != "head-a" {
			t.Fatalf("review orders = %+v", review)
		}
		if blocked := f.blocked(t); len(blocked) != 0 {
			t.Fatalf("UNKNOWN recorded merge.blocked: %+v", blocked)
		}
	})
	t.Run("verify then review", func(t *testing.T) {
		f := newPreReview(t, true)
		f.mergeable = func(int) string { return "UNKNOWN" }
		f.submit(t, f.task.ID+"-implement-1", "implementer", "head-a")
		if f.reads != 3 {
			t.Fatalf("reads = %d, want 3", f.reads)
		}
		verify := f.orders(t, core.StageVerify, "")
		if len(verify) != 1 || verify[0].HeadSHA != "head-a" {
			t.Fatalf("verify orders = %+v", verify)
		}
		// Verification of the same head completes; the review dispatch that
		// follows must not read the pull request again.
		claimed, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, verify[0].ID, core.WorkOrderClaim{SessionID: "verifier", ClientToken: "verify-token", ClaimantID: core.TaskRunClaimantID("usr-verifier"), OwnerUserID: "usr-verifier", Lease: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		claimed.State, claimed.VerificationContextID = core.WorkOrderCompleted, "verification-context"
		if err = storetest.For(f.st).UpdateWorkOrder(f.ctx, claimed); err != nil {
			t.Fatal(err)
		}
		if _, err := taskops.New(f.st).Perform(f.ctx, f.task.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		f.dispatch(t)
		if f.reads != 3 {
			t.Fatalf("verify-to-review dispatch re-read: reads = %d, want 3", f.reads)
		}
		if review := f.orders(t, core.StageReview, ""); len(review) == 0 || review[0].HeadSHA != "head-a" {
			t.Fatalf("review orders = %+v", review)
		}
	})
}

func TestPreReviewUnreadableOrLaggingHeadDispatchesAsToday(t *testing.T) {
	for _, test := range []struct {
		name    string
		viewErr error
		prHead  string
	}{
		{name: "forge error", viewErr: &githubtrigger.Error{Category: githubtrigger.ForgeRequest, Err: errors.New("request timed out")}},
		{name: "lagging head", prHead: "older-head"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPreReview(t, false)
			f.mergeable = func(int) string { return "CONFLICTING" }
			f.submitHead, f.viewErr = "head-a", test.viewErr
			if _, err := f.service.SubmitForReview(f.ctx, f.task.ID+"-implement-1", "implementer", "head-a"); err != nil {
				t.Fatal(err)
			}
			f.prHead = test.prHead
			f.dispatch(t)
			if review := f.orders(t, core.StageReview, ""); len(review) == 0 || review[0].HeadSHA != "head-a" {
				t.Fatalf("review orders = %+v", review)
			}
			if fixes := f.orders(t, core.StageImplement, "merge-conflict"); len(fixes) != 0 {
				t.Fatalf("conflict fix dispatched: %+v", fixes)
			}
			if blocked := f.blocked(t); len(blocked) != 0 {
				t.Fatalf("merge.blocked recorded: %+v", blocked)
			}
		})
	}
}

// TestPreReviewFixHeadDispatchesReview submits the pre-review fix as an
// ordinary submission: its head is checked once and goes to review with no
// stale approval.
func TestPreReviewFixHeadDispatchesReview(t *testing.T) {
	f := newPreReview(t, false)
	f.mergeable = func(int) string { return "CONFLICTING" }
	f.submit(t, f.task.ID+"-implement-1", "implementer", "head-a")
	fix := f.assertRoutedToFix(t, "head-a")
	if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, fix.ID, core.WorkOrderClaim{SessionID: "fixer", ClientToken: "fix-token", ClaimantID: core.TaskRunClaimantID("usr-fixer"), OwnerUserID: "usr-fixer", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	f.mergeable = func(int) string { return "MERGEABLE" }
	f.submit(t, fix.ID, "fixer", "head-b")
	if f.reads != 2 {
		t.Fatalf("reads = %d, want 2", f.reads)
	}
	review := f.orders(t, core.StageReview, "")
	if len(review) == 0 || review[0].HeadSHA != "head-b" {
		t.Fatalf("review orders = %+v, want a round for head-b", review)
	}
	if task := f.current(t); task.ApprovalStale || task.ApprovedHeadSHA != "" {
		t.Fatalf("fix submission marked the task approval-stale: %+v", task)
	}
	stamped, err := f.st.GetWorkOrder(f.ctx, fix.ID)
	if err != nil || stamped.BaseConflictCheckedAt.IsZero() {
		t.Fatalf("fix submission not base-checked: %+v err=%v", stamped, err)
	}
}

// TestReadinessRefreshSkipsPreDispatchCheck covers the readiness path that
// begins a refresh while it holds the task lock: it dispatches verify without
// a second read and without re-entering that lock.
func TestReadinessRefreshSkipsPreDispatchCheck(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	cfg := &config.Config{Workspace: "test", WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"review": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		"verify": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
	}}}
	cfg.Execution.VerifyStage = true
	task := core.Task{ID: "refresh-verify", Workspace: "test", Repo: "app", Branch: "conveyor/refresh-verify", BaseBranch: "main", State: core.TaskApproved, PolicyVersion: 1, SetupContract: cfg.FreezePolicy(), CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.BindTaskApproval(ctx, task.ID, "head-a"); err != nil {
		t.Fatal(err)
	}
	// A submission row for the pushed head exists, so an enabled pre-check
	// would claim and read it.
	job := core.Job{ID: task.ID + "-implement-2", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	submission, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "token", ClaimantID: core.TaskRunClaimantID("usr-impl"), OwnerUserID: "usr-impl", Lease: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	submission.State, submission.HeadSHA = core.WorkOrderSubmitted, "head-b"
	if err = storetest.For(st).UpdateWorkOrder(ctx, submission); err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	reads := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		reads++
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head-b"}, nil
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, readErr := d.ReadMergeReadiness(ctx, current)
		done <- readErr
	}()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readiness refresh deadlocked")
	}
	if reads != 1 {
		t.Fatalf("reads = %d, want only the readiness read", reads)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	verify := 0
	for _, order := range orders {
		if order.Stage == core.StageVerify && order.HeadSHA == "head-b" {
			verify++
		}
	}
	if verify != 1 {
		t.Fatalf("verify orders for head-b = %d, orders=%+v", verify, orders)
	}
	if stamped, err := st.GetWorkOrder(ctx, task.ID+"-implement-2"); err != nil || !stamped.BaseConflictCheckedAt.IsZero() {
		t.Fatalf("readiness refresh stamped the submission: %+v err=%v", stamped, err)
	}
}
