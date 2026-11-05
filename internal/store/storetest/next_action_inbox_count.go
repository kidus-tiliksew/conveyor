package storetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

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
	create := func(state core.TaskState, gate core.GateKind, parent string) string {
		t.Helper()
		id := core.NewTaskID()
		requireOK(t, st.CreateTask(ctx, core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Inbox count " + id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, GateKind: gate, ParentTaskID: parent, CreatedAt: now}))
		return id
	}

	decision := create(core.TaskAwaiting, core.GateKindPlanApproval, "")
	jobFailure := create(core.TaskAwaiting, core.GateKindJobFailure, "")
	closed := create(core.TaskClosed, "", "")
	anchor := create(core.TaskAwaiting, core.GateKindPlanApproval, "")
	child := create(core.TaskRunning, "", anchor)

	stalled := create(core.TaskQueued, "", "")
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
	contextTask := create(core.TaskRunning, "", "")
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
	_, err = taskops.New(st).SetAssignee(ctx, stalled, user)
	requireOK(t, err)
	page, err := st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: user, Limit: 10})
	requireOK(t, err)
	if page.Total != 1 || len(page.Tasks) != 1 || page.Tasks[0].ID != stalled {
		t.Fatalf("caller attention = %+v, want the stalled task", page)
	}
}

// readInboxBand reads one inbox band and its X-Conveyor-Total header.
func readInboxBand(t *testing.T, handler http.Handler, workspace, band string) ([]inboxCountItem, int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/next-actions?band="+band+"&limit=500", nil)
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
