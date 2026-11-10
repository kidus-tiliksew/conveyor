package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Tests for the conveyor/review-policy status and recorded merge bypasses
// (req-risk-based-review AC-10.1, AC-10.2, AC-10.3; component-git-delivery
// invariant 37; component-submission-merge invariants 49, 59, 65, 66;
// feature-risk-based-review RR-9).

// policyForge records each status write, and the order of status writes and
// merge requests.
type policyForge struct {
	mu    sync.Mutex
	calls []githubtrigger.PolicyStatusPublication
	steps []string
	fail  error
}

func (f *policyForge) publish(_ context.Context, publication githubtrigger.PolicyStatusPublication) (githubtrigger.PolicyStatusResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, publication)
	f.steps = append(f.steps, "status:"+publication.State)
	if f.fail != nil {
		return githubtrigger.PolicyStatusResult{}, f.fail
	}
	return githubtrigger.PolicyStatusResult{Written: true}, nil
}

func (f *policyForge) states() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		out = append(out, call.HeadSHA+"="+call.State)
	}
	return out
}

// withPolicyForge installs a recording publisher and records merge requests
// in the same step log.
func withPolicyForge(d *Dispatcher, forge *gateForge) *policyForge {
	policy := &policyForge{}
	d.PublishPolicyStatus = policy.publish
	d.RequestMerge = func(ctx context.Context, repo string, number int) error {
		policy.mu.Lock()
		policy.steps = append(policy.steps, "merge")
		policy.mu.Unlock()
		return forge.merge(ctx, repo, number)
	}
	return policy
}

func policyJob(taskID, head string, attempt int) queue.Job {
	return testJob(queue.ReviewPolicyStatusArgs{WorkspaceID: "test", TaskID: taskID, HeadSHA: head}, int64(attempt), attempt, 5)
}

func policyRow(t *testing.T, st store.Store, ctx context.Context, taskID, head string) core.ReviewPolicyStatus {
	t.Helper()
	row, ok, err := st.GetReviewPolicyStatus(ctx, taskID, head)
	if err != nil || !ok {
		t.Fatalf("policy status of %s at %s ok=%t err=%v", taskID, head, ok, err)
	}
	return row
}

func runPolicyJob(t *testing.T, ctx context.Context, d *Dispatcher, taskID, head string, attempt int) error {
	t.Helper()
	return (&reviewPolicyStatusWorker{dispatcher: d}).Work(ctx, policyJob(taskID, head, attempt))
}

// A rated head publishes pending while it lacks a required approval, and
// success once a person's approval completes it. A repeated job writes
// nothing (AC-10.1; component-git-delivery invariant 37).
func TestPolicyStatusPendingThenSuccess(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-pending", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	policy := withPolicyForge(d, forge)
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusQueued || row.Desired != core.PolicyStatusPending {
		t.Fatalf("rated head row=%+v", row)
	}
	if err := runPolicyJob(t, ctx, d, task.ID, gateHead, 1); err != nil {
		t.Fatal(err)
	}
	row := policyRow(t, st, ctx, task.ID, gateHead)
	if row.State != core.ReviewPolicyStatusPublished || row.Published != core.PolicyStatusPending || row.Attempts != 1 {
		t.Fatalf("published row=%+v", row)
	}
	if got := policy.states(); len(got) != 1 || got[0] != gateHead+"=pending" {
		t.Fatalf("writes=%v", got)
	}
	if call := policy.calls[0]; call.Repo != "acme/app" || call.Branch != task.Branch {
		t.Fatalf("publication=%+v", call)
	}
	// A repeated job for an unchanged head makes no forge call.
	if err := runPolicyJob(t, ctx, d, task.ID, gateHead, 1); err != nil {
		t.Fatal(err)
	}
	if got := policy.states(); len(got) != 1 {
		t.Fatalf("a repeat publication wrote again: %v", got)
	}
	// An approval completes the medium tier's one human approval.
	if _, err := d.RecordApproval(ctx, task, gateOperator, gateHead); err != nil {
		t.Fatalf("approval err=%v", err)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusQueued || row.Desired != core.PolicyStatusSuccess {
		t.Fatalf("after the approval row=%+v", row)
	}
	if err := runPolicyJob(t, ctx, d, task.ID, gateHead, 1); err != nil {
		t.Fatal(err)
	}
	if got := policy.states(); len(got) != 2 || got[1] != gateHead+"=success" {
		t.Fatalf("writes=%v", got)
	}
	if n := countKind(eventKinds(t, st, ctx, task.ID), core.ReviewPolicyStatusPublishedEvent); n != 2 {
		t.Fatalf("published events=%d", n)
	}
}

// The handler re-reads the requirements before its forge call: a job queued
// for success publishes pending once a raise leaves the head short, and an
// earlier head never keeps success after a new head is rated (component-git-
// delivery invariant 37).
func TestPolicyStatusDelayedJobPublishesCurrentState(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-delayed", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	policy := withPolicyForge(d, forge)
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.Desired != core.PolicyStatusSuccess {
		t.Fatalf("low tier with a merge gate off row=%+v", row)
	}
	// The raise lands before the queued success job runs.
	raiseTier(t, ctx, st, task.ID, riskpolicy.TierHigh)
	if err := runPolicyJob(t, ctx, d, task.ID, gateHead, 1); err != nil {
		t.Fatal(err)
	}
	if got := policy.states(); len(got) != 1 || got[0] != gateHead+"=pending" {
		t.Fatalf("a delayed job wrote %v, want pending", got)
	}
	// A live trust edit applies at the next evaluation (AC-9.3).
	claimImplementation(t, st, ctx, task.ID, "usr-author")
	setTrustedAuthors(d, "usr-other")
	evaluation, err := d.EvaluatePolicyAtHead(ctx, task, gateHead, "")
	if err != nil || evaluation.Met || !evaluation.Approvals.Status.NonAuthorApprovalNeeded {
		t.Fatalf("untrusted author evaluation=%+v err=%v", evaluation, err)
	}
	// A new rated head returns the old head to pending; old heads borrow no
	// approvals or reviews of the new head.
	if _, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: task.ID, Key: core.PathRiskKey("new-head"), Source: core.RiskSourcePathRule, Tier: riskpolicy.TierLow,
		HeadSHA: "new-head", BaseSHA: "base", DeclarationState: riskpolicy.DeclarationPresent, Reason: "path rule"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SyncReviewPolicyStatus(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if row := policyRow(t, st, ctx, task.ID, "new-head"); row.Desired != core.PolicyStatusPending {
		t.Fatalf("an unreviewed new head row=%+v", row)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.Desired != core.PolicyStatusPending {
		t.Fatalf("old head row=%+v", row)
	}
}

// A failed write retries and leaves the row failed on the last attempt; it
// changes no rating, verdict, or approval, and the next evaluation reopens it
// (DEC-117(1)).
func TestPolicyStatusFailureRetriesAndReopens(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-failure", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	policy := withPolicyForge(d, forge)
	policy.fail = githubtrigger.StatusError(502, errors.New("bad gateway"))
	if err := runPolicyJob(t, ctx, d, task.ID, gateHead, 1); err == nil {
		t.Fatal("a failed write reported success")
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusRetrying || row.LastError == "" || row.ForgeErrorCategory == "" {
		t.Fatalf("retrying row=%+v", row)
	}
	if err := runPolicyJob(t, ctx, d, task.ID, gateHead, 5); err == nil {
		t.Fatal("the last failed attempt reported success")
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusFailed {
		t.Fatalf("after the last attempt row=%+v", row)
	}
	if approvals, _ := st.ListTaskApprovals(ctx, task.ID); len(approvals) != 0 {
		t.Fatalf("a failed status changed approvals: %+v", approvals)
	}
	// The start-up pass reopens the failed row and reports one repair.
	var repairs []store.DriftRepair
	recorded := store.WithDriftRecorder(ctx, func(repair store.DriftRepair) { repairs = append(repairs, repair) })
	if n, err := d.ReconcileReviewPolicyStatuses(recorded); err != nil || n != 1 || len(repairs) != 1 || repairs[0].Pass != store.DriftPassReviewPolicyStatuses {
		t.Fatalf("reconcile n=%d repairs=%+v err=%v", n, repairs, err)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusQueued {
		t.Fatalf("reopened row=%+v", row)
	}
	policy.fail = nil
	if err := runPolicyJob(t, ctx, d, task.ID, gateHead, 1); err != nil {
		t.Fatal(err)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusPublished || row.Published != core.PolicyStatusPending {
		t.Fatalf("recovered row=%+v", row)
	}
	// A second start finds nothing to repair.
	repairs = nil
	if n, err := d.ReconcileReviewPolicyStatuses(recorded); err != nil || n != 0 || len(repairs) != 0 {
		t.Fatalf("second reconcile n=%d repairs=%+v err=%v", n, repairs, err)
	}
	// A discarded job leaves an open row failed, and a published row alone.
	if err := (&reviewPolicyStatusWorker{dispatcher: d}).Discarded(ctx, policyJob(task.ID, gateHead, 5), "rescued"); err != nil {
		t.Fatal(err)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusPublished {
		t.Fatalf("discard changed a published row: %+v", row)
	}
}

// The start-up pass inserts the missing intent of a rated head, as a crash
// between a rating and its re-evaluation leaves it, and reports one repair.
func TestPolicyStatusStartupInsertsMissingIntent(t *testing.T) {
	ctx, st, task, d, _ := riskGateFixture(t, "policy-startup", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	crashed := task
	crashed.ID, crashed.Branch, crashed.State, crashed.GateKind = "policy-startup-crash", "conveyor/policy-startup-crash", core.TaskRunning, ""
	if err := st.CreateTask(ctx, crashed); err != nil {
		t.Fatal(err)
	}
	recordPathRating(t, st, ctx, crashed.ID, riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	if rows, err := st.ListReviewPolicyStatuses(ctx, crashed.ID); err != nil || len(rows) != 0 {
		t.Fatalf("crashed rows=%+v err=%v", rows, err)
	}
	var repairs []store.DriftRepair
	recorded := store.WithDriftRecorder(ctx, func(repair store.DriftRepair) { repairs = append(repairs, repair) })
	n, err := d.ReconcileReviewPolicyStatuses(recorded)
	if err != nil || n != 1 || len(repairs) != 1 || repairs[0].TaskID != crashed.ID || repairs[0] != store.DriftReviewPolicyStatusStale.For(crashed.ID) {
		t.Fatalf("reconcile n=%d repairs=%+v err=%v", n, repairs, err)
	}
	if row := policyRow(t, st, ctx, crashed.ID, gateHead); row.State != core.ReviewPolicyStatusQueued || row.Desired != core.PolicyStatusPending {
		t.Fatalf("repaired row=%+v", row)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.Revision != 1 {
		t.Fatalf("an unchanged task was written again: %+v", row)
	}
}

// Off and record tasks publish nothing and read no requirement
// (AC-1.1, AC-1.6).
func TestPolicyStatusOffAndRecordPublishNothing(t *testing.T) {
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeOff, riskpolicy.ModeRecord} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, st, task, d, forge := riskApprovedTask(t, "policy-"+string(mode), riskMergeCase{mode, riskpolicy.TierHigh, 0})
			policy := withPolicyForge(d, forge)
			if err := d.SyncReviewPolicyStatus(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.MergeApprovedTaskAutomatically(ctx, task); err != nil {
				t.Fatalf("automatic merge err=%v", err)
			}
			if rows, _ := st.ListReviewPolicyStatuses(ctx, task.ID); len(rows) != 0 || len(policy.calls) != 0 || forge.merges != 1 {
				t.Fatalf("rows=%+v writes=%v merges=%d", rows, policy.states(), forge.merges)
			}
			if n, err := d.ReconcileReviewPolicyStatuses(ctx); err != nil || n != 0 {
				t.Fatalf("reconcile n=%d err=%v", n, err)
			}
		})
	}
}

// The automatic merge publishes success before its merge request
// (AC-10.2; component-submission-merge invariant 65).
func TestAutomaticMergePublishesSuccessFirst(t *testing.T) {
	ctx, st, task, d, forge := riskApprovedTask(t, "policy-automatic", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	policy := withPolicyForge(d, forge)
	if err := d.MergeApprovedTaskAutomatically(ctx, task); err != nil {
		t.Fatalf("automatic merge err=%v", err)
	}
	if strings.Join(policy.steps, ",") != "status:success,merge" {
		t.Fatalf("steps=%v, want the success status before the merge request", policy.steps)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskMerged {
		t.Fatalf("task=%s", current.State)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusPublished || row.Published != core.PolicyStatusSuccess {
		t.Fatalf("row=%+v", row)
	}
}

// A failed success publication on the automatic path sends no merge request,
// records policy_status_unpublished, and leaves the task approved for a later
// readiness read.
func TestAutomaticMergeHoldsOnUnpublishedStatus(t *testing.T) {
	ctx, st, task, d, forge := riskApprovedTask(t, "policy-automatic-fail", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	policy := withPolicyForge(d, forge)
	policy.fail = githubtrigger.StatusError(502, errors.New("bad gateway"))
	err := d.MergeApprovedTaskAutomatically(ctx, task)
	if !errors.Is(err, ErrPolicyStatusUnpublished) || forge.merges != 0 {
		t.Fatalf("err=%v merges=%d", err, forge.merges)
	}
	if errors.Is(err, errMergeIneligible) {
		t.Fatal("an unpublished status was treated as a definitive refusal, so no retry would merge")
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskApproved {
		t.Fatalf("task=%s, want approved", current.State)
	}
	if payload := lastPayload(t, st, ctx, task.ID, "merge.failed"); payload["reason_code"] != "policy_status_unpublished" {
		t.Fatalf("merge.failed=%v", payload)
	}
	if n := countKind(eventKinds(t, st, ctx, task.ID), "merge.requested"); n != 0 {
		t.Fatalf("merge.requested=%d", n)
	}
	// A later readiness read tries again once GitHub accepts the status.
	policy.fail = nil
	if err := d.MergeApprovedTaskAutomatically(ctx, task); err != nil || forge.merges != 1 {
		t.Fatalf("retry err=%v merges=%d", err, forge.merges)
	}
}

// At the gate, the person whose approval completes the count makes the
// success status count that provisional approval, and the approval is
// recorded only with the confirmed merge (AC-10.2, AC-8.7).
func TestGateMergePublishesSuccessWithTheProvisionalApproval(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-gate", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	policy := withPolicyForge(d, forge)
	var approvalsAtPublish int
	d.PublishPolicyStatus = func(ctx context.Context, publication githubtrigger.PolicyStatusPublication) (githubtrigger.PolicyStatusResult, error) {
		if publication.State == core.PolicyStatusSuccess {
			approvals, _ := st.ListTaskApprovals(ctx, task.ID)
			approvalsAtPublish = len(approvals)
		}
		return policy.publish(ctx, publication)
	}
	out, err := d.MergeAtGate(ctx, task, gateOperator)
	if err != nil || out.State != core.TaskMerged {
		t.Fatalf("gate merge out=%s err=%v", out.State, err)
	}
	if strings.Join(policy.steps, ",") != "status:success,merge" || approvalsAtPublish != 0 {
		t.Fatalf("steps=%v approvals at publish=%d", policy.steps, approvalsAtPublish)
	}
	if approvals, _ := st.ListTaskApprovals(ctx, task.ID); len(approvals) != 1 || approvals[0].UserID != "usr-approver" {
		t.Fatalf("approvals=%+v", approvals)
	}
}

// A failed publication at the gate sends no merge request, records no
// approval, and leaves the gate pending; an approval recorded earlier stays
// (AC-8.7; component-submission-merge invariant 65).
func TestGateMergeUnpublishedStatusRecordsNoApproval(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-gate-fail", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierHigh, 0})
	policy := withPolicyForge(d, forge)
	if _, err := d.MergeAtGate(ctx, task, person("usr-first")); err == nil {
		t.Fatal("the first of two approvals merged")
	}
	policy.fail = githubtrigger.StatusError(502, errors.New("bad gateway"))
	_, err := d.MergeAtGate(ctx, task, gateOperator)
	if !errors.Is(err, ErrPolicyStatusUnpublished) || forge.merges != 0 {
		t.Fatalf("err=%v merges=%d", err, forge.merges)
	}
	if !MergeRouteRefusal(err) {
		t.Fatal("the route would answer an unpublished status with a server error")
	}
	approvals, _ := st.ListTaskApprovals(ctx, task.ID)
	if len(approvals) != 1 || approvals[0].UserID != "usr-first" {
		t.Fatalf("approvals=%+v, want only the earlier approval", approvals)
	}
	assertGatePendingState(t, st, ctx, task.ID)
	if payload := lastPayload(t, st, ctx, task.ID, "merge.failed"); payload["reason_code"] != "policy_status_unpublished" {
		t.Fatalf("merge.failed=%v", payload)
	}
	// The status follows what the head has after the refused act: pending.
	row := policyRow(t, st, ctx, task.ID, gateHead)
	if row.Desired != core.PolicyStatusPending {
		t.Fatalf("row after the refused act=%+v", row)
	}
}

func bypassPayload(t *testing.T, st store.Store, ctx context.Context, taskID string) core.MergeBypass {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var found []core.MergeBypass
	for _, event := range events {
		if event.Kind == core.MergeBypassRecordedEvent {
			var bypass core.MergeBypass
			if err := json.Unmarshal(event.Payload, &bypass); err != nil {
				t.Fatal(err)
			}
			found = append(found, bypass)
		}
	}
	if len(found) != 1 {
		t.Fatalf("bypass events=%+v, want one", found)
	}
	return found[0]
}

func assertBypassCompletion(t *testing.T, st store.Store, ctx context.Context, taskID string) {
	t.Helper()
	current, _ := st.GetTask(ctx, taskID)
	if current.State != core.TaskMerged || lastStateCommand(t, st, ctx, taskID) != core.TaskMergeRecover {
		t.Fatalf("task=%s command=%s", current.State, lastStateCommand(t, st, ctx, taskID))
	}
	if items, _ := st.ListInterventions(ctx, taskID); len(items) != 0 {
		t.Fatalf("a bypass credited interventions=%+v", items)
	}
}

// A GitHub merge observed by the webhook, start-up, or timed route while the
// count is unmet completes the task, credits no approval, and records one
// bypass naming the actor, the head, and what was missing (AC-10.3;
// component-submission-merge invariant 66).
func TestObservedMergeShortOfTheCountRecordsBypass(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-bypass", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierHigh, 0})
	if _, err := d.MergeAtGate(ctx, task, person("usr-first")); err == nil {
		t.Fatal("the first of two approvals merged")
	}
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "octo-merger"
	system := store.WithActor(ctx, store.SystemActor())
	if matched, err := d.ReconcileObservedPullRequest(system, "app", "acme/app", task.ID, forge.pr); err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	assertBypassCompletion(t, st, ctx, task.ID)
	bypass := bypassPayload(t, st, ctx, task.ID)
	if bypass.ForgeActor != "octo-merger" || bypass.HeadSHA != gateHead || bypass.RequiredHumanApprovals != 2 || bypass.RecordedHumanApprovals != 1 ||
		bypass.MissingHumanApprovals != 1 || bypass.MissingAgentReviews != 0 || bypass.Tier != string(riskpolicy.TierHigh) {
		t.Fatalf("bypass=%+v", bypass)
	}
	if approvals, _ := st.ListTaskApprovals(ctx, task.ID); len(approvals) != 1 {
		t.Fatalf("a bypass credited approvals=%+v", approvals)
	}
	// A replayed observation records nothing more.
	if matched, err := d.ReconcileObservedPullRequest(system, "app", "acme/app", task.ID, forge.pr); err != nil || !matched {
		t.Fatalf("replay matched=%v err=%v", matched, err)
	}
	bypassPayload(t, st, ctx, task.ID)
	// The inbox acts log lists the bypass for its task, not as the
	// factory's own act.
	acts, err := st.ListFactoryActs(ctx, task.CreatedAt.Add(-1), 100)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, act := range acts {
		if act.Kind == core.MergeBypassRecordedEvent && act.TaskID == task.ID && !act.Automatic && strings.Contains(act.Summary, "octo-merger") {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("acts=%+v", acts)
	}
}

// A merged-head-only read by a person, the readiness read here, completes the
// same way, and an unknown GitHub account is named unknown.
func TestReadinessReadRecordsBypassWithUnknownActor(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-bypass-read", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", ""
	readiness, err := d.ReadMergeReadiness(ctx, task)
	if err != nil || readiness.State != "MERGED" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	assertBypassCompletion(t, st, ctx, task.ID)
	if bypass := bypassPayload(t, st, ctx, task.ID); bypass.ForgeActor != core.UnknownForgeActor || bypass.MissingHumanApprovals != 1 || !bypass.TrustedAuthorApprovalNeeded && bypass.RecordedHumanApprovals != 0 {
		t.Fatalf("bypass=%+v", bypass)
	}
}

// A pending gate merge request at the merged head whose person's approval
// would not complete the count, because the tier rose after the request,
// credits no intervention.merge and records the bypass; a request whose
// person completes the count credits as before.
func TestObservedPendingGateRequestCreditsOnlyAMetCount(t *testing.T) {
	t.Run("Unmet", func(t *testing.T) {
		ctx, st, task, d, forge := riskGateFixture(t, "policy-pending-unmet", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
		withPolicyForge(d, forge)
		forge.answer = func(*githubtrigger.PullRequest) error { return errors.New("connection reset") }
		_, _ = d.MergeAtGate(ctx, task, gateOperator)
		raiseTier(t, ctx, st, task.ID, riskpolicy.TierHigh)
		forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "conveyor-app"
		if matched, err := d.ReconcileObservedPullRequest(store.WithActor(ctx, store.SystemActor()), "app", "acme/app", task.ID, forge.pr); err != nil || !matched {
			t.Fatalf("matched=%v err=%v", matched, err)
		}
		assertBypassCompletion(t, st, ctx, task.ID)
		if approvals, _ := st.ListTaskApprovals(ctx, task.ID); len(approvals) != 0 {
			t.Fatalf("approvals=%+v", approvals)
		}
		if bypass := bypassPayload(t, st, ctx, task.ID); bypass.RequiredHumanApprovals != 2 || bypass.RecordedHumanApprovals != 1 || bypass.MissingAgentReviews != 0 {
			t.Fatalf("bypass=%+v", bypass)
		}
	})
	t.Run("Met", func(t *testing.T) {
		ctx, st, task, d, forge := riskGateFixture(t, "policy-pending-met", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
		withPolicyForge(d, forge)
		forge.answer = func(*githubtrigger.PullRequest) error { return errors.New("connection reset") }
		_, _ = d.MergeAtGate(ctx, task, gateOperator)
		forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "conveyor-app"
		if matched, err := d.ReconcileObservedPullRequest(store.WithActor(ctx, store.SystemActor()), "app", "acme/app", task.ID, forge.pr); err != nil || !matched {
			t.Fatalf("matched=%v err=%v", matched, err)
		}
		assertGateMergedBy(t, st, ctx, task.ID, "usr-approver")
		if n := countKind(eventKinds(t, st, ctx, task.ID), core.MergeBypassRecordedEvent); n != 0 {
			t.Fatalf("a met count recorded %d bypasses", n)
		}
	})
}

// The approve act's reread of a merged pull request with a pending request
// goes through the same evaluation: short of the count it records the
// bypass and no approval.
func TestApproveRereadRecordsBypass(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-approve-reread", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	withPolicyForge(d, forge)
	forge.answer = func(*githubtrigger.PullRequest) error { return errors.New("connection reset") }
	_, _ = d.MergeAtGate(ctx, task, gateOperator)
	raiseTier(t, ctx, st, task.ID, riskpolicy.TierHigh)
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "octo-merger"
	current, _ := st.GetTask(ctx, task.ID)
	credited, err := d.creditVisibleGateMergeLocked(ctx, current)
	if err != nil || !credited {
		t.Fatalf("credited=%t err=%v", credited, err)
	}
	assertBypassCompletion(t, st, ctx, task.ID)
	bypassPayload(t, st, ctx, task.ID)
}

// A met count observed on GitHub completes as before, with no bypass, and an
// off or record task keeps DEC-66 behavior with no evaluation.
func TestObservedMergeWithMetOrUnenforcedRequirementsRecordsNoBypass(t *testing.T) {
	for name, tc := range map[string]riskMergeCase{
		"EnforceLowTier": {riskpolicy.ModeEnforce, riskpolicy.TierLow, 0},
		"Record":         {riskpolicy.ModeRecord, riskpolicy.TierHigh, 0},
		"Off":            {riskpolicy.ModeOff, riskpolicy.TierHigh, 0},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, st, task, d, forge := riskGateFixture(t, "policy-met-"+strings.ToLower(name), tc)
			observeOutside(t, ctx, d, forge, task.ID)
			assertRecoveredOutside(t, st, ctx, task.ID)
			if n := countKind(eventKinds(t, st, ctx, task.ID), core.MergeBypassRecordedEvent); n != 0 {
				t.Fatalf("bypasses=%d", n)
			}
		})
	}
}

// A head that its latest round did not approve is missing its agent reviews,
// and the bypass names them.
func TestObservedMergeWithoutAgentReviewsNamesThem(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-no-reviews", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	settleReviewRound(t, st, ctx, task.ID, 2, "another-head", 1)
	observeOutside(t, ctx, d, forge, task.ID)
	if bypass := bypassPayload(t, st, ctx, task.ID); bypass.MissingAgentReviews != 1 || bypass.RecordedAgentReviews != 0 || bypass.MissingHumanApprovals != 0 {
		t.Fatalf("bypass=%+v", bypass)
	}
}

// Concurrent observations of one merge complete the task once and record
// one bypass.
func TestConcurrentObservationsRecordOneBypass(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "policy-concurrent", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierHigh, 0})
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "octo-merger"
	system := store.WithActor(ctx, store.SystemActor())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.ReconcileObservedPullRequest(system, "app", "acme/app", task.ID, forge.pr)
		}()
	}
	wg.Wait()
	assertBypassCompletion(t, st, ctx, task.ID)
	bypassPayload(t, st, ctx, task.ID)
}

// A merge whose head's row already records success still asks GitHub first.
// A refused recheck sends no merge request and leaves the row failed, and the
// re-evaluation after the act reopens it with a job.
func TestMergeRechecksAPublishedSuccess(t *testing.T) {
	ctx, st, task, d, forge := riskApprovedTask(t, "policy-recheck", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	policy := withPolicyForge(d, forge)
	if err := runPolicyJob(t, ctx, d, task.ID, gateHead, 1); err != nil {
		t.Fatal(err)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusPublished || row.Published != core.PolicyStatusSuccess {
		t.Fatalf("row=%+v", row)
	}
	policy.fail = githubtrigger.StatusError(502, errors.New("bad gateway"))
	if err := d.MergeApprovedTaskAutomatically(ctx, task); !errors.Is(err, ErrPolicyStatusUnpublished) || forge.merges != 0 {
		t.Fatalf("err=%v merges=%d", err, forge.merges)
	}
	if row := policyRow(t, st, ctx, task.ID, gateHead); row.State != core.ReviewPolicyStatusQueued || row.LastError != "" {
		t.Fatalf("after the refused recheck row=%+v, want reopened", row)
	}
	policy.fail = nil
	if err := d.MergeApprovedTaskAutomatically(ctx, task); err != nil || forge.merges != 1 {
		t.Fatalf("retry err=%v merges=%d", err, forge.merges)
	}
	if got := policy.states(); len(got) != 3 || strings.Join(policy.steps, ",") != "status:success,status:success,status:success,merge" {
		t.Fatalf("writes=%v steps=%v, want the job, the refused recheck, and the retry before its merge", got, policy.steps)
	}
}
