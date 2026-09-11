package storetest

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func reviewRound(taskID string, round int) ([]core.Job, []core.WorkOrder) {
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := 1; seat <= 2; seat++ {
		id := core.NewTaskID()
		jobs = append(jobs, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: round, ReviewSeat: seat, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)})
	}
	return jobs, orders
}

func runReviewRounds(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	for _, interrupted := range []bool{false, true} {
		task := newAggregateTask(t, x)
		jobs, orders := reviewRound(task.ID, 1)
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		first, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{SessionID: "seat-one", ClientToken: "one", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		first.State = core.WorkOrderCompleted
		requireOK(t, UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict))
		claim := core.WorkOrderClaim{SessionID: "seat-two", ClientToken: "two", Lease: time.Minute, ExecutionTimeout: time.Nanosecond}
		if interrupted {
			claim.Lease = time.Nanosecond
			claim.ExecutionTimeout = time.Hour
		}
		_, err = ClaimWorkOrder(ctx, st, orders[1].ID, claim)
		requireOK(t, err)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		if interrupted {
			request := store.InterruptedReviewRecoveryRequest{TaskID: task.ID, RequestID: "recover", Round: 1}
			_, err = RecoverInterruptedReviewRound(ctx, st, request, time.Hour)
			requireOK(t, err)
			_, err = RecoverInterruptedReviewRound(ctx, st, request, time.Hour)
			requireOK(t, err)
			retained, err := st.GetWorkOrder(ctx, orders[0].ID)
			requireOK(t, err)
			recovered, err := st.GetWorkOrder(ctx, orders[1].ID)
			requireOK(t, err)
			if retained.State != core.WorkOrderCompleted || recovered.State != core.WorkOrderQueued || recovered.RetrySuppressed {
				t.Fatal("interrupted recovery changed completed seat or failed to recover missing seat")
			}
		} else {
			jobs2, orders2 := reviewRound(task.ID, 2)
			request := store.ReviewRoundRetryRequest{TaskID: task.ID, RequestID: "retry", PriorRound: 1, Reason: "seat timed out", PRHead: "fixture-head"}
			result, err := RetryReviewRound(ctx, st, request, jobs2, orders2)
			requireOK(t, err)
			if result.NewRound != 2 || len(result.WorkOrders) != 2 {
				t.Fatal("retry did not create round two")
			}
			_, err = RetryReviewRound(ctx, st, request, jobs2, orders2)
			requireOK(t, err)
			request.Reason = "different"
			if _, err := RetryReviewRound(ctx, st, request, jobs2, orders2); !errors.Is(err, store.ErrReviewRetryConflict) {
				t.Fatalf("changed retry error=%v", err)
			}
			persisted, err := st.ListTaskWorkOrders(ctx, task.ID)
			requireOK(t, err)
			if len(persisted) != 4 {
				t.Fatal("retry lost historical orders or duplicated seats")
			}
		}
	}
}

func runReviewAcceptance(t *testing.T, x Fixture) {
	t.Run("DoneCriteria", func(t *testing.T) { RunReviewDoneCriteriaAcceptance(t, x) })
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	jobs, orders := reviewRound(task.ID, 1)
	requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
	for i, order := range orders {
		decision := core.ReviewDecision{TaskID: task.ID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: i + 1, Verdict: "approve", ReasonCode: "verified", Summary: "fixture acceptance", MaxBounces: 3}
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision))
	}
	current, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if current.State != core.TaskAwaiting {
		t.Fatalf("accepted review task state=%s", current.State)
	}
	events, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	count := 0
	for _, event := range events {
		if event.Kind == "review.round_completed" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("round completion events=%d", count)
	}
}

// PR907MandatoryValidation is the exact unresolved entry from the accepted
// 260910-9899e4-review-1-seat-1 verdict at head 962caf43aeea0e9a7ff2b32537144fcbecf83499.
const PR907MandatoryValidation = "`make test`, `make test-integration`, and configured-backend `make test-integration-singlestore-ci` pass. Environment limitations are reported separately; focused checks do not substitute for the mandatory aggregates."

const ReviewDoneCriteriaPlan = "## Approach\nVerify acceptance.\n\n## Files touched\n- internal/store/store.go\n\n## Ordering\n1. Verify.\n\n## Risks\n- Contradictory approval.\n\n## Done criteria\n- " + PR907MandatoryValidation

// RunReviewDoneCriteriaAcceptance exercises the actual transactional command in
// every backend, including the supported correction and new-review lifecycle.
func RunReviewDoneCriteriaAcceptance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	for _, gate := range []bool{false, true} {
		for _, source := range []string{"task", "parent", "no-plan", "legacy"} {
			t.Run(fmt.Sprintf("merge-gate=%t/%s", gate, source), func(t *testing.T) {
				id := core.NewTaskID()
				task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, MergeApproval: gate, CreatedAt: time.Now().UTC()}
				if source == "parent" {
					parent := newAggregateTask(t, x)
					spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: parent.ID, Content: ReviewDoneCriteriaPlan, Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
					requireOK(t, err)
					requireOK(t, st.ApproveSpecVersion(ctx, parent.ID, spec.Version))
					task.ParentTaskID, task.OriginSpecVersion = parent.ID, spec.Version
					// A later parent plan cannot displace the child's exact pin.
					_, err = st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: parent.ID, Content: "new unapproved draft", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
					requireOK(t, err)
				}
				requireOK(t, st.CreateTask(ctx, task))
				if source == "task" || source == "legacy" {
					content := ReviewDoneCriteriaPlan
					if source == "legacy" {
						content = "# Legacy blueprint\n## Done criteria\nLegacy acceptance"
					}
					spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: content, Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
					requireOK(t, err)
					requireOK(t, st.ApproveSpecVersion(ctx, task.ID, spec.Version))
					_, err = st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "unapproved draft", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
					requireOK(t, err)
				}
				hasPlan := source == "task" || source == "parent"
				startReview := func(round int) core.ReviewDecision {
					jobs, orders := reviewRound(task.ID, round)
					// One seat keeps the correction/bounce cycle independent of panel size.
					requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs[:1], orders[:1]))
					session := fmt.Sprintf("review-%s-%d", task.ID, round)
					_, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
					requireOK(t, err)
					return core.ReviewDecision{TaskID: task.ID, JobID: jobs[0].ID, ReviewWorkOrderID: orders[0].ID, ReviewRound: round, ReviewSeat: 1, ClaimSession: session, Verdict: "approve", ReasonCode: "verified", Summary: "acceptance fixture", ReviewedCommitSHA: fmt.Sprintf("head-%d", round), PublicationEligible: true, PolicyVersion: 1, MergeApproval: gate, MaxBounces: 3}
				}
				decision := startReview(1)
				if hasPlan {
					cases := []struct {
						name, want string
						assessment *core.DoneCriteriaAssessment
					}{
						{"missing", "assessment is required", nil},
						{"false applicability", "does not match", &core.DoneCriteriaAssessment{Summary: "caller claims no plan"}},
						{"unverified", "unverified", &core.DoneCriteriaAssessment{Applicable: true, Summary: "Environment limitation does not exempt mandatory validation", Satisfied: []string{"behavior tested"}, Unverified: []string{PR907MandatoryValidation}}},
						{"unsatisfied", "unsatisfied", &core.DoneCriteriaAssessment{Applicable: true, Summary: "aggregate failed", Unsatisfied: []string{PR907MandatoryValidation}}},
						{"conflicts", "conflicts", &core.DoneCriteriaAssessment{Applicable: true, Summary: "conflicting evidence", Conflicts: []string{PR907MandatoryValidation}}},
						{"all categories", "unsatisfied, unverified, conflicts", &core.DoneCriteriaAssessment{Applicable: true, Summary: "all blockers", Satisfied: []string{"code"}, Unsatisfied: []string{"failed"}, Unverified: []string{PR907MandatoryValidation}, Conflicts: []string{"conflict"}}},
					}
					for _, tc := range cases {
						t.Run(tc.name, func(t *testing.T) {
							before, err := st.GetTask(ctx, task.ID)
							requireOK(t, err)
							events, err := st.ListEvents(ctx, task.ID)
							requireOK(t, err)
							order, err := st.GetWorkOrder(ctx, decision.ReviewWorkOrderID)
							requireOK(t, err)
							jobs, err := st.ListJobs(ctx, task.ID)
							requireOK(t, err)
							decision.DoneCriteriaAssessment = tc.assessment
							err = taskops.New(st).AcceptReviewDecision(ctx, decision)
							if err == nil || !strings.Contains(err.Error(), tc.want) {
								t.Fatalf("acceptance error=%v, want %s", err, tc.want)
							}
							after, err := st.GetTask(ctx, task.ID)
							requireOK(t, err)
							afterEvents, err := st.ListEvents(ctx, task.ID)
							requireOK(t, err)
							afterOrder, err := st.GetWorkOrder(ctx, decision.ReviewWorkOrderID)
							requireOK(t, err)
							afterJobs, err := st.ListJobs(ctx, task.ID)
							requireOK(t, err)
							if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(events, afterEvents) || !reflect.DeepEqual(order, afterOrder) || !reflect.DeepEqual(jobs, afterJobs) {
								t.Fatal("refused approval changed task, events, claim, or jobs")
							}
							if _, err := st.GetReviewPublication(ctx, decision.ReviewWorkOrderID); err == nil {
								t.Fatal("refused approval became publication eligible")
							}
						})
					}
					decision.Verdict, decision.ReasonCode, decision.Feedback = "changes_requested", "validation", "Run the mandatory aggregate"
					decision.DoneCriteriaAssessment = &core.DoneCriteriaAssessment{Applicable: true, Summary: "truthful unresolved criterion", Unverified: []string{PR907MandatoryValidation}}
					requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision))
					bounced, err := st.GetTask(ctx, task.ID)
					requireOK(t, err)
					if bounced.State != core.TaskQueued || bounced.NextStage != core.StageImplement || bounced.ApprovedHeadSHA != "" {
						t.Fatalf("invalid bounce: %+v", bounced)
					}
					// The fresh implementation must run before a new review round.
					_, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart})
					requireOK(t, err)
					job := core.Job{ID: core.NewTaskID(), TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
					requireOK(t, st.CreateJob(ctx, job))
					requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageImplement}))
					order, err := ClaimWorkOrder(ctx, st, job.ID, core.WorkOrderClaim{SessionID: "fresh-implement-" + task.ID, ClientToken: "fresh-token-" + task.ID, Lease: time.Minute})
					requireOK(t, err)
					order.State = core.WorkOrderSubmitted
					requireOK(t, UpdateWorkOrder(ctx, st, order, core.WorkOrderCmdSubmitForReview))
					job.State = core.JobDone
					requireOK(t, st.UpdateJob(ctx, job))
					_, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
					requireOK(t, err)
					_, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart})
					requireOK(t, err)
					decision = startReview(2)
					decision.DoneCriteriaAssessment = &core.DoneCriteriaAssessment{Applicable: true, Summary: "all required validation completed", Satisfied: []string{PR907MandatoryValidation}}
				}
				requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision))
				current, err := st.GetTask(ctx, task.ID)
				requireOK(t, err)
				want := core.TaskApproved
				if gate {
					want = core.TaskAwaiting
				}
				if current.State != want || current.ReviewedHeadSHA != decision.ReviewedCommitSHA {
					t.Fatalf("complete approval task=%+v", current)
				}
				order, err := st.GetWorkOrder(ctx, decision.ReviewWorkOrderID)
				requireOK(t, err)
				if order.State != core.WorkOrderCompleted {
					t.Fatalf("review did not complete: %+v", order)
				}
				pub, err := st.GetReviewPublication(ctx, decision.ReviewWorkOrderID)
				requireOK(t, err)
				if pub.State != core.ReviewPublicationQueued {
					t.Fatalf("publication=%+v", pub)
				}
			})
		}
	}
}
