package dispatch

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Tests for counted human approvals, authors, and trust on enforcing tasks
// (req-risk-based-review REQ-8, REQ-9, AC-8.3 to AC-8.8, AC-9.1 to AC-9.3;
// req-review-gates-evidence AC-1.3; feature-risk-based-review RR-7).

func person(id string) core.Actor {
	return core.Actor{ID: store.UserActorID(id), Role: core.ActorUser}
}

// claimImplementation records that userID's session claimed an
// implementation order of the task, which makes the person an author.
func claimImplementation(t *testing.T, st store.Store, ctx context.Context, taskID, userID string) {
	t.Helper()
	order := core.WorkOrder{ID: taskID + "-implement-" + userID, TaskID: taskID, Stage: core.StageImplement, ClaimantID: core.TaskRunClaimantID(userID)}
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "work_order.claimed", Payload: store.WorkOrderEventPayload(order)}); err != nil {
		t.Fatal(err)
	}
}

func setTrustedAuthors(d *Dispatcher, trusted ...string) {
	d.Cfg = &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app", TrustedAuthors: trusted}}}
}

func approvalCount(t *testing.T, d *Dispatcher, ctx context.Context, st store.Store, taskID string) riskpolicy.ApprovalStatus {
	t.Helper()
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := d.EvaluateApprovals(ctx, task, "")
	if err != nil {
		t.Fatal(err)
	}
	return evaluation.Status
}

// A trusted author's approval is required while it is missing, and a
// non-author's waiver satisfies that condition without changing the number
// (AC-8.4, AC-8.5).
func TestTrustedAuthorApprovalMissingThenWaived(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "risk-trusted-author", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	claimImplementation(t, st, ctx, task.ID, "usr-author")
	_, err := d.MergeAtGate(ctx, task, gateOperator)
	var pending *RiskApprovalPending
	if !errors.As(err, &pending) || !pending.Recorded || !pending.Evaluation.Status.TrustedAuthorApprovalNeeded || pending.Evaluation.Status.Remaining != 1 {
		t.Fatalf("merge without the trusted author err=%v pending=%+v", err, pending)
	}
	if forge.merges != 0 {
		t.Fatalf("merges=%d", forge.merges)
	}
	// An author cannot waive the author approval.
	if _, _, err := d.WaiveAuthorApproval(ctx, task, person("usr-author"), gateHead, "away"); !errors.Is(err, store.ErrRefusal) {
		t.Fatalf("author waiver err=%v, want a refusal", err)
	}
	// A waiver at another head is refused with nothing recorded.
	if _, _, err := d.WaiveAuthorApproval(ctx, task, person("usr-lead"), "other-head", "away"); !errors.Is(err, store.ErrRefusal) {
		t.Fatalf("stale waiver err=%v, want a refusal", err)
	}
	evaluation, created, err := d.WaiveAuthorApproval(ctx, task, person("usr-lead"), gateHead, "the author is on leave")
	if err != nil || !created || evaluation.Status.Required != 1 || !evaluation.Status.Satisfied {
		t.Fatalf("waiver evaluation=%+v created=%t err=%v", evaluation.Status, created, err)
	}
	if _, created, err := d.WaiveAuthorApproval(ctx, task, person("usr-lead"), gateHead, "the author is on leave"); err != nil || created {
		t.Fatalf("replayed waiver created=%t err=%v", created, err)
	}
	if n := countKind(eventKinds(t, st, ctx, task.ID), core.TaskAuthorWaiverEvent); n != 1 {
		t.Fatalf("waiver events=%d, want 1", n)
	}
	out, err := d.MergeAtGate(ctx, task, gateOperator)
	if err != nil || out.State != core.TaskMerged || forge.merges != 1 {
		t.Fatalf("merge after the waiver out=%s err=%v merges=%d", out.State, err, forge.merges)
	}
}

// The trusted author's own approval satisfies the author condition and counts
// toward the number (AC-8.4).
func TestTrustedAuthorApprovalCounts(t *testing.T) {
	ctx, _, task, d, forge := riskGateFixture(t, "risk-author-approves", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	claimImplementation(t, d.Store, ctx, task.ID, "usr-author")
	out, err := d.MergeAtGate(ctx, task, person("usr-author"))
	if err != nil || out.State != core.TaskMerged || forge.merges != 1 {
		t.Fatalf("author merge out=%s err=%v merges=%d", out.State, err, forge.merges)
	}
}

// An untrusted author's approval does not count, the task needs a non-author,
// and it never merges automatically at any tier (AC-9.2). A trust-list edit
// applies at the next evaluation (AC-9.3).
func TestUntrustedAuthorApprovalDoesNotCount(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "risk-untrusted", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	claimImplementation(t, st, ctx, task.ID, "usr-author")
	setTrustedAuthors(d, "usr-other")
	before := len(eventKinds(t, st, ctx, task.ID))
	_, err := d.MergeAtGate(ctx, task, person("usr-author"))
	var pending *RiskApprovalPending
	if !errors.As(err, &pending) || pending.Recorded || pending.Reason != riskpolicy.ApprovalReasonUntrustedAuthor || !pending.Evaluation.Status.NonAuthorApprovalNeeded {
		t.Fatalf("untrusted author merge err=%v pending=%+v", err, pending)
	}
	if after := len(eventKinds(t, st, ctx, task.ID)); after != before || forge.merges != 0 {
		t.Fatalf("untrusted author's merge appended %d events and %d merges", after-before, forge.merges)
	}
	// Trusting the author applies the next time the task is evaluated.
	setTrustedAuthors(d, "usr-author")
	if status := approvalCount(t, d, ctx, st, task.ID); status.UntrustedAuthor || !status.TrustedAuthorApprovalNeeded {
		t.Fatalf("after the trust edit status=%+v", status)
	}
	setTrustedAuthors(d, "usr-other")
	out, err := d.MergeAtGate(ctx, task, gateOperator)
	if err != nil || out.State != core.TaskMerged || forge.merges != 1 {
		t.Fatalf("non-author merge out=%s err=%v merges=%d", out.State, err, forge.merges)
	}
}

// An untrusted author holds the automatic merge even at a tier that needs no
// approval (AC-9.2), while a trusted author's low-tier task merges.
func TestUntrustedAuthorHoldsTheAutomaticMerge(t *testing.T) {
	ctx, st, task, d, forge := riskApprovedTask(t, "risk-untrusted-auto", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	claimImplementation(t, st, ctx, task.ID, "no-gates-author")
	err := d.MergeApprovedTaskAutomatically(ctx, task)
	var refusal *store.RiskApprovalsRequiredError
	if !errors.As(err, &refusal) || refusal.Required != 1 || forge.merges != 0 {
		t.Fatalf("automatic merge err=%v merges=%d, want a hold", err, forge.merges)
	}
	ctx, st, task, d, forge = riskApprovedTask(t, "risk-trusted-auto", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierLow, 0})
	claimImplementation(t, st, ctx, task.ID, "usr-author")
	if err := d.MergeApprovedTaskAutomatically(ctx, task); err != nil || forge.merges != 1 {
		t.Fatalf("trusted low-tier automatic merge err=%v merges=%d", err, forge.merges)
	}
}

// A head that cannot land records no approval: an unready readiness peek
// records nothing, and so does a stale requested head (AC-8.7; AC-1.3).
func TestUnreadyHeadRecordsNoApproval(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "risk-unready", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierHigh, 0})
	forge.pr.Mergeable = "CONFLICTING"
	var notReady *ErrGateMergeNotReady
	if _, err := d.MergeAtGate(ctx, task, gateOperator); !errors.As(err, &notReady) {
		t.Fatalf("conflicting merge err=%v", err)
	}
	if _, err := d.RecordApproval(ctx, task, gateOperator, gateHead); !errors.As(err, &notReady) {
		t.Fatalf("conflicting approval err=%v", err)
	}
	forge.pr.Mergeable, forge.pr.HeadSHA = "MERGEABLE", "moved-head"
	if _, err := d.RecordApproval(ctx, task, gateOperator, gateHead); !errors.As(err, &notReady) || notReady.State != "STALE" {
		t.Fatalf("moved head approval err=%v", err)
	}
	forge.pr.HeadSHA = gateHead
	if _, err := d.RecordApproval(ctx, task, gateOperator, "other-head"); !errors.Is(err, store.ErrRefusal) {
		t.Fatalf("stale requested head err=%v", err)
	}
	if approvals, _ := st.ListTaskApprovals(ctx, task.ID); len(approvals) != 0 {
		t.Fatalf("approvals=%+v, want none", approvals)
	}
	if n := countKind(eventKinds(t, st, ctx, task.ID), core.TaskApprovalEvent); n != 0 || forge.merges != 0 {
		t.Fatalf("approval events=%d merges=%d", n, forge.merges)
	}
}

// A standalone approval never merges, even when it meets the count, and a
// person without operate_gates does not count (AC-8.3).
func TestStandaloneApprovalNeverMerges(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "risk-standalone", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	_, err := d.RecordApproval(ctx, task, person("no-gates-viewer"), gateHead)
	var pending *RiskApprovalPending
	if !errors.As(err, &pending) || pending.Recorded || pending.Reason != riskpolicy.ApprovalReasonNoCapability {
		t.Fatalf("approval without operate_gates err=%v pending=%+v", err, pending)
	}
	evaluation, err := d.RecordApproval(ctx, task, gateOperator, gateHead)
	if err != nil || !evaluation.Status.Satisfied || evaluation.Status.Recorded != 1 {
		t.Fatalf("approval evaluation=%+v err=%v", evaluation.Status, err)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskAwaiting || forge.merges != 0 {
		t.Fatalf("standalone approval state=%s merges=%d", current.State, forge.merges)
	}
	// The recorded approval is the count: a later merge by the same person
	// lands the head.
	if out, err := d.MergeAtGate(ctx, task, gateOperator); err != nil || out.State != core.TaskMerged {
		t.Fatalf("merge after approval out=%s err=%v", out.State, err)
	}
}

// Approvals bind to the head and review round: a new round, which every new
// head reaches the merge gate through, finds none, while a tier that rises
// at the same head keeps them and requires the additional number (AC-8.6,
// AC-8.8).
func TestNewHeadClearsApprovalsAndTierRiseKeepsThem(t *testing.T) {
	ctx, st, task, d, _ := riskGateFixture(t, "risk-new-head", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierMedium, 0})
	if _, err := d.RecordApproval(ctx, task, gateOperator, gateHead); err != nil {
		t.Fatal(err)
	}
	raiseTier(t, ctx, st, task.ID, riskpolicy.TierHigh)
	if status := approvalCount(t, d, ctx, st, task.ID); status.Recorded != 1 || status.Required != 2 || status.Remaining != 1 {
		t.Fatalf("after the raise status=%+v, want 1 of 2", status)
	}
	createReviewRound(t, st, ctx, task.ID, 2)
	if status := approvalCount(t, d, ctx, st, task.ID); status.Recorded != 0 || status.Remaining != 2 {
		t.Fatalf("after a new round status=%+v, want 0 of 2", status)
	}
}

// A definitive forge refusal of the last approval's merge credits no new
// approval; earlier approvals stay (AC-8.7; DEC-64(6)).
func TestRefusedFinalMergeRecordsNoApproval(t *testing.T) {
	ctx, st, task, d, forge := riskGateFixture(t, "risk-refused", riskMergeCase{riskpolicy.ModeEnforce, riskpolicy.TierHigh, 0})
	if _, err := d.MergeAtGate(ctx, task, person("usr-first")); err == nil {
		t.Fatal("first merge landed")
	}
	forge.answer = func(*githubtrigger.PullRequest) error {
		return githubtrigger.StatusError(http.StatusMethodNotAllowed, errors.New("branch protection"))
	}
	if _, err := d.MergeAtGate(ctx, task, gateOperator); !errors.Is(err, ErrGateMergeRefused) {
		t.Fatalf("refused merge err=%v", err)
	}
	approvals, err := st.ListTaskApprovals(ctx, task.ID)
	if err != nil || len(approvals) != 1 || approvals[0].UserID != "usr-first" {
		t.Fatalf("approvals=%+v err=%v, want only the first person's", approvals, err)
	}
	assertGatePendingState(t, st, ctx, task.ID)
}

// Off and record tasks refuse the approval routes and keep DEC-64's merge.
func TestApprovalRoutesRefuseNonEnforcingTasks(t *testing.T) {
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeRecord, riskpolicy.ModeOff} {
		ctx, st, task, d, _ := riskGateFixture(t, "risk-routes-"+string(mode), riskMergeCase{mode, riskpolicy.TierHigh, 2})
		if _, err := d.RecordApproval(ctx, task, gateOperator, gateHead); !errors.Is(err, store.ErrRefusal) {
			t.Fatalf("%s approval err=%v", mode, err)
		}
		if _, _, err := d.WaiveAuthorApproval(ctx, task, gateOperator, gateHead, "reason"); !errors.Is(err, store.ErrRefusal) {
			t.Fatalf("%s waiver err=%v", mode, err)
		}
		if approvals, _ := st.ListTaskApprovals(ctx, task.ID); len(approvals) != 0 {
			t.Fatalf("%s approvals=%+v", mode, approvals)
		}
	}
}
