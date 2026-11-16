package storetest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// attentionBodyBytes is the body of every task the body-free count case seeds.
// A count that transfers even one body receives at least this many bytes.
const attentionBodyBytes = 96 << 10

// attentionOpenStates is the state set CountOperatorAttention reads.
var attentionOpenStates = []core.TaskState{core.TaskClaiming, core.TaskQueued, core.TaskRunning, core.TaskAwaiting, core.TaskApproved, core.TaskParked}

// inboxCountItem is the part of one inbox item the count case compares.
type inboxCountItem struct {
	Kind             string              `json:"kind"`
	Band             string              `json:"band"`
	Task             *core.Task          `json:"task"`
	NextAction       *nextaction.Compact `json:"next_action"`
	NoJudgmentNeeded bool                `json:"no_judgment_needed"`
	Proposal         *struct {
		ID string `json:"id"`
	} `json:"proposal"`
}

// runInboxAttentionCount reads the inbox and the attention count through the
// real handler and proves the count equals the decisions band: a stalled task
// and a job-failure gate stay in automatic and out of the count, a blueprint
// anchor and a closed task appear in neither, and a pending document proposal
// and a task-context proposal each count once. The stalled task keeps caller
// attention, because only the count narrows (req-operator-next-action AC-2.5,
// AC-2.7; component-next-action, Band membership and One attention
// definition).
func runInboxAttentionCount(t *testing.T, x Fixture) {
	if x.LineageHTTP == nil {
		t.Fatal("conformance fixture must provide LineageHTTP for the inbox attention count")
	}
	st, ctx := x.Backend, x.Context
	handler := x.LineageHTTP(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	create := func(state core.TaskState, parent string) string {
		t.Helper()
		id := core.NewTaskID()
		requireOK(t, st.CreateTask(ctx, core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Inbox count " + id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, ParentTaskID: parent, CreatedAt: now}))
		return id
	}
	// A gate is entered through its recording command, the act that names its
	// kind (req-operator-next-action AC-1.2).
	plane := taskops.New(st)
	gated := func(command core.TaskCommand) string {
		t.Helper()
		id := create(core.TaskRunning, "")
		_, err := plane.Perform(ctx, id, taskops.Command{Kind: command})
		requireOK(t, err)
		return id
	}

	decision := gated(core.TaskGateSpec)
	jobFailure := gated(core.TaskJobFail)
	closed := create(core.TaskClosed, "")
	anchor := gated(core.TaskGateSpec)
	child := create(core.TaskRunning, anchor)

	stalled := create(core.TaskQueued, "")
	order := stalled + "-implement-1"
	requireOK(t, st.CreateJob(ctx, core.Job{ID: order, TaskID: stalled, Stage: core.StageImplement, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: order, TaskID: stalled, JobID: order, Stage: core.StageImplement}))
	claimed, err := For(st).ClaimWorkOrder(ctx, order, core.WorkOrderClaim{SessionID: "inbox-count-session", ClientToken: "inbox-count-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	claimed.State = core.WorkOrderTimedOut
	requireOK(t, UpdateWorkOrder(ctx, st, claimed, core.WorkOrderCmdTimeout))

	suffix := core.NewTaskID()[:8]
	confirmed, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "cw-inbox-count-confirmed-" + suffix, Title: "Inbox count confirmed", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("initial"), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmSystemDesignVersion(ctx, confirmed.ID, initial.Version)
	requireOK(t, err)
	contextTask := create(core.TaskRunning, "")
	_, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: contextTask, TargetKind: core.TaskContextProposalSystemDesign, TargetID: confirmed.ID, Source: core.TaskContextProposalTriage, Justification: "Triage suggests this context."})
	requireOK(t, err)
	pending, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "cw-inbox-count-pending-" + suffix, Title: "Inbox count pending", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("proposed"), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)

	decisions, decisionsTotal := readInboxBand(t, handler, x.Workspace, "decisions")
	var decisionTasks, proposals []string
	for _, item := range decisions {
		switch {
		case item.Task != nil:
			decisionTasks = append(decisionTasks, item.Task.ID)
		case item.Kind == "document_proposal" && item.Proposal != nil:
			proposals = append(proposals, item.Proposal.ID)
		default:
			t.Fatalf("unexpected decisions item %+v", item)
		}
	}
	slices.Sort(decisionTasks)
	wantTasks := []string{contextTask, decision}
	slices.Sort(wantTasks)
	if !slices.Equal(decisionTasks, wantTasks) {
		t.Fatalf("decisions band tasks = %v, want %v", decisionTasks, wantTasks)
	}
	if !slices.Equal(proposals, []string{pending.ID}) {
		t.Fatalf("decisions band proposals = %v, want %s only", proposals, pending.ID)
	}

	automatic, _ := readInboxBand(t, handler, x.Workspace, "automatic")
	listed := map[string]inboxCountItem{}
	for _, item := range automatic {
		if item.Task != nil {
			listed[item.Task.ID] = item
		}
	}
	for id, kind := range map[string]string{stalled: nextaction.KindStalled, jobFailure: string(core.GateKindJobFailure)} {
		item, ok := listed[id]
		if !ok || item.NextAction == nil || item.NextAction.Kind != kind || item.NextAction.Actor != nextaction.ActorOperator || item.NextAction.Judgment || !item.NoJudgmentNeeded {
			t.Fatalf("automatic item for %s (%s) = %+v found=%t, want a no-judgment operator item", id, kind, item, ok)
		}
	}
	for _, id := range []string{anchor, closed} {
		if _, ok := listed[id]; ok || slices.Contains(decisionTasks, id) {
			t.Fatalf("task %s is listed in the inbox", id)
		}
	}
	if _, ok := listed[child]; !ok {
		t.Fatalf("anchor child %s is missing from automatic: %v", child, listed)
	}

	var projection struct {
		Attention struct {
			TaskCount            int `json:"task_count"`
			PendingProposalCount int `json:"pending_proposal_count"`
			OperatorDecisions    int `json:"operator_decisions"`
			Total                int `json:"total"`
		} `json:"attention"`
	}
	claimFailureGet(t, handler, x.Workspace, "/v1/pending-proposals", &projection)
	attention := projection.Attention
	if attention.OperatorDecisions != decisionsTotal || attention.Total != decisionsTotal || decisionsTotal != len(decisions) {
		t.Fatalf("attention %+v differs from the %d decisions (%d listed)", attention, decisionsTotal, len(decisions))
	}
	if attention.TaskCount != len(decisionTasks) || attention.PendingProposalCount != len(proposals) {
		t.Fatalf("attention %+v, want %d tasks and %d proposals", attention, len(decisionTasks), len(proposals))
	}
	count, err := st.CountOperatorAttention(ctx)
	requireOK(t, err)
	if count != len(decisionTasks) {
		t.Fatalf("CountOperatorAttention = %d, want %d", count, len(decisionTasks))
	}

	// Caller attention keeps actor=operator: the stalled task the count leaves
	// out still needs attention on its detail and in its assignee's list.
	var detail struct {
		NeedsAttention bool `json:"needs_attention"`
	}
	claimFailureGet(t, handler, x.Workspace, "/v1/tasks/"+stalled+"/activity", &detail)
	if !detail.NeedsAttention {
		t.Fatalf("stalled task lost needs_attention")
	}
	user, err := enrollFixtureOwner(ctx, st, x.Workspace, "inbox-count-"+suffix)
	requireOK(t, err)
	_, err = plane.SetAssignee(ctx, stalled, user)
	requireOK(t, err)
	page, err := st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: user, Limit: 10})
	requireOK(t, err)
	if page.Total != 1 || len(page.Tasks) != 1 || page.Tasks[0].ID != stalled {
		t.Fatalf("caller attention = %+v, want the stalled task", page)
	}
}

// runInboxAttentionCountBodyFree proves CountOperatorAttention transfers no
// task body and keeps its answer. It seeds a task for each next-action kind it
// can reach through public store acts, every one with an attentionBodyBytes
// body except the plan-revision fixture's own task, and requires:
//
//   - ListTasksFilteredWithoutBody to return ListTasksFiltered's rows with only
//     Body cleared, so the projection reads every other field it read before;
//   - ReadNextActions over those rows to equal ReadNextActions over the full
//     rows, compact and complete, for every task;
//   - the count to equal the previous read's count, the inbox decisions band,
//     and the attention /v1/pending-proposals states, with the blueprint anchor
//     excluded and the pending proposal counted once;
//   - the bytes the count receives, under one read label, to stay below one
//     seeded body, while ListTasksFiltered receives every seeded body
//     (req-operator-next-action AC-2.7; component-next-action, One attention
//     definition; component-persistence, Read labels).
func runInboxAttentionCountBodyFree(t *testing.T, x Fixture) {
	if x.LineageHTTP == nil {
		t.Fatal("conformance fixture must provide LineageHTTP for the body-free attention count")
	}
	st, ctx := x.Backend, x.Context
	handler := x.LineageHTTP(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	body := strings.Repeat("b", attentionBodyBytes)
	plane := taskops.New(st)
	seededBodies := 0
	seed := func(task core.Task, dependencies ...string) string {
		t.Helper()
		task.ID = core.NewTaskID()
		task.Workspace, task.Repo, task.Title, task.Body = x.Workspace, "conveyor", "Body-free count "+task.ID, body
		task.BaseBranch, task.Branch, task.CreatedAt = "main", "conveyor/task-"+task.ID, now
		requireOK(t, st.CreateTaskWithDependencies(ctx, task, dependencies))
		if !core.TaskTerminal(task.State) {
			seededBodies += len(body)
		}
		return task.ID
	}
	event := func(id, kind string, payload map[string]any) {
		t.Helper()
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: id, Kind: kind, ActorRole: core.ActorSystem, At: now, Payload: core.JSONPayload(payload)}))
	}
	order := func(id string) string {
		t.Helper()
		orderID := id + "-implement-1"
		requireOK(t, st.CreateJob(ctx, core.Job{ID: orderID, TaskID: id, Stage: core.StageImplement, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: id, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}))
		return orderID
	}
	claim := func(orderID string) core.WorkOrder {
		t.Helper()
		claimed, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{ClaimantID: "worker-" + orderID, SessionID: "session-" + orderID, ClientToken: "token", Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		return claimed
	}
	gated := func(command core.TaskCommand) string {
		t.Helper()
		id := seed(core.Task{State: core.TaskRunning})
		_, err := plane.Perform(ctx, id, taskops.Command{Kind: command})
		requireOK(t, err)
		return id
	}
	want := map[string]string{}
	restoreRecoveryBound := setAutomaticRecoveryBound(t, st, ctx, 1)
	recoveryTask, recoveryOrders := seedReviewRoundWithVerdict(t, st, ctx, x.Workspace)
	exhaustReviewSeat(t, st, ctx, recoveryOrders[1].ID)
	restoreRecoveryBound()
	want[recoveryTask.ID] = nextaction.KindReviewRecovery

	want[gated(core.TaskGateSpec)] = nextaction.KindPlanApproval
	want[gated(core.TaskJobFail)] = nextaction.KindJobFailure
	revision := newPlanRevisionFixture(t, st, ctx, core.StageImplement, true, true)
	_, err := requestPlanRevision(ctx, st, revision.order.TaskID, revision.order.ID, revision.workerID, revision.sessionID, "the API moved")
	requireOK(t, err)
	want[revision.order.TaskID] = nextaction.KindPlanRevision

	mergeGate := seed(core.Task{State: core.TaskRunning})
	acceptReviewRound(t, st, ctx, mergeGate, func(order core.WorkOrder, seat int) core.ReviewDecision {
		return core.ReviewDecision{TaskID: mergeGate, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: seat, Verdict: "approve", ReasonCode: "verified", Summary: "merge gate", ReviewedCommitSHA: "head", PolicyVersion: 1, MergeApproval: true, MaxBounces: 3}
	})
	want[mergeGate] = nextaction.KindMergeApproval
	bounce := seed(core.Task{State: core.TaskRunning})
	acceptReviewRound(t, st, ctx, bounce, func(order core.WorkOrder, seat int) core.ReviewDecision {
		return core.ReviewDecision{TaskID: bounce, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: seat, Verdict: "changes_requested", ReasonCode: "fix", Summary: "bounce", Feedback: "again", MaxBounces: 1}
	})
	want[bounce] = nextaction.KindReviewBounceLimit

	parked := seed(core.Task{State: core.TaskParked})
	event(parked, "task.state_changed", map[string]any{"command": "triage.park"})
	want[parked] = nextaction.KindParked
	exhausted := seed(core.Task{State: core.TaskParked})
	event(exhausted, "task.state_changed", map[string]any{"command": "dispatch.fail_final"})
	want[exhausted] = nextaction.KindDispatchExhausted

	conflict := seed(core.Task{State: core.TaskApproved, MergeApproval: true})
	event(conflict, "merge.blocked", map[string]any{"approved_head": "a", "new_head": "b"})
	for range 3 {
		event(conflict, "merge.conflict_dispatch_failed", map[string]any{"approved_head": "a", "new_head": "b", "next_retry_at": now.Add(time.Hour)})
	}
	want[conflict] = nextaction.KindMergeConflict
	retrying := seed(core.Task{State: core.TaskApproved, MergeApproval: true})
	event(retrying, "merge.blocked", map[string]any{"approved_head": "a", "new_head": "b"})
	event(retrying, "merge.conflict_dispatch_failed", map[string]any{"approved_head": "a", "new_head": "b", "next_retry_at": now.Add(time.Hour)})
	want[retrying] = nextaction.KindRetrying
	want[seed(core.Task{State: core.TaskApproved, MergeApproval: true})] = nextaction.KindMergeExecution
	want[seed(core.Task{State: core.TaskApproved})] = nextaction.KindMerging
	held := seed(core.Task{State: core.TaskApproved, ApprovedHeadSHA: "held-head"})
	event(held, store.EventCheckHold, map[string]any{"head_sha": "held-head", "reason": nextaction.CheckHoldFailed, "checks": []map[string]any{{"name": "e2e", "state": "failed"}}})
	want[held] = nextaction.KindCheckHold

	queued := seed(core.Task{State: core.TaskQueued, NextStage: core.StageImplement})
	order(queued)
	want[queued] = nextaction.KindQueued
	working := seed(core.Task{State: core.TaskRunning})
	claim(order(working))
	want[working] = nextaction.KindWorking
	stalled := seed(core.Task{State: core.TaskRunning})
	timedOut := claim(order(stalled))
	timedOut.State = core.WorkOrderTimedOut
	requireOK(t, UpdateWorkOrder(ctx, st, timedOut, core.WorkOrderCmdTimeout))
	want[stalled] = nextaction.KindStalled
	question := seed(core.Task{State: core.TaskRunning})
	questionOrder := order(question)
	claim(questionOrder)
	_, err = ReleaseWorkerClaim(ctx, st, questionOrder, "worker-"+questionOrder, core.WorkOrderRelease{
		SessionID: "session-" + questionOrder, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Choose the next step."},
	})
	requireOK(t, err)
	want[question] = nextaction.KindAgentQuestion

	forge := seed(core.Task{State: core.TaskRunning})
	event(forge, "merge.failed", map[string]any{"error": "merge refused", "forge_error_category": "permission"})
	want[forge] = nextaction.KindForgeFailure

	blocker := seed(core.Task{State: core.TaskRunning})
	order(blocker)
	blocked := seed(core.Task{State: core.TaskQueued, NextStage: core.StageImplement}, blocker)
	order(blocked)
	want[blocked] = nextaction.KindBlocked
	closedBlocker := seed(core.Task{State: core.TaskQueued})
	stranded := seed(core.Task{State: core.TaskQueued, NextStage: core.StageImplement}, closedBlocker)
	order(stranded)
	_, err = plane.Perform(ctx, closedBlocker, taskops.Command{Kind: core.TaskCancel})
	requireOK(t, err)
	seededBodies -= len(body)
	want[stranded] = nextaction.KindDependencyUnsatisfiable

	suffix := core.NewTaskID()[:8]
	confirmed, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "cw-body-free-confirmed-" + suffix, Title: "Body-free confirmed", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("initial"), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmSystemDesignVersion(ctx, confirmed.ID, initial.Version)
	requireOK(t, err)
	contextTask := seed(core.Task{State: core.TaskRunning})
	_, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: contextTask, TargetKind: core.TaskContextProposalSystemDesign, TargetID: confirmed.ID, Source: core.TaskContextProposalTriage, Justification: "Triage suggests this context."})
	requireOK(t, err)
	want[contextTask] = nextaction.KindContextProposal
	verificationFixture := newVerificationFixture(t, x, true)
	subject := recoveryFixtureRegister(t, &verificationFixture, recoveryFixtureObligation("attention-"+verificationFixture.access.TaskID, "operator_action_required", ""))
	verificationFixture.runID = verificationFixture.apply(t, recoveryFixtureStart("attention", "", subject)).ID
	verificationFixture.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "blocked", Explanation: "fixture environment unavailable"}})
	verificationFixture.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: verificationFixtureCoverage(verificationFixture.snapshot(t)), Feedback: "The exercise needs an operator", RequiredAction: "Restore the fixture"}})
	want[verificationFixture.access.TaskID] = nextaction.KindVerificationDisposition
	_, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: "cw-body-free-pending-" + suffix, Title: "Body-free pending", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("proposed"), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)

	// A gated anchor would count as a decision if the read lost Children.
	anchor := gated(core.TaskGateSpec)
	anchorChild := seed(core.Task{State: core.TaskRunning, ParentTaskID: anchor})
	order(anchorChild)
	seed(core.Task{State: core.TaskClosed})
	assignee, err := enrollFixtureOwner(ctx, st, x.Workspace, "body-free-"+suffix)
	requireOK(t, err)
	_, err = plane.SetAssignee(ctx, working, assignee)
	requireOK(t, err)

	// Record mode keeps an ordinary order ahead of risk; enforce mode waits
	// for the submitted plan's risk rating instead of offering approval.
	policy := func(mode riskpolicy.Mode) config.PolicyContract {
		return config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
	}
	recordTask := seed(core.Task{State: core.TaskRunning, NextStage: core.StageRisk, SetupContract: policy(riskpolicy.ModeRecord)})
	order(recordTask)
	riskOrderID := recordTask + "-risk-1"
	requireOK(t, st.CreateJob(ctx, core.Job{ID: riskOrderID, TaskID: recordTask, Stage: core.StageRisk, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: riskOrderID, TaskID: recordTask, JobID: riskOrderID, Stage: core.StageRisk, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}))
	enforceTask := seed(core.Task{State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded, PolicyVersion: 1, SetupContract: policy(riskpolicy.ModeEnforce)})
	_, err = plane.Perform(ctx, enforceTask, taskops.Command{
		Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true,
		PlanState:   &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted},
		PlanVersion: &core.SpecVersion{Content: planRiskPlan("", ""), Acceptance: []byte(`[]`), Decomposition: []byte(`[]`), Agent: "codex", Model: "planner"},
	})
	requireOK(t, err)

	if seededBodies < 1<<20 {
		t.Fatalf("seeded %d open body bytes, want at least 1 MiB", seededBodies)
	}

	// List reads omit bodies but retain frozen policy. Both spellings agree for
	// the count's filter, an inactive filter, and a narrowing filter; ListTasks
	// remains a whole-row read (req-operator-next-action AC-2.7;
	// component-persistence, Database read
	// accounting).
	openFilter := store.TaskFilter{States: attentionOpenStates}
	listRows := func(name string, filter store.TaskFilter) (lean []core.Task) {
		t.Helper()
		lean, err := st.ListTasksFiltered(ctx, filter)
		requireOK(t, err)
		withoutBody, err := st.ListTasksFilteredWithoutBody(ctx, filter)
		requireOK(t, err)
		if len(lean) == 0 || !reflect.DeepEqual(lean, withoutBody) {
			t.Fatalf("%s filter: ListTasksFilteredWithoutBody differs from ListTasksFiltered:\n lean %+v\n without %+v", name, lean, withoutBody)
		}
		for _, task := range lean {
			if task.Body != "" {
				t.Fatalf("%s filter: list read states task %s with a %d-byte body", name, task.ID, len(task.Body))
			}
		}
		return lean
	}
	listRows("inactive", store.TaskFilter{})
	listRows("query", store.TaskFilter{States: attentionOpenStates, Query: working})
	leanRows := listRows("open", openFilter)
	whole, err := st.ListTasks(ctx)
	requireOK(t, err)
	wholeBodies := 0
	for _, task := range whole {
		wholeBodies += len(task.Body)
	}
	if wholeBodies == 0 {
		t.Fatal("ListTasks states no body, so the body-free assertion is vacuous")
	}
	inverted := store.TaskFilter{UpdatedFrom: now, UpdatedTo: now.Add(-time.Hour)}
	if _, err := st.ListTasksFilteredWithoutBody(ctx, inverted); err == nil {
		t.Fatal("ListTasksFilteredWithoutBody accepted an inverted range")
	}
	// split is the previous count's own selection: this workspace's open tasks,
	// and among them the ones that are not blueprint anchors.
	split := func(tasks []core.Task) (open, candidates []core.Task) {
		for _, task := range tasks {
			if task.Workspace != x.Workspace || core.TaskTerminal(task.State) {
				continue
			}
			open = append(open, task)
			if !core.BlueprintAnchor(task) {
				candidates = append(candidates, task)
			}
		}
		return open, candidates
	}
	// The full side is the whole-row read, so a projection over body-free rows
	// is compared with the projection over rows that still state everything
	// (component-persistence, Read labels).
	fullOpen, fullCandidates := split(whole)
	leanOpen, leanCandidates := split(leanRows)
	// The frozen policy the next-action projection reads stays on list rows, so
	// a list view names the same action as the task detail (DEC-61).
	policies := make(map[string]config.PolicyContract, len(whole))
	for _, task := range whole {
		policies[task.ID] = task.SetupContract
	}
	retained := 0
	for _, task := range leanRows {
		if !reflect.DeepEqual(task.SetupContract, policies[task.ID]) {
			t.Fatalf("list read lost task %s's frozen policy: %+v, whole row %+v", task.ID, task.SetupContract, policies[task.ID])
		}
		if task.SetupContract.HasFrozenPolicy() {
			retained++
		}
	}
	if retained == 0 {
		t.Fatal("no seeded task carries a frozen policy, so the retention assertion is vacuous")
	}
	if slices.ContainsFunc(leanCandidates, func(task core.Task) bool { return task.ID == anchor }) {
		t.Fatalf("anchor %s lost its children in the body-free read", anchor)
	}
	var previous map[string]nextaction.NextAction
	for _, compact := range []bool{true, false} {
		was, err := store.ReadNextActions(ctx, st, fullCandidates, store.NextActionOptions{Now: now, OpenTasksLoaded: true, OpenTasks: fullOpen, CompactOnly: compact})
		requireOK(t, err)
		got, err := store.ReadNextActions(ctx, st, leanCandidates, store.NextActionOptions{Now: now, OpenTasksLoaded: true, OpenTasks: leanOpen, CompactOnly: compact})
		requireOK(t, err)
		if !reflect.DeepEqual(got, was) {
			t.Fatalf("compact=%t: projection over body-free rows differs:\n got %+v\n was %+v", compact, got, was)
		}
		if compact {
			previous = was
			continue
		}
		if action := got[enforceTask]; action.Details.ReasonCode != nextaction.PlanRiskRating {
			t.Fatalf("enforcing task does not wait for plan risk: %+v", action)
		}
		if action := got[recordTask]; action.CurrentOrder == nil || action.CurrentOrder.ID != recordTask+"-implement-1" {
			t.Fatalf("record task did not select its implement order: %+v", action)
		}
		for id, kind := range want {
			if action, found := got[id]; !found || action.Kind != kind {
				t.Fatalf("seeded task %s = %+v found=%t, want kind %s", id, action, found, kind)
			}
		}
	}
	// Compare board lines with the full task-detail projection over the same
	// heterogeneous fixtures used by the attention read.
	boardSeen := make(map[string]bool)
	boardKinds := make(map[string]bool)
	for offset := 0; ; {
		board, err := store.ReadBoard(ctx, st, store.BoardOptions{Offset: offset, NextAction: store.NextActionOptions{Now: now}})
		requireOK(t, err)
		for _, item := range board.Items {
			task, err := st.GetTask(ctx, item.ID)
			requireOK(t, err)
			orders, err := st.ListTaskWorkOrders(ctx, item.ID)
			requireOK(t, err)
			detail, err := store.ReadNextActionForTask(ctx, st, task, orders, store.NextActionOptions{Now: now})
			requireOK(t, err)
			if compact := nextaction.CompactFrom(detail); !reflect.DeepEqual(item.NextAction, compact) {
				t.Errorf("task %s board action %+v, detail %+v", item.ID, item.NextAction, compact)
			}
			boardSeen[item.ID] = true
			boardKinds[item.NextAction.Kind] = true
		}
		offset += len(board.Items)
		if offset >= board.Total {
			break
		}
		if len(board.Items) == 0 {
			t.Fatal("board pagination did not advance")
		}
	}
	for id := range want {
		if !boardSeen[id] {
			t.Errorf("scenario task %s missing from board", id)
		}
	}
	expectedKinds := make(map[string]bool)
	for _, kind := range nextaction.Kinds() {
		// Terminal tasks are excluded from the open-only board.
		if kind != nextaction.KindTerminal {
			expectedKinds[kind] = true
		}
	}
	if !reflect.DeepEqual(boardKinds, expectedKinds) {
		t.Errorf("board kinds %v, want catalogue open kinds %v", boardKinds, expectedKinds)
	}
	decisionsWas := 0
	for _, task := range fullCandidates {
		if store.TaskIsInboxDecision(previous[task.ID]) {
			decisionsWas++
		}
	}

	count, err := st.CountOperatorAttention(ctx)
	requireOK(t, err)
	if count != decisionsWas || count == 0 {
		t.Fatalf("CountOperatorAttention = %d, the previous read counted %d", count, decisionsWas)
	}
	decisions, decisionsTotal := readInboxBand(t, handler, x.Workspace, "decisions")
	var decisionTasks, proposals int
	for _, item := range decisions {
		switch {
		case item.Task != nil && item.Task.ID == anchor:
			t.Fatalf("anchor %s is listed among the decisions", anchor)
		case item.Task != nil:
			decisionTasks++
		case item.Kind == "document_proposal" && item.Proposal != nil:
			proposals++
		}
	}
	if count != decisionTasks || proposals == 0 {
		t.Fatalf("CountOperatorAttention = %d, the decisions band lists %d tasks and %d proposals", count, decisionTasks, proposals)
	}
	var projection struct {
		Attention struct {
			TaskCount            int `json:"task_count"`
			PendingProposalCount int `json:"pending_proposal_count"`
			OperatorDecisions    int `json:"operator_decisions"`
			Total                int `json:"total"`
		} `json:"attention"`
	}
	claimFailureGet(t, handler, x.Workspace, "/v1/pending-proposals", &projection)
	attention := projection.Attention
	if attention.TaskCount != decisionTasks || attention.PendingProposalCount != proposals || attention.OperatorDecisions != decisionsTotal || attention.Total != decisionsTotal || decisionsTotal != decisionTasks+proposals {
		t.Fatalf("attention %+v, want %d tasks and %d proposals of %d decisions", attention, decisionTasks, proposals, decisionsTotal)
	}

	// Another workspace's decision and body stay out of this workspace's count
	// and its bytes.
	other := x.Workspace + "-body-free-" + suffix
	otherCtx := store.WithWorkspace(ctx, other)
	bootstrap, ok := st.(interface {
		BootstrapWorkspaceConfig(context.Context, *config.Config) (bool, error)
	})
	if !ok {
		t.Fatal("store cannot bootstrap an isolated workspace")
	}
	_, err = bootstrap.BootstrapWorkspaceConfig(otherCtx, &config.Config{Workspace: other, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}})
	requireOK(t, err)
	otherID := core.NewTaskID()
	requireOK(t, st.CreateTask(otherCtx, core.Task{ID: otherID, Workspace: other, Repo: "conveyor", Title: "Other workspace", Body: body, BaseBranch: "main", Branch: "conveyor/task-" + otherID, State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}))
	after, err := st.CountOperatorAttention(ctx)
	requireOK(t, err)
	if after != count {
		t.Fatalf("another workspace's decision moved the count from %d to %d", count, after)
	}
	// The count discards foreign rows itself, so the read's own workspace scope
	// shows only in its rows: they keep matching ListTasksFiltered, and on a
	// durable backend, which scopes that read, they hold no foreign task.
	for name, filter := range map[string]store.TaskFilter{"foreign open": openFilter, "foreign inactive": {}} {
		lean := listRows(name, filter)
		foreign := slices.ContainsFunc(lean, func(task core.Task) bool { return task.Workspace != x.Workspace })
		if foreign && x.DrainReadBytes != nil {
			t.Fatalf("%s filter: ListTasksFilteredWithoutBody returned another workspace's task", name)
		}
	}
	otherRows, err := st.ListTasksFilteredWithoutBody(otherCtx, openFilter)
	requireOK(t, err)
	if !slices.ContainsFunc(otherRows, func(task core.Task) bool { return task.ID == otherID && task.Body == "" }) {
		t.Fatalf("the other workspace's read = %+v, want its own task %s without a body", otherRows, otherID)
	}

	if x.DrainReadBytes == nil {
		return
	}
	x.DrainReadBytes()
	_, err = st.ListTasks(dbmetrics.WithReadLabel(ctx, "attention-body-control"))
	requireOK(t, err)
	if got := x.DrainReadBytes()["attention-body-control"]; got < uint64(seededBodies) {
		t.Fatalf("the whole-row read received %d bytes, want at least the %d seeded body bytes", got, seededBodies)
	}
	x.DrainReadBytes()
	_, err = st.ListTasksFiltered(dbmetrics.WithReadLabel(ctx, "attention-list-body-free"), openFilter)
	requireOK(t, err)
	if got := x.DrainReadBytes()["attention-list-body-free"]; got >= attentionBodyBytes {
		t.Fatalf("ListTasksFiltered received %d bytes, want fewer than one %d-byte body of the %d seeded body bytes", got, attentionBodyBytes, seededBodies)
	}
	again, err := st.CountOperatorAttention(dbmetrics.WithReadLabel(ctx, "attention-body-free"))
	requireOK(t, err)
	got := x.DrainReadBytes()["attention-body-free"]
	if again != count || got == 0 || got >= attentionBodyBytes {
		t.Fatalf("CountOperatorAttention = %d (want %d) received %d bytes, want more than 0 and fewer than one %d-byte body of the %d seeded body bytes", again, count, got, attentionBodyBytes, seededBodies)
	}
	t.Logf("CountOperatorAttention received %d bytes with %d open body bytes seeded", got, seededBodies)
}

// readInboxBand reads one inbox band and its X-Conveyor-Total header.
func readInboxBand(t *testing.T, handler http.Handler, workspace, band string) ([]inboxCountItem, int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/next-actions?band="+band+"&limit=200", nil)
	request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
	request.Header.Set("X-Workspace-ID", workspace)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("inbox band %s status=%d body=%s", band, response.Code, response.Body.String())
	}
	var inbox struct {
		Items []inboxCountItem `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &inbox); err != nil {
		t.Fatalf("inbox band %s: %v", band, err)
	}
	total, err := strconv.Atoi(response.Header().Get("X-Conveyor-Total"))
	if err != nil {
		t.Fatalf("inbox band %s total header: %v", band, err)
	}
	return inbox.Items, total
}
