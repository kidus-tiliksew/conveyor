package dispatch

// Counted human approvals at the merge gate of an enforcing task
// (req-risk-based-review REQ-8, REQ-9, AC-8.3 to AC-8.8, AC-9.1 to AC-9.3;
// req-review-gates-evidence AC-1.3; DEC-124; feature-risk-based-review RR-7;
// component-submission-merge). Off and record tasks never reach this file:
// DEC-64 and DEC-66 govern them unchanged.

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	github "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// RiskApprovalPending answers a merge or approve act that left an enforcing
// task short of its required approvals: Recorded is true when the act
// recorded the person's approval now. Nothing was merged and the gate stays
// pending (req-risk-based-review AC-8.7). It matches store.ErrRefusal, so a
// route that does not render it answers 409 with its text.
type RiskApprovalPending struct {
	Recorded   bool
	Evaluation store.ApprovalEvaluation
	// Reason names why the person's approval did not count, when it did not.
	Reason string
}

func (e *RiskApprovalPending) Error() string {
	status := e.Evaluation.Status
	var b strings.Builder
	switch {
	case e.Recorded:
		b.WriteString("Your approval is recorded. ")
	case e.Reason != "":
		fmt.Fprintf(&b, "Your approval does not count: %s. ", e.Reason)
	}
	b.WriteString(ApprovalShortfallText(status))
	b.WriteString(" Nothing was merged.")
	return b.String()
}

// Is matches store.ErrRefusal.
func (e *RiskApprovalPending) Is(target error) bool { return target == store.ErrRefusal }

// ApprovalShortfallText states what an enforcing task still needs in one or
// two sentences: the approvals that remain and each author condition
// (req-risk-based-review AC-11.1).
func ApprovalShortfallText(status riskpolicy.ApprovalStatus) string {
	approvals := "approvals"
	if status.Remaining == 1 {
		approvals = "approval"
	}
	text := fmt.Sprintf("The task has %d of %d required human approvals; %d more %s needed.", status.Recorded, status.Required, status.Remaining, pluralVerb(approvals, status.Remaining))
	switch {
	case status.TrustedAuthorApprovalNeeded && status.NonAuthorApprovalNeeded:
		text += " A trusted author's approval and an approval from a person who is not an author are among them."
	case status.TrustedAuthorApprovalNeeded:
		text += " A trusted author's approval is among them."
	case status.NonAuthorApprovalNeeded:
		text += " An approval from a person who is not an author is among them."
	}
	return text
}

func pluralVerb(noun string, n int) string {
	if n == 1 {
		return noun + " is"
	}
	return noun + " are"
}

// trustedAuthorsFor reads the task repository's live trusted_authors list
// (req-risk-based-review AC-9.3).
func (d *Dispatcher) trustedAuthorsFor(ctx context.Context, task core.Task) ([]string, error) {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return nil, err
	}
	repo, _ := cfg.Repo(task.Repo)
	return repo.TrustedAuthors, nil
}

// EvaluateApprovals evaluates an enforcing task's approvals with the live
// trust list. prospective is a person whose approval is being considered, or
// empty.
func (d *Dispatcher) EvaluateApprovals(ctx context.Context, task core.Task, prospective string) (store.ApprovalEvaluation, error) {
	if !task.SetupContract.Risk.Enforcing() {
		return store.ApprovalEvaluation{}, nil
	}
	trusted, err := d.trustedAuthorsFor(ctx, task)
	if err != nil {
		return store.ApprovalEvaluation{}, err
	}
	return store.EvaluateTaskApprovals(ctx, d.Store, task, trusted, prospective)
}

// actorUserID is the person an act runs as: the user actor, or the owner of
// the request credential (DEC-116(2)).
func actorUserID(ctx context.Context, actor core.Actor) string {
	if id, ok := strings.CutPrefix(actor.ID, "user:"); ok {
		return strings.TrimSpace(id)
	}
	if credential, ok := store.CredentialFromContext(ctx); ok {
		return strings.TrimSpace(credential.OwnerUserID)
	}
	return ""
}

// riskOperatorMergeLocked decides a person's merge act on an enforcing task
// under the task side-effect lock, after the act's readiness read. It returns
// nil when the person's approval completes every requirement, so the caller
// lands the head; the final approval is then recorded only with the
// confirmed merge (req-review-gates-evidence AC-1.3). Short of that, it
// records the person's approval when it counts and the head is ready, lands
// nothing, and returns *RiskApprovalPending with what is still needed
// (req-risk-based-review AC-8.7). An unready head records nothing.
func (d *Dispatcher) riskOperatorMergeLocked(ctx context.Context, current core.Task, actor core.Actor, pr github.PullRequest) error {
	if !current.SetupContract.Risk.Enforcing() {
		return nil
	}
	userID := actorUserID(ctx, actor)
	evaluation, err := d.EvaluateApprovals(ctx, current, userID)
	if err != nil {
		return err
	}
	if evaluation.Status.Satisfied {
		return nil
	}
	_, err = d.recordApprovalLocked(ctx, current, userID, pr, "merge", evaluation)
	return err
}

// recordApprovalLocked records userID's approval of the reviewed head after
// the side-effect-free readiness peek, and answers *RiskApprovalPending when
// the requirements are still not met. It returns the evaluation after the
// record.
func (d *Dispatcher) recordApprovalLocked(ctx context.Context, current core.Task, userID string, pr github.PullRequest, source string, evaluation store.ApprovalEvaluation) (store.ApprovalEvaluation, error) {
	status := evaluation.Status
	if !status.ProspectiveEligible {
		if status.ProspectiveReason == riskpolicy.ApprovalReasonAlreadyCounted {
			if status.Satisfied {
				return evaluation, nil
			}
			return evaluation, &RiskApprovalPending{Evaluation: evaluation}
		}
		return evaluation, &RiskApprovalPending{Evaluation: evaluation, Reason: status.ProspectiveReason}
	}
	state, err := d.peekGateMergeReadinessLocked(ctx, current, pr)
	if err != nil {
		return evaluation, err
	}
	if state != "MERGEABLE" {
		return evaluation, &ErrGateMergeNotReady{State: state}
	}
	if evaluation.ReviewRound < 1 || evaluation.Head == "" || evaluation.Head != pr.HeadSHA {
		return evaluation, &ErrGateMergeNotReady{State: "STALE"}
	}
	personCtx := store.WithActor(ctx, core.Actor{ID: store.UserActorID(userID), Role: core.ActorUser})
	_, created, err := d.Store.RecordTaskApproval(personCtx, core.TaskApproval{
		TaskID: current.ID, Kind: core.TaskApprovalPerson, HeadSHA: evaluation.Head, ReviewRound: evaluation.ReviewRound,
		UserID: userID, Source: source,
	})
	if err != nil {
		return evaluation, err
	}
	after, err := d.EvaluateApprovals(ctx, current, "")
	if err != nil {
		return evaluation, err
	}
	if after.Status.Satisfied {
		return after, nil
	}
	return after, &RiskApprovalPending{Recorded: created, Evaluation: after}
}

// RecordApproval is the approve act on an enforcing task at the merge gate:
// POST /v1/tasks/{id}/approvals and the merge gate's Approve. Under the task
// side-effect lock it checks that head is the reviewed head, peeks the pull
// request's readiness without side effects, and records the person's
// approval only at a ready head. It never merges. It returns the evaluation
// after the act; *RiskApprovalPending reports a recorded approval that leaves
// the task short, and a nil error means every requirement is met
// (req-risk-based-review AC-8.3, AC-8.6; feature-risk-based-review RR-7).
func (d *Dispatcher) RecordApproval(ctx context.Context, task core.Task, actor core.Actor, head string) (store.ApprovalEvaluation, error) {
	var out store.ApprovalEvaluation
	err := d.Store.WithTaskSideEffectLock(ctx, task.ID, func(lockedCtx context.Context) error {
		current, err := d.Store.GetTask(lockedCtx, task.ID)
		if err != nil {
			return err
		}
		if err := approvalGateOpen(current); err != nil {
			return err
		}
		if strings.TrimSpace(head) == "" || head != strings.TrimSpace(current.ReviewedHeadSHA) {
			return store.Refuse(&store.ApprovalHeadError{Head: head, Current: current.ReviewedHeadSHA})
		}
		userID := actorUserID(lockedCtx, actor)
		evaluation, err := d.EvaluateApprovals(lockedCtx, current, userID)
		if err != nil {
			return err
		}
		pr, err := d.gatePullRequest(lockedCtx, current)
		if err != nil {
			return err
		}
		out, err = d.recordApprovalLocked(lockedCtx, current, userID, pr, "approval", evaluation)
		return err
	})
	return out, err
}

// WaiveAuthorApproval records a non-author's waiver of the trusted author's
// approval for the reviewed head (req-risk-based-review AC-8.5). The waiver
// changes no number and does not satisfy a non-author approval requirement.
// It is refused for an author, for a task with no trusted author, and for a
// head that is not the reviewed head.
func (d *Dispatcher) WaiveAuthorApproval(ctx context.Context, task core.Task, actor core.Actor, head, reason string) (store.ApprovalEvaluation, bool, error) {
	var out store.ApprovalEvaluation
	created := false
	err := d.Store.WithTaskSideEffectLock(ctx, task.ID, func(lockedCtx context.Context) error {
		current, err := d.Store.GetTask(lockedCtx, task.ID)
		if err != nil {
			return err
		}
		if err := approvalGateOpen(current); err != nil {
			return err
		}
		if strings.TrimSpace(head) == "" || head != strings.TrimSpace(current.ReviewedHeadSHA) {
			return store.Refuse(&store.ApprovalHeadError{Head: head, Current: current.ReviewedHeadSHA})
		}
		userID := actorUserID(lockedCtx, actor)
		evaluation, err := d.EvaluateApprovals(lockedCtx, current, "")
		if err != nil {
			return err
		}
		if userID == "" || evaluation.IsAuthor(userID) {
			return store.Refusef("an author cannot waive the author approval; a person who is not an author records the waiver")
		}
		trusted, err := d.trustedAuthorsFor(lockedCtx, current)
		if err != nil {
			return err
		}
		probe := riskpolicy.ApprovalInput{TrustedAuthors: trusted, Gatekeepers: map[string]bool{}}
		hasTrusted := false
		for _, author := range evaluation.Authors {
			if len(trusted) > 0 {
				hasTrusted = hasTrusted || probe.Trusted(author)
				continue
			}
			holds, err := store.AuthorHoldsOperateGates(lockedCtx, d.Store, current.Workspace, author)
			if err != nil {
				return err
			}
			hasTrusted = hasTrusted || holds
		}
		if !hasTrusted {
			return store.Refusef("the task has no trusted author whose approval could be waived")
		}
		if evaluation.ReviewRound < 1 {
			return store.Refusef("the task has no review round to bind the waiver to")
		}
		personCtx := store.WithActor(lockedCtx, core.Actor{ID: store.UserActorID(userID), Role: core.ActorUser})
		_, created, err = d.Store.RecordTaskApproval(personCtx, core.TaskApproval{
			TaskID: current.ID, Kind: core.TaskApprovalAuthorWaiver, HeadSHA: head, ReviewRound: evaluation.ReviewRound,
			UserID: userID, Reason: reason,
		})
		if err != nil {
			return err
		}
		out, err = d.EvaluateApprovals(lockedCtx, current, "")
		return err
	})
	return out, created, err
}

// approvalGateOpen refuses an approval act on a task that does not enforce or
// is not at the merge gate or approved.
func approvalGateOpen(task core.Task) error {
	if !task.SetupContract.Risk.Enforcing() {
		return store.Refusef("the task's frozen risk policy does not count approvals; use the merge gate")
	}
	atGate := task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval
	if !atGate && task.State != core.TaskApproved {
		return store.Refusef("the task is not waiting at the merge gate")
	}
	return nil
}

// gatePullRequest reads the task's pull request for the readiness peek. A
// read failure is an unready head: nothing is recorded.
func (d *Dispatcher) gatePullRequest(ctx context.Context, task core.Task) (github.PullRequest, error) {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return github.PullRequest{}, err
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok || strings.TrimSpace(repo.GitHub) == "" {
		return github.PullRequest{}, &ErrGateMergeNotReady{State: "UNKNOWN"}
	}
	pr, err := d.readPullRequestMergeability(ctx, repo.GitHub, task.Branch)
	if err != nil {
		log.Printf("approval readiness for %s: %v", task.ID, err)
		return github.PullRequest{}, &ErrGateMergeNotReady{State: "UNKNOWN"}
	}
	if pr.Merged {
		return github.PullRequest{}, &ErrGateMergeNotReady{State: "MERGED"}
	}
	return pr, nil
}
