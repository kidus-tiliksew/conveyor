package storetest

import (
	"context"
	"errors"
	"strings"
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
		action := read(id, tc.kind)
		if action.Details.GateKind == "" {
			t.Fatalf("%s missing gate kind", tc.kind)
		}
		if tc.kind == nextaction.KindJobFailure {
			// No review approved the state a job failure holds: Approve is
			// withheld, and the route's predicate refuses it.
			requireApproveWithheld(t, st, ctx, id, action)
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
	// A review-sourced limit, recorded without a source, keeps the review
	// wording and Approve.
	got := read(bounceID, nextaction.KindReviewBounceLimit)
	if got.Label != "Settle review" || got.Question != "Review bounced 1 times. Approve, ask for changes, or reject?" {
		t.Fatalf("bounce label=%q question=%q, want the review wording and the recorded bounce count", got.Label, got.Question)
	}
	requireApproveOffered(t, st, ctx, bounceID, got)
	runGateSourceFixtures(t, st, ctx, workspace, now, read)

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

// runGateSourceFixtures reads the store-backed statement of gates whose
// recorded source selects their wording: a bounce limit that planning reached
// with invalid output names planning, and a triage whose output stayed invalid
// fails its job and names triage, never review. A later sourceless job
// failure keeps the base wording (component-triage invariant 26;
// component-next-action invariants 4, 17, 18; req-operator-next-action AC-1.2,
// AC-1.8).
func runGateSourceFixtures(t *testing.T, st store.Store, ctx context.Context, workspace string, now time.Time, read func(string, string) nextaction.NextAction) {
	t.Helper()
	plane := taskops.New(st)
	gate := func(command core.TaskCommand, recovery core.Stage, events ...core.Event) string {
		t.Helper()
		id := createRunningTask(t, st, ctx, workspace)
		for i, event := range events {
			event.TaskID, event.ActorRole, event.At = id, core.ActorSystem, now.Add(time.Duration(i-len(events))*time.Second)
			if err := st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: command, RecoveryStage: recovery, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	planning := gate(core.TaskStageBounceLimit, core.StageSpec,
		core.Event{Kind: "spec.output_invalid", Payload: core.JSONPayload(map[string]any{"error": "plan markdown is required"})},
		core.Event{Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": "spec.output_invalid", "window": 3, "max_bounces": 3})})
	got := read(planning, nextaction.KindReviewBounceLimit)
	if got.Label != "Settle planning output" || got.Summary != "Planning returned invalid output until the bounce limit." ||
		got.Question != "Planning returned invalid output 3 times. Ask for changes, or reject?" || got.Details.GateKind != string(core.GateKindReviewBounceLimit) {
		t.Fatalf("planning-sourced bounce limit=%+v", got)
	}
	if labels := choiceLabels(got.Choices); strings.Join(labels, ",") != "Ask for changes…,Reject" {
		t.Fatalf("planning-sourced bounce limit choices=%v, want redirect and reject with Approve withheld", labels)
	}
	requireApproveWithheld(t, st, ctx, planning, got)
	// A limit review reached with invalid output keeps the review wording and
	// Approve.
	reviewOutput := gate(core.TaskStageBounceLimit, core.StageReview,
		core.Event{Kind: "review.output_invalid", Payload: core.JSONPayload(map[string]any{"error": "verdict is required"})},
		core.Event{Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": core.ReviewOutputInvalidSource, "window": 3, "max_bounces": 3})})
	got = read(reviewOutput, nextaction.KindReviewBounceLimit)
	if got.Label != "Settle review" || choiceLabels(got.Choices)[0] != "Approve" {
		t.Fatalf("review-output bounce limit=%+v, want the review wording with Approve", got)
	}
	requireApproveOffered(t, st, ctx, reviewOutput, got)
	// Every backend transfers at most the first 64 characters of a source. A
	// source that only begins with review's is cut and never read as review's.
	cut := gate(core.TaskStageBounceLimit, core.StageImplement,
		core.Event{Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": core.ReviewOutputInvalidSource + strings.Repeat("x", 80), "window": 3, "max_bounces": 3})})
	got = read(cut, nextaction.KindReviewBounceLimit)
	if got.Label != "Settle bounce limit" {
		t.Fatalf("cut-source bounce limit=%+v, want the unreviewed wording", got)
	}
	requireApproveWithheld(t, st, ctx, cut, got)
	triage := gate(core.TaskJobFail, core.StageTriage,
		core.Event{Kind: "triage.output_invalid", Payload: core.JSONPayload(map[string]any{"error": "triage summary is required"})},
		core.Event{Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": "triage.output_invalid", "window": 3, "max_bounces": 3})},
		core.Event{Kind: "job.failed", Payload: core.JSONPayload(map[string]any{"error": "Triage output stayed invalid after 3 attempts, so triage has no outcome. Last validation error: triage summary is required", "source": "triage.output_invalid"})})
	got = read(triage, nextaction.KindJobFailure)
	if got.Label != "Retry triage" || got.Summary != "Triage returned invalid output until the bounce limit, so it has no outcome." ||
		got.Question != "Retry triage, or reject the task?" || got.Details.GateKind != string(core.GateKindJobFailure) || got.Judgment {
		t.Fatalf("triage job failure=%+v", got)
	}
	requireTriageRetry(t, got)
	requireApproveWithheld(t, st, ctx, triage, got)
	// A later sourceless failure of triage, such as an exceeded authority
	// budget, drops the invalid-output summary and still offers Retry
	// (req-intake-and-triage AC-3.4).
	superseded := gate(core.TaskJobFail, core.StageTriage,
		core.Event{Kind: "job.failed", Payload: core.JSONPayload(map[string]any{"error": "invalid", "source": "triage.output_invalid"})},
		core.Event{Kind: "context.authority_budget_exceeded", Payload: core.JSONPayload(map[string]any{"reason_code": "authority_budget_exceeded"})})
	if got = read(superseded, nextaction.KindJobFailure); got.Label != "Retry triage" || got.Summary != "Triage failed, so it has no outcome." || got.Question != "Retry triage, or reject the task?" {
		t.Fatalf("a later sourceless triage failure kept the invalid-output wording: %+v", got)
	}
	requireTriageRetry(t, got)
	requireApproveWithheld(t, st, ctx, superseded, got)
}

// requireTriageRetry checks a triage job failure's choices: Retry, the
// redirect act, and reject, with no sentence naming review
// (component-next-action invariant 8; req-intake-and-triage AC-3.4).
func requireTriageRetry(t *testing.T, got nextaction.NextAction) {
	t.Helper()
	if labels := choiceLabels(got.Choices); strings.Join(labels, ",") != "Retry,Reject" || got.Choices[0].ID != "redirect" {
		t.Fatalf("triage job failure choices=%+v, want Retry and Reject", got.Choices)
	}
	for _, text := range []string{got.Label, got.Summary, got.Question, got.IfIgnored, got.Choices[0].Label, got.Choices[0].Effect} {
		if strings.Contains(strings.ToLower(text), "review") || strings.Contains(strings.ToLower(text), "changes") {
			t.Fatalf("triage job failure uses a review word: %q", text)
		}
	}
}

// requireApproveWithheld checks that the statement withholds Approve with the
// route's refusal code and plain sentence, and that the route's predicate,
// reading the same history on this backend, refuses it and writes nothing
// (component-next-action invariants 7, 18; req-operator-next-action AC-1.4).
func requireApproveWithheld(t *testing.T, st store.Store, ctx context.Context, id string, got nextaction.NextAction) {
	t.Helper()
	for _, choice := range got.Choices {
		if choice.ID == "approve" {
			t.Fatalf("task %s offers approve: %+v", id, got.Choices)
		}
	}
	want := nextaction.Refusal{Choice: "approve", Text: core.ApproveWithoutReviewRefusal, Code: core.ApproveWithoutReviewCode}
	if len(got.Details.Refusals) != 1 || got.Details.Refusals[0] != want {
		t.Fatalf("task %s refusals=%+v, want %+v", id, got.Details.Refusals, want)
	}
	before, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UnreviewedApproveRefusal(ctx, st, before); !errors.Is(err, store.ErrApproveWithoutReview) {
		t.Fatalf("task %s route refusal=%v, want ErrApproveWithoutReview", id, err)
	}
	after, err := st.GetTask(ctx, id)
	if err != nil || after.State != core.TaskAwaiting || after.GateKind != before.GateKind || after.ApprovedHeadSHA != before.ApprovedHeadSHA {
		t.Fatalf("task %s after a refused approve=%+v err=%v, want it at its gate", id, after, err)
	}
}

// requireApproveOffered checks that a review-sourced gate offers Approve and
// that the route's predicate admits it on this backend.
func requireApproveOffered(t *testing.T, st store.Store, ctx context.Context, id string, got nextaction.NextAction) {
	t.Helper()
	if len(got.Choices) == 0 || got.Choices[0].ID != "approve" || len(got.Details.Refusals) != 0 {
		t.Fatalf("task %s choices=%+v refusals=%+v, want Approve offered", id, got.Choices, got.Details.Refusals)
	}
	task, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UnreviewedApproveRefusal(ctx, st, task); err != nil {
		t.Fatalf("task %s route refusal=%v, want the approve admitted", id, err)
	}
}

func choiceLabels(choices []nextaction.Choice) []string {
	labels := make([]string, 0, len(choices))
	for _, choice := range choices {
		labels = append(labels, choice.Label)
	}
	return labels
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
