package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runNextActionReadFixtures(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	now := time.Now().UTC().Truncate(time.Second)
	read := func(id, kind string) nextaction.NextAction {
		t.Helper()
		action, err := store.ReadNextAction(ctx, st, id, store.NextActionOptions{Now: now})
		if err != nil || action.Kind != kind || action.Label == "" {
			t.Fatalf("task %s kind=%s label=%q want %s err=%v", id, action.Kind, action.Label, kind, err)
		}
		return action
	}
	plane := taskops.New(st)
	for _, tc := range []struct {
		command core.TaskCommand
		kind    string
	}{
		{core.TaskGateSpec, nextaction.KindPlanApproval},
		{core.TaskJobFail, nextaction.KindJobFailure},
	} {
		id := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: tc.command}); err != nil {
			t.Fatal(err)
		}
		if action := read(id, tc.kind); action.Details.GateKind == "" {
			t.Fatalf("%s missing gate kind", tc.kind)
		}
		if tc.kind != nextaction.KindPlanApproval {
			continue
		}
		// The plan gate names the recorded latest plan version
		// (feature-operator-next-action NA-4).
		for i := 0; i < 2; i++ {
			if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: id, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})}); err != nil {
				t.Fatal(err)
			}
		}
		if got := read(id, tc.kind).Question; got != "Is plan v2 the right way to do this? Approving starts implementation." {
			t.Fatalf("plan question = %q", got)
		}
	}
	revision := newPlanRevisionFixture(t, st, ctx, core.StageImplement, true, true)
	if _, err := requestPlanRevision(ctx, st, revision.order.TaskID, revision.order.ID, revision.workerID, revision.sessionID, "the API moved"); err != nil {
		t.Fatal(err)
	}
	read(revision.order.TaskID, nextaction.KindPlanRevision)
	mergeID := createRunningTask(t, st, ctx, workspace)
	acceptReviewRound(t, st, ctx, mergeID, func(order core.WorkOrder, seat int) core.ReviewDecision {
		return core.ReviewDecision{TaskID: mergeID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: seat, Verdict: "approve", ReasonCode: "verified", Summary: "merge gate", ReviewedCommitSHA: "head", PolicyVersion: 1, MergeApproval: true, MaxBounces: 3}
	})
	if got := read(mergeID, nextaction.KindMergeApproval).Question; got != "Review approved the pull request. Merge it?" {
		t.Fatalf("merge question without a recorded pull request = %q", got)
	}
	// The question names the recorded pull request, never an issue number.
	if err := st.AppendEvent(ctx, core.Event{TaskID: mergeID, Kind: "pull_request.opened", ActorRole: core.ActorSystem, At: now, Payload: core.JSONPayload(map[string]any{"number": 4242, "head_sha": "head"})}); err != nil {
		t.Fatal(err)
	}
	if got := read(mergeID, nextaction.KindMergeApproval).Question; got != "Review approved pull request #4242. Merge it?" {
		t.Fatalf("merge question = %q", got)
	}
	bounceID := createRunningTask(t, st, ctx, workspace)
	acceptReviewRound(t, st, ctx, bounceID, func(order core.WorkOrder, seat int) core.ReviewDecision {
		return core.ReviewDecision{TaskID: bounceID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: seat, Verdict: "changes_requested", ReasonCode: "fix", Summary: "bounce", Feedback: "again", MaxBounces: 1}
	})
	if got := read(bounceID, nextaction.KindReviewBounceLimit).Question; got != "Review bounced 1 times. Approve, ask for changes, or reject?" {
		t.Fatalf("bounce question = %q, want the recorded bounce count", got)
	}

	claimedID := orderTask(t, st, ctx, workspace, core.WorkOrder{})
	if _, err := For(st).ClaimWorkOrder(ctx, "order-"+claimedID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", ClaimantID: "agent", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	read(claimedID, nextaction.KindWorking)
	for _, command := range []string{"triage.park", "dispatch.fail_final"} {
		id := createTask(t, st, ctx, workspace, core.TaskParked, "")
		if err := st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "task.state_changed", ActorRole: core.ActorSystem, At: now, Payload: core.JSONPayload(map[string]any{"command": command})}); err != nil {
			t.Fatal(err)
		}
		want := nextaction.KindParked
		if command == "dispatch.fail_final" {
			want = nextaction.KindDispatchExhausted
		}
		read(id, want)
	}
	read(createTask(t, st, ctx, workspace, core.TaskMerged, ""), nextaction.KindTerminal)

	listed, err := st.ListTasksFiltered(ctx, store.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := store.ReadNextActions(ctx, st, listed, store.NextActionOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range listed {
		one, err := store.ReadNextAction(ctx, st, task.ID, store.NextActionOptions{Now: now})
		if err != nil || one.Kind != batch[task.ID].Kind || one.Label != batch[task.ID].Label {
			t.Fatalf("batch drift %s one=%s batch=%s err=%v", task.ID, one.Kind, batch[task.ID].Kind, err)
		}
	}
	runCallerAttentionOrder(t, st, ctx, workspace)
}

func orderTask(t *testing.T, st store.Store, ctx context.Context, workspace string, order core.WorkOrder, commands ...core.WorkOrderCommand) string {
	id := createTask(t, st, ctx, workspace, core.TaskRunning, "")
	if err := st.CreateJob(ctx, core.Job{ID: "job-" + id, TaskID: id, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	order.ID, order.TaskID, order.JobID, order.Stage, order.State = "order-"+id, id, "job-"+id, core.StageImplement, ""
	if err := For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if len(commands) > 0 {
		if err := For(st).UpdateWorkOrder(ctx, order, commands...); err != nil {
			t.Fatalf("update order: %v", err)
		}
	}
	return id
}

func runCallerAttentionOrder(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	user, err := enrollFixtureOwner(ctx, st, workspace, "attention-"+core.NewTaskID())
	if err != nil {
		t.Fatal(err)
	}
	plane := taskops.New(st)
	var ids []string
	for i := 0; i < 3; i++ {
		id := createRunningTask(t, st, ctx, workspace)
		if _, err = plane.Perform(ctx, id, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		if _, err = plane.SetAssignee(ctx, id, user); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		time.Sleep(time.Millisecond)
	}
	running := createRunningTask(t, st, ctx, workspace)
	if _, err = plane.SetAssignee(ctx, running, user); err != nil {
		t.Fatal(err)
	}
	page, err := st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: user, Limit: 1, Offset: 0})
	if err != nil || page.Total != 3 || len(page.Tasks) != 1 {
		t.Fatalf("page=%+v err=%v ids=%v", page, err, ids)
	}
	next, err := st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: user, Limit: 1, Offset: 1})
	if err != nil || len(next.Tasks) != 1 || next.Tasks[0].ID == page.Tasks[0].ID || next.Total != 3 {
		t.Fatalf("offset page=%+v err=%v", next, err)
	}
	if page.Tasks[0].CreatedAt.Before(next.Tasks[0].CreatedAt) {
		t.Fatalf("created_at order first=%s second=%s", page.Tasks[0].ID, next.Tasks[0].ID)
	}
}
