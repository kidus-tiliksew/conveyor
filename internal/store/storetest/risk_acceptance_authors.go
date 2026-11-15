package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunRiskAcceptanceAuthorsConformance proves on every backend that the
// acceptance edge of an enforcing task counts the one human approval that an
// untrusted or unknown author makes necessary. Each case reaches its state
// through real store commands: implementation claims, role grants and
// revocations, workspace configuration saves, review rounds, and verdicts.
// An approved round with the merge gate off enters the merge gate when any
// author, a released claim's author included, is not trusted, and takes
// review.accept when every author is trusted and nothing else requires a
// person. Refresh rounds take the same edge. The trust list and each author's
// role are read live at acceptance. A task already approved or waiting at the
// merge gate does not move when trust changes. Record and off tasks keep
// their edge (req-risk-based-review AC-1.1, AC-1.6, AC-8.2, AC-8.4, AC-9.1 to
// AC-9.3, AC-11.1; component-risk-review invariants 16 and 19;
// component-review invariant 21; component-task-lifecycle invariant 5).
func RunRiskAcceptanceAuthorsConformance(t *testing.T, x Fixture) {
	ctx, _ := bootstrapOwner(t, x)
	st := x.Backend
	enforce := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	const repo = "conveyor"

	// person provisions one account and grants it a workspace role:
	// maintainer holds operate_gates and contributor does not.
	emails := map[string]string{}
	person := func(t *testing.T, role core.WorkspaceRole) string {
		t.Helper()
		email := fmt.Sprintf("author-%s-%s@risk.test", role, strings.ToLower(core.NewTaskID()))
		user, err := st.ProvisionIdentityUser(ctx, email, "Author "+string(role))
		requireOK(t, err)
		_, err = st.GrantWorkspaceRole(ctx, email, x.Workspace, role)
		requireOK(t, err)
		emails[user.ID] = email
		return user.ID
	}
	// setRole changes a member's role through the membership grant.
	setRole := func(t *testing.T, userID string, role core.WorkspaceRole) {
		t.Helper()
		_, err := st.GrantWorkspaceRole(ctx, emails[userID], x.Workspace, role)
		requireOK(t, err)
	}
	// setTrusted saves the repository's trusted_authors list through the
	// workspace configuration route's store command.
	setTrusted := func(t *testing.T, ids []string) {
		t.Helper()
		current, err := st.WorkspaceConfig(ctx)
		requireOK(t, err)
		next, err := st.RuntimeConfig(ctx, x.Config)
		requireOK(t, err)
		found := false
		for i := range next.Repos {
			if next.Repos[i].Name == repo {
				next.Repos[i].TrustedAuthors, found = ids, true
			}
		}
		if !found {
			t.Fatalf("workspace configuration has no repository %s", repo)
		}
		_, err = st.UpdateWorkspaceConfig(ctx, current.Version, next)
		requireOK(t, err)
	}
	trustedList := func(ctx context.Context, task core.Task) ([]string, error) {
		cfg, err := st.RuntimeConfig(ctx, x.Config)
		if err != nil {
			return nil, err
		}
		stored, _ := cfg.Repo(task.Repo)
		return stored.TrustedAuthors, nil
	}
	fileTask := func(t *testing.T, policy *riskpolicy.Policy, gate bool) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: repo, Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskRunning, NextStage: core.StageImplement, PolicyVersion: 1, MergeApproval: gate, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: policy, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
			StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "4h", Verify: "1h", Review: "45m"}}
		requireOK(t, st.CreateTask(ctx, task))
		stored, err := st.GetTask(ctx, id)
		requireOK(t, err)
		return stored
	}
	rate := func(t *testing.T, taskID, head string, tier riskpolicy.Tier, approvals int) {
		t.Helper()
		_, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: taskID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule, Tier: tier,
			HeadSHA: head, BaseSHA: "base", DeclarationState: riskpolicy.DeclarationAbsent, Reason: "path rule " + string(tier), HumanApprovals: approvals})
		requireOK(t, err)
	}
	// claimImplement opens one implementation order and claims it with each
	// claimant in turn. Every claim but the last is released, so the last
	// claimant holds the current order and the earlier claimants are named
	// only by the recorded claims.
	claimImplement := func(t *testing.T, taskID string, claimants ...string) {
		t.Helper()
		id := taskID + "-implement-1"
		now := time.Now().UTC()
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageImplement, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}))
		for i, claimant := range claimants {
			session := fmt.Sprintf("%s-implementer-%d", taskID, i)
			_, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: claimant, SessionID: session, ClientToken: session + "-token",
				Agent: "claude-code", Model: "implementer-model", Lease: time.Hour, ExecutionTimeout: time.Hour})
			requireOK(t, err)
			if i == len(claimants)-1 {
				break
			}
			_, err = taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdRelease, func(lease taskops.TaskLease) (core.WorkOrder, error) {
				return st.ReleaseWorkerClaimCommand(ctx, lease, id, core.WorkOrderClaimIdentity{ClaimantID: claimant, SessionID: session},
					core.WorkOrderRelease{SessionID: session, Reason: "harness crashed", Outcome: core.WorkOrderOutcomeChildFailure,
						FailureDetail: "harness crashed", InitialRetryDelay: time.Millisecond, MaximumRetryDelay: time.Millisecond})
			})
			requireOK(t, err)
			time.Sleep(20 * time.Millisecond)
		}
	}
	openRound := func(t *testing.T, task core.Task, head string) {
		t.Helper()
		id := task.ID + "-review-1-seat-1"
		now := time.Now().UTC()
		jobs := []core.Job{{ID: id, TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}}
		orders := []core.WorkOrder{{ID: id, JobID: id, TaskID: task.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1,
			HeadSHA: head, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}}
		seats := store.ReviewRoundSeatResolution{SeatCount: 1, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 1, FloorSeatCount: 1}
		_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
			return struct{}{}, st.CreateReviewRoundCommand(ctx, lease, task.ID, seats, jobs, orders)
		})
		requireOK(t, err)
	}
	approve := func(t *testing.T, task core.Task, head, kind string) core.Task {
		t.Helper()
		id := task.ID + "-review-1-seat-1"
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: id, ReviewWorkOrderID: id, ReviewRound: 1, ReviewSeat: 1,
			Verdict: "approve", ReasonCode: "verified", Summary: "author seat", MaxBounces: 3, ReviewedCommitSHA: head, HeadSHA: head,
			ReviewerModel: "reviewer-model", PolicyVersion: 1, MergeApproval: task.MergeApproval, ReviewKind: kind}))
		stored, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		return stored
	}
	// deliver files a task, claims its implementation order, rates its
	// head, opens one review round, and approves it.
	type delivery struct {
		policy    *riskpolicy.Policy
		gate      bool
		tier      riskpolicy.Tier
		approvals int
		kind      string
		claimants []string
		// beforeAccept runs after the round opens and before the verdict.
		beforeAccept func(t *testing.T)
	}
	deliver := func(t *testing.T, d delivery) core.Task {
		t.Helper()
		const head = "author-head"
		task := fileTask(t, d.policy, d.gate)
		claimImplement(t, task.ID, d.claimants...)
		requireOK(t, ProjectTaskNextStage(ctx, st, task.ID, core.StageReview))
		if d.policy != nil {
			tier := d.tier
			if tier == "" {
				tier = riskpolicy.TierLow
			}
			rate(t, task.ID, head, tier, d.approvals)
		}
		openRound(t, task, head)
		if d.beforeAccept != nil {
			d.beforeAccept(t)
		}
		accepted := approve(t, task, head, d.kind)
		if accepted.ReviewedHeadSHA != head {
			t.Fatalf("reviewed head=%q, want %q", accepted.ReviewedHeadSHA, head)
		}
		return accepted
	}
	requireGate := func(t *testing.T, task core.Task) {
		t.Helper()
		if task.State != core.TaskAwaiting || task.GateKind != core.GateKindMergeApproval {
			t.Fatalf("task state=%s gate=%s, want the merge_approval gate", task.State, task.GateKind)
		}
		if task.ApprovedHeadSHA != "" {
			t.Fatalf("task at the merge gate bound approved head %q", task.ApprovedHeadSHA)
		}
	}
	requireApproved := func(t *testing.T, task core.Task) {
		t.Helper()
		if task.State != core.TaskApproved || task.ApprovedHeadSHA != task.ReviewedHeadSHA {
			t.Fatalf("task state=%s approved head=%q reviewed head=%q, want review.accept to approved", task.State, task.ApprovedHeadSHA, task.ReviewedHeadSHA)
		}
	}
	requireCommand := func(t *testing.T, taskID string, want core.TaskCommand) {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		var last core.TaskCommand
		for _, event := range events {
			var payload struct {
				Command core.TaskCommand `json:"command"`
			}
			if event.Kind == "task.state_changed" && json.Unmarshal(event.Payload, &payload) == nil {
				last = payload.Command
			}
		}
		if last != want {
			t.Fatalf("last task command=%s, want %s", last, want)
		}
	}
	evaluate := func(t *testing.T, taskID string) riskpolicy.ApprovalStatus {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		trusted, err := trustedList(ctx, task)
		requireOK(t, err)
		evaluation, err := store.EvaluateTaskApprovals(ctx, st, task, trusted, "")
		requireOK(t, err)
		return evaluation.Status
	}

	t.Run("TrustedAuthorTakesReviewAccept", func(t *testing.T) {
		author := person(t, core.WorkspaceRoleMaintainer)
		task := deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(author)}})
		requireApproved(t, task)
		requireCommand(t, task.ID, core.TaskReviewAccept)
	})

	t.Run("UntrustedAuthorEntersMergeGate", func(t *testing.T) {
		author := person(t, core.WorkspaceRoleContributor)
		task := deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(author)}})
		requireGate(t, task)
		requireCommand(t, task.ID, core.TaskGateMerge)
		// A review verdict credits no human approval (component-risk-review
		// invariant 10).
		approvals, err := st.ListTaskApprovals(ctx, task.ID)
		requireOK(t, err)
		if len(approvals) != 0 {
			t.Fatalf("acceptance recorded approvals %+v", approvals)
		}
		if status := evaluate(t, task.ID); status.Required != 1 || status.Recorded != 0 || status.Remaining != 1 || !status.UntrustedAuthor || !status.NonAuthorApprovalNeeded {
			t.Fatalf("approval status=%+v, want 0 of 1 with a non-author approval needed", status)
		}
	})

	t.Run("UnknownAuthorEntersMergeGate", func(t *testing.T) {
		// A claim by an identity that names no person makes an author who is
		// never trusted, whatever the trust list says.
		task := deliver(t, delivery{policy: &enforce, claimants: []string{"agent:unattributed-session"}})
		requireGate(t, task)
		if status := evaluate(t, task.ID); status.Required != 1 || !status.UntrustedAuthor {
			t.Fatalf("approval status=%+v, want one approval for an untrusted author", status)
		}
	})

	t.Run("ReleasedClaimAuthorCounts", func(t *testing.T) {
		// The untrusted author's attempt was released, so only the recorded
		// claim names it; the trusted author holds the current order.
		untrusted := person(t, core.WorkspaceRoleContributor)
		trusted := person(t, core.WorkspaceRoleMaintainer)
		task := deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(untrusted), core.TaskRunClaimantID(trusted)}})
		requireGate(t, task)
		// Every claimant trusted takes review.accept.
		both := person(t, core.WorkspaceRoleMaintainer)
		accepted := deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(both), core.TaskRunClaimantID(trusted)}})
		requireApproved(t, accepted)
	})

	t.Run("ExplicitTrustListDecides", func(t *testing.T) {
		gatekeeper := person(t, core.WorkspaceRoleMaintainer)
		listed := person(t, core.WorkspaceRoleContributor)
		setTrusted(t, []string{listed})
		t.Cleanup(func() { setTrusted(t, nil) })
		// A listed author is trusted without operate_gates.
		requireApproved(t, deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(listed)}}))
		// An unlisted author is untrusted even with operate_gates.
		requireGate(t, deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(gatekeeper)}}))
	})

	t.Run("LiveEditsBeforeAcceptanceDecide", func(t *testing.T) {
		// A role grant after the claim and before acceptance makes the author
		// trusted at acceptance: nothing caches the claim-time answer.
		promoted := person(t, core.WorkspaceRoleContributor)
		requireApproved(t, deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(promoted)},
			beforeAccept: func(t *testing.T) { setRole(t, promoted, core.WorkspaceRoleMaintainer) }}))
		// A revoked role makes the author untrusted at acceptance.
		revoked := person(t, core.WorkspaceRoleMaintainer)
		requireGate(t, deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(revoked)},
			beforeAccept: func(t *testing.T) { requireOK(t, st.RevokeWorkspaceRole(ctx, revoked, x.Workspace)) }}))
		// A trust list saved before acceptance without the author makes the
		// author untrusted, and clearing it trusts operate_gates holders
		// again.
		other := person(t, core.WorkspaceRoleMaintainer)
		author := person(t, core.WorkspaceRoleMaintainer)
		t.Cleanup(func() { setTrusted(t, nil) })
		requireGate(t, deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(author)},
			beforeAccept: func(t *testing.T) { setTrusted(t, []string{other}) }}))
		requireApproved(t, deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(author)},
			beforeAccept: func(t *testing.T) { setTrusted(t, nil) }}))
	})

	t.Run("RefreshRoundsUseTheSameCount", func(t *testing.T) {
		untrusted := person(t, core.WorkspaceRoleContributor)
		trusted := person(t, core.WorkspaceRoleMaintainer)
		requireGate(t, deliver(t, delivery{policy: &enforce, kind: "refresh", claimants: []string{core.TaskRunClaimantID(untrusted)}}))
		requireApproved(t, deliver(t, delivery{policy: &enforce, kind: "refresh", claimants: []string{core.TaskRunClaimantID(trusted)}}))
	})

	t.Run("OtherFloorsStillHold", func(t *testing.T) {
		// A trusted author lowers no other floor: the tier, the latest path
		// rating, and the frozen merge gate each still require a person.
		trusted := person(t, core.WorkspaceRoleMaintainer)
		claimant := core.TaskRunClaimantID(trusted)
		requireGate(t, deliver(t, delivery{policy: &enforce, tier: riskpolicy.TierMedium, claimants: []string{claimant}}))
		requireGate(t, deliver(t, delivery{policy: &enforce, approvals: 1, claimants: []string{claimant}}))
		requireGate(t, deliver(t, delivery{policy: &enforce, gate: true, claimants: []string{claimant}}))
		// With an untrusted author the count is the largest floor, not a sum.
		untrusted := person(t, core.WorkspaceRoleContributor)
		held := deliver(t, delivery{policy: &enforce, approvals: 2, claimants: []string{core.TaskRunClaimantID(untrusted)}})
		requireGate(t, held)
		if status := evaluate(t, held.ID); status.Required != 2 || !status.UntrustedAuthor {
			t.Fatalf("approval status=%+v, want 2 required", status)
		}
	})

	t.Run("RecordAndOffKeepTheirEdge", func(t *testing.T) {
		untrusted := person(t, core.WorkspaceRoleContributor)
		claimant := core.TaskRunClaimantID(untrusted)
		requireApproved(t, deliver(t, delivery{policy: &record, claimants: []string{claimant}}))
		requireApproved(t, deliver(t, delivery{policy: &record, kind: "refresh", claimants: []string{"agent:unattributed-session"}}))
		requireApproved(t, deliver(t, delivery{claimants: []string{claimant}}))
	})

	t.Run("TrustEditsMoveNoSettledTask", func(t *testing.T) {
		// An approved task is not repaired when its author becomes untrusted;
		// the automatic merge refusal reads the new count instead
		// (component-submission-merge invariant 64).
		author := person(t, core.WorkspaceRoleMaintainer)
		approved := deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(author)}})
		requireApproved(t, approved)
		setRole(t, author, core.WorkspaceRoleContributor)
		stored, err := st.GetTask(ctx, approved.ID)
		requireOK(t, err)
		requireApproved(t, stored)
		if status := evaluate(t, approved.ID); status.Required != 1 || !status.UntrustedAuthor {
			t.Fatalf("approval status after the role change=%+v, want one required approval", status)
		}
		// A task at the merge gate stays there when its author becomes
		// trusted and the count falls to zero; a person acts on it.
		waiting := person(t, core.WorkspaceRoleContributor)
		held := deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(waiting)}})
		requireGate(t, held)
		setRole(t, waiting, core.WorkspaceRoleMaintainer)
		stored, err = st.GetTask(ctx, held.ID)
		requireOK(t, err)
		requireGate(t, stored)
	})

	t.Run("NextActionReadsTheMergeGate", func(t *testing.T) {
		author := person(t, core.WorkspaceRoleContributor)
		task := deliver(t, delivery{policy: &enforce, claimants: []string{core.TaskRunClaimantID(author)}})
		requireGate(t, task)
		read := func(t *testing.T) nextaction.NextAction {
			t.Helper()
			na, err := store.ReadNextAction(ctx, st, task.ID, store.NextActionOptions{
				Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true}, TrustedAuthors: trustedList,
			})
			requireOK(t, err)
			return na
		}
		na := read(t)
		want := "The reviewed head has 0 of 1 required human approvals. 1 more approval is needed, including an approval from a person who is not an author."
		if na.Kind != nextaction.KindMergeApproval || na.Actor != nextaction.ActorOperator || na.Summary != want {
			t.Fatalf("next action %s/%s summary=%q, want the operator's merge approval with %q", na.Actor, na.Kind, na.Summary, want)
		}
		// A non-author's approval is recorded and counted: 1 of 1 with none
		// remaining.
		gatekeeper := person(t, core.WorkspaceRoleMaintainer)
		_, created, err := st.RecordTaskApproval(ctx, core.TaskApproval{TaskID: task.ID, Kind: core.TaskApprovalPerson, HeadSHA: task.ReviewedHeadSHA, ReviewRound: 1, UserID: gatekeeper, Source: "approval"})
		requireOK(t, err)
		if !created {
			t.Fatal("the approval was not recorded")
		}
		if status := evaluate(t, task.ID); status.Required != 1 || status.Recorded != 1 || status.Remaining != 0 || !status.Satisfied {
			t.Fatalf("approval status=%+v, want 1 of 1 with none remaining", status)
		}
		if na := read(t); na.Kind != nextaction.KindMergeApproval {
			t.Fatalf("next action kind=%s after the approval, want %s", na.Kind, nextaction.KindMergeApproval)
		}
	})
}
