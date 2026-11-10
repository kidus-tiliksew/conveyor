package dispatch

// The conveyor/review-policy commit status of an enforcing task
// (req-risk-based-review AC-10.1, AC-10.2; component-git-delivery invariant
// 37; component-submission-merge invariant 65; feature-risk-based-review
// RR-9). A task whose frozen risk mode is off or record publishes nothing and
// reads nothing here.
//
// The status of a head is pending while the head lacks any agent review or
// human approval its requirements call for, and success once it has them.
// Every evaluation reads the ratings, review rounds, approvals, waivers,
// authors, and live trust list current at that call; no cached status, task
// state, or MergeApproval flag stands in for them. A head other than the
// reviewed head borrows no approval or review of another head.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// PolicyEvaluation is what an enforcing task's head has against its risk
// requirements.
type PolicyEvaluation struct {
	Enforced bool
	Head     string
	Tier     riskpolicy.Tier
	// Approvals is the human approval evaluation at Head.
	Approvals store.ApprovalEvaluation
	// RequiredAgentReviews and RecordedAgentReviews count the approving
	// agent reviews of the latest settled review round at Head.
	RequiredAgentReviews int
	RecordedAgentReviews int
	// Met is true when Head has every agent review and human approval it
	// requires.
	Met bool
}

// Desired is the policy status Head calls for.
func (e PolicyEvaluation) Desired() string {
	if e.Met {
		return core.PolicyStatusSuccess
	}
	return core.PolicyStatusPending
}

// MissingAgentReviews is the number of approving agent reviews Head lacks.
func (e PolicyEvaluation) MissingAgentReviews() int {
	return max(e.RequiredAgentReviews-e.RecordedAgentReviews, 0)
}

// EvaluatePolicyAtHead evaluates an enforcing task's requirements at head.
// prospective is a person whose approval is being considered, or empty: a
// person's merge at the gate counts that person's provisional final approval
// before it is recorded (component-submission-merge invariant 65).
func (d *Dispatcher) EvaluatePolicyAtHead(ctx context.Context, task core.Task, head, prospective string) (PolicyEvaluation, error) {
	head = strings.TrimSpace(head)
	if !task.SetupContract.Risk.Enforcing() {
		return PolicyEvaluation{}, nil
	}
	trusted, err := d.trustedAuthorsFor(ctx, task)
	if err != nil {
		return PolicyEvaluation{}, err
	}
	// The approval evaluation binds to the probe's reviewed head, so a head
	// other than the reviewed head counts only approvals recorded for it.
	probe := task
	probe.ReviewedHeadSHA = head
	approvals, err := store.EvaluateTaskApprovals(ctx, d.Store, probe, trusted, prospective)
	if err != nil {
		return PolicyEvaluation{}, err
	}
	events, err := d.Store.ListCompactEvents(ctx, task.ID)
	if err != nil {
		return PolicyEvaluation{}, err
	}
	recorded := approvingAgentReviewsAt(events, head)
	evaluation := PolicyEvaluation{
		Enforced: true, Head: head, Tier: approvals.Tier, Approvals: approvals,
		RequiredAgentReviews: max(approvals.Requirements.AgentReviews, 1), RecordedAgentReviews: recorded,
	}
	evaluation.Met = head != "" && approvals.Status.Satisfied && recorded >= evaluation.RequiredAgentReviews
	return evaluation, nil
}

// observedMergeBypass evaluates an enforcing task's requirements at a head
// GitHub merged and returns the bypass to record, or nil when the head met
// them or the task does not enforce (req-risk-based-review AC-10.3;
// component-submission-merge invariant 66). A Conveyor merge request still
// pending at that head counts its person's provisional final approval, as
// the request itself did; the GitHub account that merged never counts as an
// approval.
func (d *Dispatcher) observedMergeBypass(ctx context.Context, task core.Task, githubRepo string, pr github.PullRequest, events []core.Event) (*core.MergeBypass, error) {
	if !task.SetupContract.Risk.Enforcing() {
		return nil, nil
	}
	prospective := ""
	switch task.State {
	case core.TaskAwaiting:
		if request, pending := pendingGateMergeRequest(events); pending && request.HeadSHA == pr.HeadSHA {
			prospective = request.OperatorUserID
		}
	case core.TaskApproved:
		prospective = pendingOperatorMergeRequest(events, pr.HeadSHA)
	}
	evaluation, err := d.EvaluatePolicyAtHead(ctx, task, pr.HeadSHA, prospective)
	if err != nil {
		return nil, err
	}
	if evaluation.Met {
		return nil, nil
	}
	status := evaluation.Approvals.Status
	actor := strings.TrimSpace(pr.MergedBy)
	if actor == "" {
		actor = core.UnknownForgeActor
	}
	return &core.MergeBypass{
		Repository: githubRepo, PullRequest: pr.Number, URL: pr.URL,
		ForgeActor: actor, HeadSHA: pr.HeadSHA, MergeCommitSHA: pr.MergeCommitSHA, Tier: string(evaluation.Tier),
		RequiredHumanApprovals: status.Required, RecordedHumanApprovals: status.Recorded, MissingHumanApprovals: status.Remaining,
		TrustedAuthorApprovalNeeded: status.TrustedAuthorApprovalNeeded, NonAuthorApprovalNeeded: status.NonAuthorApprovalNeeded,
		AuthorWaiverNeeded:   status.AuthorWaiverNeeded,
		RequiredAgentReviews: evaluation.RequiredAgentReviews, RecordedAgentReviews: evaluation.RecordedAgentReviews,
		MissingAgentReviews: evaluation.MissingAgentReviews(),
	}, nil
}

// approvingAgentReviewsAt counts the approving seats of the latest settled
// review round when that round approved head, and 0 otherwise: a round that
// requested changes, or approved another head, leaves head without review.
func approvingAgentReviewsAt(events []core.Event, head string) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_completed" {
			continue
		}
		var round struct {
			Verdict         string `json:"verdict"`
			ApprovedHeadSHA string `json:"approved_head_sha"`
			Reviews         []struct {
				Verdict           string `json:"verdict"`
				ReviewedCommitSHA string `json:"reviewed_commit_sha"`
			} `json:"reviews"`
		}
		if json.Unmarshal(events[i].Payload, &round) != nil || round.Verdict != "approve" || head == "" || round.ApprovedHeadSHA != head {
			return 0
		}
		count := 0
		for _, review := range round.Reviews {
			if review.Verdict == "approve" && (review.ReviewedCommitSHA == "" || review.ReviewedCommitSHA == head) {
				count++
			}
		}
		return count
	}
	return 0
}

// ratedHead is the head of the task's latest path rating: the head its
// latest submission or refresh rated before review (req-risk-based-review
// AC-5.1). It is empty before any head is rated.
func ratedHead(ratings []core.RiskRating) string {
	sorted := append([]core.RiskRating(nil), ratings...)
	store.SortRiskRatings(sorted)
	head := ""
	for _, rating := range sorted {
		if rating.Source == core.RiskSourcePathRule && strings.TrimSpace(rating.HeadSHA) != "" {
			head = strings.TrimSpace(rating.HeadSHA)
		}
	}
	return head
}

// SyncReviewPolicyStatus re-evaluates an enforcing task's rated head and
// every head it already published and queues each status whose state
// changed. Callers that already hold the task side-effect lock call
// syncReviewPolicyStatusLocked instead.
func (d *Dispatcher) SyncReviewPolicyStatus(ctx context.Context, taskID string) error {
	_, err := d.syncReviewPolicyStatus(ctx, taskID)
	return err
}

// syncReviewPolicyStatus is SyncReviewPolicyStatus reporting the rows it
// queued.
func (d *Dispatcher) syncReviewPolicyStatus(ctx context.Context, taskID string) (int, error) {
	queued := 0
	err := d.Store.WithTaskSideEffectLock(ctx, taskID, func(lockedCtx context.Context) error {
		task, err := d.Store.GetTask(lockedCtx, taskID)
		if err != nil {
			return err
		}
		queued, err = d.syncReviewPolicyStatusLocked(lockedCtx, task)
		return err
	})
	return queued, err
}

// syncReviewPolicyStatusLocked queues the status of the rated head and
// re-queues every earlier head whose state changed, so an older head that
// once published success returns to pending (component-git-delivery
// invariant 37). A task that does not enforce reads nothing.
func (d *Dispatcher) syncReviewPolicyStatusLocked(ctx context.Context, task core.Task) (int, error) {
	if !task.SetupContract.Risk.Enforcing() {
		return 0, nil
	}
	if task.Workspace != "" {
		ctx = store.WithWorkspace(ctx, task.Workspace)
	}
	ratings, err := d.Store.ListTaskRiskRatings(ctx, task.ID)
	if err != nil {
		return 0, err
	}
	rows, err := d.Store.ListReviewPolicyStatuses(ctx, task.ID)
	if err != nil {
		return 0, err
	}
	heads := []string{}
	if head := ratedHead(ratings); head != "" {
		heads = append(heads, head)
	}
	for _, row := range rows {
		if row.HeadSHA != "" && !containsString(heads, row.HeadSHA) {
			heads = append(heads, row.HeadSHA)
		}
	}
	queued := 0
	for _, head := range heads {
		evaluation, err := d.EvaluatePolicyAtHead(ctx, task, head, "")
		if err != nil {
			return queued, err
		}
		_, wrote, err := d.Store.QueueReviewPolicyStatus(ctx, task.ID, head, evaluation.Desired())
		if err != nil {
			return queued, err
		}
		if wrote {
			queued++
		}
	}
	return queued, nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// syncReviewPolicyStatusAfter re-evaluates the task after an act that can
// change its requirements or what its head has: a rating, a settled review
// round, an approval or waiver, a head change, or a merge. The act already
// committed, so a failure here is logged and repaired by the next act or by
// startup reconciliation; it never fails the act.
func (d *Dispatcher) syncReviewPolicyStatusAfter(ctx context.Context, task core.Task, locked bool) {
	if !task.SetupContract.Risk.Enforcing() {
		return
	}
	// The factory publishes the status, whoever's act changed the head's
	// requirements.
	ctx = store.WithActor(ctx, store.SystemActor("review-policy-status"))
	var err error
	if locked {
		var current core.Task
		if current, err = d.Store.GetTask(ctx, task.ID); err == nil {
			_, err = d.syncReviewPolicyStatusLocked(ctx, current)
		}
	} else {
		err = d.SyncReviewPolicyStatus(ctx, task.ID)
	}
	if err != nil {
		log.Printf("policy status of task %s: %v", task.ID, err)
	}
}

// SyncReviewPolicyStatusAfterAct is the exported hook for acts outside this
// package, such as a submission, a verdict, or a tier raise.
func (d *Dispatcher) SyncReviewPolicyStatusAfterAct(ctx context.Context, taskID string) {
	if d == nil {
		return
	}
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		log.Printf("policy status of task %s: %v", taskID, err)
		return
	}
	d.syncReviewPolicyStatusAfter(ctx, task, false)
}

// ReconcileReviewPolicyStatuses is the start-up drift pass of the
// workspace's policy statuses: every live enforcing task, and every task with
// an open row, is re-evaluated, which inserts a missing intent, reopens a
// failed row for its retry, and returns an obsolete success to pending. Each
// task it queued a status for is reported as one repair. It reports the
// tasks repaired.
func (d *Dispatcher) ReconcileReviewPolicyStatuses(ctx context.Context) (int, error) {
	ctx = store.WithActor(ctx, store.SystemActor("review-policy-status"))
	tasks, err := d.Store.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	open, err := d.Store.ListOpenReviewPolicyStatuses(ctx)
	if err != nil {
		return 0, err
	}
	withOpenRow := map[string]bool{}
	for _, row := range open {
		withOpenRow[row.TaskID] = true
	}
	repaired := 0
	var errs []error
	for _, task := range tasks {
		if !task.SetupContract.Risk.Enforcing() || (core.TaskTerminal(task.State) && !withOpenRow[task.ID]) {
			continue
		}
		queued, err := d.syncReviewPolicyStatus(ctx, task.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", task.ID, err))
			continue
		}
		if queued > 0 {
			repaired++
			store.ReportDriftRepair(ctx, store.DriftReviewPolicyStatusStale.For(task.ID))
		}
	}
	return repaired, errors.Join(errs...)
}

// ErrPolicyStatusUnpublished refuses a merge request of an enforcing task
// whose conveyor/review-policy success was not accepted by GitHub
// (component-submission-merge invariant 65).
var ErrPolicyStatusUnpublished = errors.New("policy_status_unpublished")

type policyStatusUnpublishedError struct{ cause error }

func (e *policyStatusUnpublishedError) Error() string {
	return "Conveyor could not publish the conveyor/review-policy success status, so it sent no merge request; it tries again later: " + e.cause.Error()
}
func (e *policyStatusUnpublishedError) Unwrap() error { return e.cause }
func (e *policyStatusUnpublishedError) Is(target error) bool {
	return target == ErrPolicyStatusUnpublished || target == store.ErrRefusal
}

// publishPolicySuccessBeforeMergeLocked publishes conveyor/review-policy
// success on head before an enforcing task's merge request, on the gate and
// automatic paths alike, and returns nil only after GitHub accepts it
// (req-risk-based-review AC-10.2; component-submission-merge invariant 65).
// prospective is the acting person whose provisional final approval the gate
// counts; it is not recorded here. A failure appends merge.failed with reason
// policy_status_unpublished and returns ErrPolicyStatusUnpublished: the
// caller sends no merge request and records no new approval. The caller holds
// the task side-effect lock. A task that does not enforce returns nil.
func (d *Dispatcher) publishPolicySuccessBeforeMergeLocked(ctx context.Context, task core.Task, githubRepo, head, prospective string, author core.ForgeAuthoringIdentity) error {
	if !task.SetupContract.Risk.Enforcing() {
		return nil
	}
	fail := func(cause error) error {
		unpublished := &policyStatusUnpublishedError{cause: cause}
		return d.recordMergeFailureWithAuthor(ctx, task, "policy_status_unpublished", unpublished, author)
	}
	evaluation, err := d.EvaluatePolicyAtHead(ctx, task, head, prospective)
	if err != nil {
		return fail(err)
	}
	if !evaluation.Met {
		return fail(fmt.Errorf("head %s does not have every agent review and human approval its risk policy requires", head))
	}
	row, _, err := d.Store.QueueReviewPolicyStatus(ctx, task.ID, head, core.PolicyStatusSuccess)
	if err != nil {
		return fail(err)
	}
	if row.State == core.ReviewPolicyStatusPublished && row.Published == core.PolicyStatusSuccess {
		// The row records success, but another writer of the context could
		// have changed GitHub's latest status since. The read-then-post
		// writes nothing when GitHub still holds success.
		if _, publishErr := d.publishPolicyStatus(ctx, task, githubRepo, head, core.PolicyStatusSuccess); publishErr != nil {
			// No job owns this row, so it is left failed for the
			// re-evaluation after the act to reopen with one.
			row.State, row.Attempts = core.ReviewPolicyStatusFailed, row.Attempts+1
			row.ForgeErrorCategory, row.LastError = string(github.ErrorCategory(publishErr)), publishErr.Error()
			if _, updateErr := d.Store.UpdateReviewPolicyStatus(ctx, row); updateErr != nil {
				return fail(errors.Join(publishErr, fmt.Errorf("record policy status failure: %w", updateErr)))
			}
			return fail(publishErr)
		}
		return nil
	}
	row.State, row.Desired, row.Attempts = core.ReviewPolicyStatusRetrying, core.PolicyStatusSuccess, row.Attempts+1
	row.ForgeErrorCategory, row.LastError = "", ""
	if row, err = d.Store.UpdateReviewPolicyStatus(ctx, row); err != nil {
		return fail(err)
	}
	if _, publishErr := d.publishPolicyStatus(ctx, task, githubRepo, head, core.PolicyStatusSuccess); publishErr != nil {
		row.ForgeErrorCategory, row.LastError = string(github.ErrorCategory(publishErr)), publishErr.Error()
		if _, updateErr := d.Store.UpdateReviewPolicyStatus(ctx, row); updateErr != nil {
			return fail(errors.Join(publishErr, fmt.Errorf("record policy status failure: %w", updateErr)))
		}
		return fail(publishErr)
	}
	row.State, row.Published = core.ReviewPolicyStatusPublished, core.PolicyStatusSuccess
	if _, err := d.Store.UpdateReviewPolicyStatus(ctx, row); err != nil {
		return fail(err)
	}
	return nil
}

// publishPolicyStatus sends one status write through the injected publisher.
func (d *Dispatcher) publishPolicyStatus(ctx context.Context, task core.Task, githubRepo, head, state string) (github.PolicyStatusResult, error) {
	if d.PublishPolicyStatus == nil {
		return github.PolicyStatusResult{}, fmt.Errorf("the policy status publisher is not configured")
	}
	if strings.TrimSpace(githubRepo) == "" {
		return github.PolicyStatusResult{}, fmt.Errorf("repository %q does not configure GitHub", task.Repo)
	}
	return d.PublishPolicyStatus(ctx, github.PolicyStatusPublication{Repo: githubRepo, Branch: task.Branch, HeadSHA: head, State: state})
}

type reviewPolicyStatusWorker struct {
	dispatcher *Dispatcher
}

// Work publishes one head's policy status. Under the task side-effect lock it
// re-reads the head's requirements immediately before the forge call, so a
// delayed or retried job publishes the state current at that call and never
// restores an obsolete pending or success. A failure changes no rating,
// verdict, or approval; the queue retries it, and the last attempt leaves the
// row failed for the next re-evaluation or startup to reopen (DEC-117(1)).
func (w *reviewPolicyStatusWorker) Work(ctx context.Context, job queue.Job) error {
	args, err := queue.DecodeArgs[queue.ReviewPolicyStatusArgs](job)
	if err != nil {
		return err
	}
	d := w.dispatcher
	ctx = store.WithActor(ctx, store.SystemActor(fmt.Sprintf("queue:review-policy-status:%s", job.ID)))
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	return d.Store.WithTaskSideEffectLock(ctx, args.TaskID, func(lockedCtx context.Context) error {
		return w.publishLocked(lockedCtx, job, args)
	})
}

func (w *reviewPolicyStatusWorker) publishLocked(ctx context.Context, job queue.Job, args queue.ReviewPolicyStatusArgs) error {
	d := w.dispatcher
	row, found, err := d.Store.GetReviewPolicyStatus(ctx, args.TaskID, args.HeadSHA)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !found) {
		return nil
	}
	if err != nil {
		return err
	}
	task, err := d.Store.GetTask(ctx, args.TaskID)
	if err != nil {
		return w.failEarly(ctx, job, row, err)
	}
	evaluation, err := d.EvaluatePolicyAtHead(ctx, task, args.HeadSHA, "")
	if err != nil {
		return w.failEarly(ctx, job, row, err)
	}
	desired := evaluation.Desired()
	if row.State == core.ReviewPolicyStatusPublished && row.Published == desired && row.Desired == desired {
		return nil
	}
	row.State, row.Desired, row.Attempts = core.ReviewPolicyStatusRetrying, desired, row.Attempts+1
	row.ForgeErrorCategory, row.LastError = "", ""
	if row, err = d.Store.UpdateReviewPolicyStatus(ctx, row); err != nil {
		return err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return w.failEarly(ctx, job, row, err)
	}
	repo, _ := cfg.Repo(task.Repo)
	if _, publishErr := d.publishPolicyStatus(ctx, task, repo.GitHub, args.HeadSHA, desired); publishErr != nil {
		row.ForgeErrorCategory, row.LastError = string(github.ErrorCategory(publishErr)), publishErr.Error()
		if job.Attempt >= job.MaxAttempts {
			row.State = core.ReviewPolicyStatusFailed
		}
		if _, updateErr := d.Store.UpdateReviewPolicyStatus(ctx, row); updateErr != nil {
			return fmt.Errorf("publish policy status: %v; record retry: %w", publishErr, updateErr)
		}
		return publishErr
	}
	row.State, row.Published = core.ReviewPolicyStatusPublished, desired
	_, err = d.Store.UpdateReviewPolicyStatus(ctx, row)
	return err
}

// failEarly records an attempt that stopped before its forge call; the last
// queue attempt leaves the row failed.
func (w *reviewPolicyStatusWorker) failEarly(ctx context.Context, job queue.Job, row core.ReviewPolicyStatus, cause error) error {
	row.LastError = cause.Error()
	if row.State == core.ReviewPolicyStatusPublished {
		row.State = core.ReviewPolicyStatusRetrying
	}
	if job.Attempt >= job.MaxAttempts {
		row.State = core.ReviewPolicyStatusFailed
	}
	if _, updateErr := w.dispatcher.Store.UpdateReviewPolicyStatus(ctx, row); updateErr != nil {
		return fmt.Errorf("%v; record retry: %w", cause, updateErr)
	}
	return cause
}

// Discarded leaves an open row failed with the reason, so the next
// re-evaluation or startup reopens it. A published or failed row is left
// alone, so a repeat changes nothing.
func (w *reviewPolicyStatusWorker) Discarded(ctx context.Context, job queue.Job, reason string) error {
	args, err := queue.DecodeArgs[queue.ReviewPolicyStatusArgs](job)
	if err != nil {
		log.Printf("queue job %s: discard: decode job: %v", job.ID, err)
		return nil
	}
	ctx = store.WithActor(ctx, store.SystemActor(fmt.Sprintf("queue:review-policy-status:%s", job.ID)))
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	row, found, err := w.dispatcher.Store.GetReviewPolicyStatus(ctx, args.TaskID, args.HeadSHA)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !found) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.State == core.ReviewPolicyStatusPublished || row.State == core.ReviewPolicyStatusFailed {
		return nil
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	row.State, row.LastError = core.ReviewPolicyStatusFailed, reason
	_, err = w.dispatcher.Store.UpdateReviewPolicyStatus(ctx, row)
	return err
}
