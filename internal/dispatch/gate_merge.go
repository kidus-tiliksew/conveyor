package dispatch

// The one-act operator merge at the merge_approval gate (DEC-64(1)-(7),(9);
// component-submission-merge; component-task-lifecycle).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	github "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// ErrGateMergeNotReady refuses a one-act gate merge whose readiness peek is
// not MERGEABLE. Nothing is appended and the gate stays pending (DEC-64(2,3)).
type ErrGateMergeNotReady struct{ State string }

func (e *ErrGateMergeNotReady) Error() string {
	return "merge is not offered: readiness " + e.State
}

// ErrGateMergeRefused marks a definitive forge refusal of a one-act gate
// merge, recorded as merge.failed{forge_merge_failed} (DEC-64(6)).
var ErrGateMergeRefused = errors.New("the forge refused the merge")

// ErrGateMergeUnconfirmed marks a one-act gate merge the forge did not
// confirm at the reviewed head, recorded as merge.failed{merge_unconfirmed}
// or {merge_verification_failed}; the gate stays pending (DEC-64(6)).
var ErrGateMergeUnconfirmed = errors.New("merge was not confirmed by the forge")

// gateMergeRequest is the operator act a pending merge.requested records at
// the current merge_approval gate episode (DEC-64(7)).
type gateMergeRequest struct {
	OperatorUserID string
	DisplayName    string
	HeadSHA        string
}

// pendingGateMergeRequest returns the latest merge.requested at the current
// merge_approval gate episode: after the latest task.state_changed with
// command gate.merge. A later merge.failed{forge_merge_failed} answers it;
// unconfirmed and verification failures leave it pending, so a later forge
// observation at its head still credits the requester (DEC-64(6,7); R4).
func pendingGateMergeRequest(events []core.Event) (gateMergeRequest, bool) {
	start := -1
	for i := len(events) - 1; i >= 0 && start < 0; i-- {
		if events[i].Kind != "task.state_changed" {
			continue
		}
		var changed struct {
			Command core.TaskCommand `json:"command"`
		}
		if json.Unmarshal(events[i].Payload, &changed) == nil && changed.Command == core.TaskGateMerge {
			start = i
		}
	}
	if start < 0 {
		return gateMergeRequest{}, false
	}
	var request gateMergeRequest
	pending := false
	for _, event := range events[start+1:] {
		switch event.Kind {
		case "merge.requested":
			var payload struct {
				GateKind    core.GateKind `json:"gate_kind"`
				Head        string        `json:"head_sha"`
				UserID      string        `json:"approving_operator_user_id"`
				DisplayName string        `json:"approving_operator_display_name"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.GateKind != core.GateKindMergeApproval || payload.Head == "" || payload.UserID == "" {
				continue
			}
			request, pending = gateMergeRequest{OperatorUserID: payload.UserID, DisplayName: payload.DisplayName, HeadSHA: payload.Head}, true
		case "merge.failed":
			var payload struct {
				Reason string `json:"reason_code"`
			}
			if pending && json.Unmarshal(event.Payload, &payload) == nil && payload.Reason == "forge_merge_failed" {
				pending = false
			}
		}
	}
	return request, pending
}

// gateReviewedHead is the head the merge_approval gate offers to merge.
func (d *Dispatcher) gateReviewedHead(ctx context.Context, task core.Task) string {
	if head := strings.TrimSpace(task.ReviewedHeadSHA); head != "" {
		return head
	}
	return strings.TrimSpace(d.reviewedHeadFromEvents(ctx, task.ID))
}

// peekGateMergeReadinessLocked classifies a merge_approval gate's unmerged
// pull request without side effects: it appends nothing, dispatches nothing,
// and begins no refresh (DEC-64(2,3); DEC-61). An active conflict-fix order
// or an unreadable head is UNKNOWN; a head other than the reviewed head is
// STALE.
func (d *Dispatcher) peekGateMergeReadinessLocked(ctx context.Context, task core.Task, pr github.PullRequest) (string, error) {
	if _, active, err := d.activeImplementationWorkOrder(ctx, task.ID, "merge-conflict"); err != nil {
		return "", err
	} else if active {
		return "UNKNOWN", nil
	}
	reviewed := d.gateReviewedHead(ctx, task)
	if reviewed == "" || pr.HeadSHA == "" {
		return "UNKNOWN", nil
	}
	if pr.HeadSHA != reviewed {
		return "STALE", nil
	}
	switch pr.Mergeable {
	case "MERGEABLE", "CONFLICTING":
		return pr.Mergeable, nil
	}
	return "UNKNOWN", nil
}

// MergeAtGate is the one-act operator merge at the merge_approval gate
// (DEC-64(1)-(6); component-submission-merge). Under the task side-effect
// lock it requests the forge merge of the reviewed head and, only once the
// forge confirms that head merged, records the operator's approval, the
// confirmation, and intervention.merge in one store transaction. Every
// failure leaves the gate pending with no intervention.
func (d *Dispatcher) MergeAtGate(ctx context.Context, task core.Task, actor core.Actor) (core.Task, error) {
	var out core.Task
	err := d.Store.WithTaskSideEffectLock(ctx, task.ID, func(lockedCtx context.Context) error {
		var lockedErr error
		out, lockedErr = d.mergeAtGateLocked(lockedCtx, task.ID, actor)
		return lockedErr
	})
	return out, err
}

func (d *Dispatcher) mergeAtGateLocked(ctx context.Context, taskID string, actor core.Actor) (core.Task, error) {
	current, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return core.Task{}, err
	}
	if current.State == core.TaskMerged {
		// A repeated act answers the merged task and appends nothing (R3).
		return current, nil
	}
	if _, err = core.TransitionGateMerge(current.State, current.GateKind); err != nil {
		return core.Task{}, err
	}
	author, message, err := d.gateMergeAuthor(ctx, actor)
	if err != nil {
		return core.Task{}, err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return core.Task{}, err
	}
	repo, ok := cfg.Repo(current.Repo)
	if !ok || strings.TrimSpace(repo.GitHub) == "" {
		return core.Task{}, &ErrGateMergeNotReady{State: "UNKNOWN"}
	}
	pr, err := d.readPullRequestMergeability(ctx, repo.GitHub, current.Branch)
	if err != nil {
		log.Printf("gate merge readiness for %s: %v", current.ID, err)
		return core.Task{}, &ErrGateMergeNotReady{State: "UNKNOWN"}
	}
	if pr.Merged {
		return d.reconcileGateMergedLocked(ctx, current, repo.GitHub, pr)
	}
	state, err := d.peekGateMergeReadinessLocked(ctx, current, pr)
	if err != nil {
		return core.Task{}, err
	}
	if state != "MERGEABLE" {
		return core.Task{}, &ErrGateMergeNotReady{State: state}
	}
	head := pr.HeadSHA
	requestPayload := map[string]any{"repository": repo.GitHub, "pull_request": pr.Number, "url": pr.URL, "head_sha": head, "gate_kind": core.GateKindMergeApproval}
	addForgeAuthor(requestPayload, author)
	if err = d.Store.AppendEvent(ctx, core.Event{TaskID: current.ID, Kind: "merge.requested", Payload: core.JSONPayload(requestPayload)}); err != nil {
		return core.Task{}, err
	}
	// The forge merges only the reviewed head (DEC-64(5)).
	mergeCtx := github.WithExpectedHead(github.WithMergeMessage(ctx, message), head)
	if err = d.RequestMerge(mergeCtx, repo.GitHub, pr.Number); err != nil {
		// Only a definitive refusal answers the request; a transport or 5xx
		// failure may hide a landed merge, so the request stays pending (R2).
		if github.MergeRefused(err) {
			return core.Task{}, d.recordMergeFailureWithAuthor(ctx, current, "forge_merge_failed", fmt.Errorf("%w: pull request %s#%d: %w", ErrGateMergeRefused, repo.GitHub, pr.Number, err), author)
		}
		return core.Task{}, d.recordMergeFailureWithAuthor(ctx, current, "merge_unconfirmed", fmt.Errorf("%w: pull request %s#%d: %w", ErrGateMergeUnconfirmed, repo.GitHub, pr.Number, err), author)
	}
	confirmed, err := d.ViewPullRequest(ctx, repo.GitHub, current.Branch)
	if err != nil {
		return core.Task{}, d.recordMergeFailureWithAuthor(ctx, current, "merge_unconfirmed", fmt.Errorf("%w: could not re-read pull request %s#%d: %w", ErrGateMergeUnconfirmed, repo.GitHub, pr.Number, err), author)
	}
	if !confirmed.Merged {
		return core.Task{}, d.recordMergeFailureWithAuthor(ctx, current, "merge_unconfirmed", fmt.Errorf("%w: pull request %s#%d is not merged", ErrGateMergeUnconfirmed, repo.GitHub, pr.Number), author)
	}
	if confirmed.HeadSHA != head || confirmed.MergeCommitSHA == "" {
		return core.Task{}, d.recordMergeFailureWithAuthor(ctx, current, "merge_verification_failed", fmt.Errorf("%w: pull request %s#%d merged at %q with merge commit %q, not reviewed head %s", ErrGateMergeUnconfirmed, repo.GitHub, pr.Number, confirmed.HeadSHA, confirmed.MergeCommitSHA, head), author)
	}
	return d.completeGateMergeLocked(ctx, current, repo.GitHub, confirmed, author)
}

// reconcileGateMergedLocked runs the shared observed-merge reconcile for a
// gate whose pull request is already merged when the act arrives.
func (d *Dispatcher) reconcileGateMergedLocked(ctx context.Context, current core.Task, githubRepo string, pr github.PullRequest) (core.Task, error) {
	head, eligible := d.observedMergeExpectedHead(ctx, current)
	if !eligible || pr.MergeCommitSHA == "" {
		return core.Task{}, &ErrGateMergeNotReady{State: "UNKNOWN"}
	}
	matched, err := d.reconcileForgeMergeLocked(ctx, current, githubRepo, pr, head, "merged_outside_conveyor")
	if err != nil {
		return core.Task{}, err
	}
	if !matched {
		return core.Task{}, &ErrGateMergeNotReady{State: "MERGED_HEAD_MISMATCH"}
	}
	return d.Store.GetTask(ctx, current.ID)
}

// gateMergeAuthor resolves the acting operator, not the latest approver, as
// the forge author and Approved-by trailer (DEC-64(4); DEC-59). An agent
// credential acts only as its owning operator (DEC-45; DEC-64(9)).
func (d *Dispatcher) gateMergeAuthor(ctx context.Context, actor core.Actor) (core.ForgeAuthoringIdentity, string, error) {
	author := core.ForgeAuthoringIdentity{Class: core.ForgeAuthorApprovingOperator}
	userID := ""
	if id, ok := strings.CutPrefix(actor.ID, "user:"); ok {
		userID = strings.TrimSpace(id)
	} else if credential, ok := store.CredentialFromContext(ctx); ok {
		userID = strings.TrimSpace(credential.OwnerUserID)
	}
	if userID == "" {
		return author, "", github.PermissionError(fmt.Errorf("merge requires an operator identity"))
	}
	identities, ok := d.Store.(store.CallerIdentityStore)
	if !ok {
		return author, "", fmt.Errorf("approving operator identity store is unavailable")
	}
	identity, err := store.GitAuthorForUser(ctx, identities, userID)
	if err != nil {
		return author, "", err
	}
	if strings.ContainsAny(identity.Name+identity.Email, "\r\n") {
		return author, "", fmt.Errorf("approving operator identity contains a newline")
	}
	author.UserID, author.DisplayName = userID, identity.Name
	return author, fmt.Sprintf("Approved-by: %s <%s>", identity.Name, identity.Email), nil
}

// completeGateMergeLocked commits a confirmed gate merge in one store
// transaction: the operator's approve intervention bound to the merged
// reviewed head, merge.confirmed, and intervention.merge (DEC-64(1,7)).
// The intervention carries the operator as its actor.
func (d *Dispatcher) completeGateMergeLocked(ctx context.Context, current core.Task, githubRepo string, pr github.PullRequest, author core.ForgeAuthoringIdentity) (core.Task, error) {
	payload := map[string]any{
		"repository": githubRepo, "pull_request": pr.Number, "url": pr.URL,
		"base_sha": pr.BaseSHA, "head_sha": pr.HeadSHA, "merged_by": pr.MergedBy,
		"merge_commit_sha": pr.MergeCommitSHA, "gate_kind": core.GateKindMergeApproval,
	}
	addForgeAuthor(payload, author)
	operatorCtx := store.WithActor(ctx, core.Actor{ID: store.UserActorID(author.UserID), Role: core.ActorUser})
	intervention := core.Intervention{TaskID: current.ID, Action: core.InterventionApprove, ReasonCode: "merge-at-gate"}
	out, err := taskops.New(d.Store).MergeAtGate(operatorCtx, intervention, pr.HeadSHA, core.Event{TaskID: current.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(payload)})
	if err != nil {
		return core.Task{}, err
	}
	d.observeConfirmedMerge(ctx, out, githubRepo, pr, "merge.confirmed")
	return out, nil
}

// creditPendingGateMergeLocked completes an observed merge_approval gate
// merge through intervention.merge when a pending request at the merged
// reviewed head exists (DEC-64(7)). It reports false when the DEC-66 recover
// path must run instead.
func (d *Dispatcher) creditPendingGateMergeLocked(ctx context.Context, task core.Task, githubRepo string, pr github.PullRequest, events []core.Event) (bool, error) {
	if task.State != core.TaskAwaiting || task.GateKind != core.GateKindMergeApproval || pr.MergeCommitSHA == "" {
		return false, nil
	}
	request, pending := pendingGateMergeRequest(events)
	if !pending || request.HeadSHA != pr.HeadSHA || request.HeadSHA != d.gateReviewedHead(ctx, task) {
		return false, nil
	}
	author := core.ForgeAuthoringIdentity{Class: core.ForgeAuthorApprovingOperator, UserID: request.OperatorUserID, DisplayName: request.DisplayName}
	if _, err := d.completeGateMergeLocked(ctx, task, githubRepo, pr, author); err != nil {
		return false, err
	}
	return true, nil
}

// creditVisibleGateMergeLocked reads the pull request only when the gate has
// a pending one-act request, and credits that request when its head merged.
func (d *Dispatcher) creditVisibleGateMergeLocked(ctx context.Context, task core.Task) (bool, error) {
	if task.State != core.TaskAwaiting || task.GateKind != core.GateKindMergeApproval {
		return false, nil
	}
	events, err := d.Store.ListEvents(ctx, task.ID)
	if err != nil {
		return false, err
	}
	if _, pending := pendingGateMergeRequest(events); !pending {
		return false, nil
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return false, err
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok || repo.GitHub == "" {
		return false, nil
	}
	pr, err := d.ViewPullRequest(ctx, repo.GitHub, task.Branch)
	if err != nil || !pr.Merged {
		return false, nil
	}
	return d.creditPendingGateMergeLocked(ctx, task, repo.GitHub, pr, events)
}
