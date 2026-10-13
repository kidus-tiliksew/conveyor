package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Tests for the one-act operator merge at the merge_approval gate
// (DEC-64(1)-(7); component-submission-merge).

const gateHead = "reviewed-head"

var gateOperator = core.Actor{ID: store.UserActorID("usr-approver"), Role: core.ActorUser}

type gateForge struct {
	mu     sync.Mutex
	pr     githubtrigger.PullRequest
	merges int
	answer func(*githubtrigger.PullRequest) error
}

func (f *gateForge) view(context.Context, string, string) (githubtrigger.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pr, nil
}

func (f *gateForge) merge(ctx context.Context, _ string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges++
	if head := githubtrigger.ExpectedHead(ctx); head != gateHead {
		return githubtrigger.StatusError(http.StatusConflict, errors.New("merge did not pin the reviewed head: "+head))
	}
	if f.answer != nil {
		return f.answer(&f.pr)
	}
	f.pr.Merged, f.pr.State, f.pr.MergeCommitSHA, f.pr.MergedBy = true, "closed", "landed", "conveyor-app"
	return nil
}

func gateMergeFixture(t *testing.T, id string) (context.Context, store.Store, core.Task, *Dispatcher, *gateForge) {
	t.Helper()
	ctx := store.WithActor(forgeReconcileCtx(), gateOperator)
	st := mergeIdentityStore{store.NewMemory()}
	task := enterMergeGate(t, st, ctx, id, gateHead, "", true)
	openPullRequest(t, st, ctx, task.ID, 7, gateHead)
	d := newForgeDispatcher(st)
	forge := &gateForge{pr: githubtrigger.PullRequest{Number: 7, State: "open", Mergeable: "MERGEABLE", HeadSHA: gateHead, HeadRef: task.Branch, URL: "https://github.test/acme/app/pull/7"}}
	d.ViewPullRequest, d.RequestMerge = forge.view, forge.merge
	return ctx, st, task, d, forge
}

func countKind(kinds []string, kind string) int {
	n := 0
	for _, k := range kinds {
		if k == kind {
			n++
		}
	}
	return n
}

func lastPayload(t *testing.T, st store.Store, ctx context.Context, id, kind string) map[string]any {
	t.Helper()
	events, _ := st.ListEvents(ctx, id)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == kind {
			var payload map[string]any
			if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			return payload
		}
	}
	t.Fatalf("no %s event", kind)
	return nil
}

func lastStateCommand(t *testing.T, st store.Store, ctx context.Context, id string) core.TaskCommand {
	t.Helper()
	var changed struct {
		Command core.TaskCommand `json:"command"`
	}
	payload, _ := json.Marshal(lastPayload(t, st, ctx, id, "task.state_changed"))
	_ = json.Unmarshal(payload, &changed)
	return changed.Command
}

func assertGatePending(t *testing.T, st store.Store, ctx context.Context, id string) {
	t.Helper()
	current, err := st.GetTask(ctx, id)
	if err != nil || current.State != core.TaskAwaiting || current.GateKind != core.GateKindMergeApproval || current.ApprovedHeadSHA != "" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	if items, _ := st.ListInterventions(ctx, id); len(items) != 0 {
		t.Fatalf("interventions=%+v", items)
	}
}

func assertGateMergedBy(t *testing.T, st store.Store, ctx context.Context, id, userID string) {
	t.Helper()
	current, err := st.GetTask(ctx, id)
	if err != nil || current.State != core.TaskMerged || current.ApprovedHeadSHA != gateHead {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	items, _ := st.ListInterventions(ctx, id)
	if len(items) != 1 || items[0].Action != core.InterventionApprove || items[0].ActorID != store.UserActorID(userID) {
		t.Fatalf("interventions=%+v", items)
	}
	if confirmed := lastPayload(t, st, ctx, id, "merge.confirmed"); confirmed["approving_operator_user_id"] != userID || confirmed["head_sha"] != gateHead {
		t.Fatalf("merge.confirmed=%v", confirmed)
	}
	if command := lastStateCommand(t, st, ctx, id); command != core.TaskInterventionMerge {
		t.Fatalf("state command=%s", command)
	}
	kinds := eventKinds(t, st, ctx, id)
	if countKind(kinds, "merge.reconciled") != 0 || countKind(kinds, "merge.confirmed") != 1 {
		t.Fatalf("events=%v", kinds)
	}
}

func TestMergeAtGateOneActMerges(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-one-act")
	before := len(eventKinds(t, st, ctx, task.ID))
	out, err := d.MergeAtGate(ctx, task, gateOperator)
	if err != nil || out.State != core.TaskMerged {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	assertGateMergedBy(t, st, ctx, task.ID, "usr-approver")
	kinds := eventKinds(t, st, ctx, task.ID)[before:]
	want := []string{"merge.requested", "intervention.approve", "approval.bound", "merge.confirmed", "task.state_changed"}
	for i, kind := range want {
		if i >= len(kinds) || kinds[i] != kind {
			t.Fatalf("appended=%v want prefix %v", kinds, want)
		}
	}
	if requested := lastPayload(t, st, ctx, task.ID, "merge.requested"); requested["head_sha"] != gateHead || requested["approving_operator_user_id"] != "usr-approver" || requested["gate_kind"] != string(core.GateKindMergeApproval) {
		t.Fatalf("merge.requested=%v", requested)
	}
	if forge.merges != 1 {
		t.Fatalf("merges=%d", forge.merges)
	}
	// A repeated act answers the merged task and appends nothing (R3).
	after := len(eventKinds(t, st, ctx, task.ID))
	if again, err := d.MergeAtGate(ctx, task, gateOperator); err != nil || again.State != core.TaskMerged || len(eventKinds(t, st, ctx, task.ID)) != after || forge.merges != 1 {
		t.Fatalf("repeat out=%+v err=%v merges=%d", again, err, forge.merges)
	}
}

func TestMergeAtGateRefusesUnlessMergeable(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		pr         func(*githubtrigger.PullRequest)
		conflict   bool
	}{
		{name: "conflicting", want: "CONFLICTING", pr: func(pr *githubtrigger.PullRequest) { pr.Mergeable = "CONFLICTING" }},
		{name: "stale", want: "STALE", pr: func(pr *githubtrigger.PullRequest) { pr.HeadSHA = "moved-head" }},
		{name: "unknown", want: "UNKNOWN", pr: func(pr *githubtrigger.PullRequest) { pr.Mergeable = "UNKNOWN" }},
		{name: "active conflict fix", want: "UNKNOWN", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, task, d, forge := gateMergeFixture(t, "gate-refuse-"+tc.want)
			if tc.pr != nil {
				tc.pr(&forge.pr)
			}
			if tc.conflict {
				job := core.Job{ID: task.ID + "-fix", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
				if err := st.CreateJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, ReasonCode: "merge-conflict"}); err != nil {
					t.Fatal(err)
				}
			}
			before := len(eventKinds(t, st, ctx, task.ID))
			orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
			_, err := d.MergeAtGate(ctx, task, gateOperator)
			var notReady *ErrGateMergeNotReady
			if !errors.As(err, &notReady) || notReady.State != tc.want {
				t.Fatalf("err=%v want readiness %s", err, tc.want)
			}
			if after := len(eventKinds(t, st, ctx, task.ID)); after != before || forge.merges != 0 {
				t.Fatalf("appended %d events merges=%d", after-before, forge.merges)
			}
			if now, _ := st.ListTaskWorkOrders(ctx, task.ID); len(now) != len(orders) {
				t.Fatalf("orders=%+v", now)
			}
			assertGatePending(t, st, ctx, task.ID)
		})
	}
}

func TestMergeAtGateRefusedOutsideGate(t *testing.T) {
	ctx, st, task, d, _ := gateMergeFixture(t, "gate-outside")
	if err := d.HandleIntervention(ctx, task, core.Job{}, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove}); err != nil {
		t.Fatal(err)
	}
	approved, _ := st.GetTask(ctx, task.ID)
	if _, err := d.MergeAtGate(ctx, approved, gateOperator); err == nil {
		t.Fatal("approved task merged through the gate act")
	}
}

func TestMergeAtGateForgeRefusal(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-refusal")
	forge.answer = func(*githubtrigger.PullRequest) error {
		return githubtrigger.StatusError(http.StatusMethodNotAllowed, errors.New("405 not mergeable"))
	}
	if _, err := d.MergeAtGate(ctx, task, gateOperator); !errors.Is(err, ErrGateMergeRefused) {
		t.Fatalf("err=%v", err)
	}
	if failed := lastPayload(t, st, ctx, task.ID, "merge.failed"); failed["reason_code"] != "forge_merge_failed" {
		t.Fatalf("merge.failed=%v", failed)
	}
	assertGatePending(t, st, ctx, task.ID)
}

func TestMergeAtGateUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(*githubtrigger.PullRequest) error
	}{
		{name: "not merged", answer: func(*githubtrigger.PullRequest) error { return nil }},
		{name: "5xx", answer: func(*githubtrigger.PullRequest) error {
			return githubtrigger.StatusError(http.StatusBadGateway, errors.New("502"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, task, d, forge := gateMergeFixture(t, "gate-unconfirmed")
			forge.answer = tc.answer
			if _, err := d.MergeAtGate(ctx, task, gateOperator); !errors.Is(err, ErrGateMergeUnconfirmed) {
				t.Fatalf("err=%v", err)
			}
			if failed := lastPayload(t, st, ctx, task.ID, "merge.failed"); failed["reason_code"] != "merge_unconfirmed" {
				t.Fatalf("merge.failed=%v", failed)
			}
			assertGatePending(t, st, ctx, task.ID)
			// The forge merges later: the observation credits the requester.
			forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "someone"
			current, _ := st.GetTask(ctx, task.ID)
			if matched, err := d.ReconcileObservedPullRequest(store.WithActor(ctx, store.SystemActor()), "app", "acme/app", current.ID, forge.pr); err != nil || !matched {
				t.Fatalf("matched=%v err=%v", matched, err)
			}
			assertGateMergedBy(t, st, ctx, task.ID, "usr-approver")
		})
	}
}

func TestMergeAtGateVerificationFailed(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-verification")
	forge.answer = func(pr *githubtrigger.PullRequest) error {
		pr.Merged, pr.State, pr.HeadSHA, pr.MergeCommitSHA = true, "closed", "other-head", "landed"
		return nil
	}
	if _, err := d.MergeAtGate(ctx, task, gateOperator); !errors.Is(err, ErrGateMergeUnconfirmed) {
		t.Fatalf("err=%v", err)
	}
	if failed := lastPayload(t, st, ctx, task.ID, "merge.failed"); failed["reason_code"] != "merge_verification_failed" {
		t.Fatalf("merge.failed=%v", failed)
	}
	assertGatePending(t, st, ctx, task.ID)
}

func TestObservedGateMergeCreditsPendingRequest(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-observed-pending")
	forge.answer = func(*githubtrigger.PullRequest) error { return errors.New("connection reset") }
	_, _ = d.MergeAtGate(ctx, task, gateOperator)
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "conveyor-app"
	if matched, err := d.ReconcileObservedPullRequest(store.WithActor(ctx, store.SystemActor()), "app", "acme/app", task.ID, forge.pr); err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	assertGateMergedBy(t, st, ctx, task.ID, "usr-approver")
}

func assertRecoveredOutside(t *testing.T, st store.Store, ctx context.Context, id string) {
	t.Helper()
	current, _ := st.GetTask(ctx, id)
	if current.State != core.TaskMerged || lastStateCommand(t, st, ctx, id) != core.TaskMergeRecover {
		t.Fatalf("task=%+v command=%s", current, lastStateCommand(t, st, ctx, id))
	}
	if reconciled := reconciledPayload(t, st, ctx, id); reconciled["result"] != "merged_outside_conveyor" || reconciled["merged_by"] != "github-operator" {
		t.Fatalf("merge.reconciled=%v", reconciled)
	}
	if items, _ := st.ListInterventions(ctx, id); len(items) != 0 {
		t.Fatalf("interventions=%+v", items)
	}
}

func observeOutside(t *testing.T, ctx context.Context, d *Dispatcher, forge *gateForge, id string) {
	t.Helper()
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "github-operator"
	if matched, err := d.ReconcileObservedPullRequest(store.WithActor(ctx, store.SystemActor()), "app", "acme/app", id, forge.pr); err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
}

func TestObservedGateMergeNoRequest(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-observed-none")
	observeOutside(t, ctx, d, forge, task.ID)
	assertRecoveredOutside(t, st, ctx, task.ID)
}

func TestObservedGateMergeAfterForgeRefusalUsesRecover(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-observed-refused")
	forge.answer = func(*githubtrigger.PullRequest) error {
		return githubtrigger.StatusError(http.StatusConflict, errors.New("409 head moved"))
	}
	_, _ = d.MergeAtGate(ctx, task, gateOperator)
	observeOutside(t, ctx, d, forge, task.ID)
	assertRecoveredOutside(t, st, ctx, task.ID)
}

func TestObservedGateMergeOtherHeadRequest(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-observed-other")
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.requested", Payload: core.JSONPayload(map[string]any{"head_sha": "other-head", "gate_kind": core.GateKindMergeApproval, "approving_operator_user_id": "usr-approver"})}); err != nil {
		t.Fatal(err)
	}
	observeOutside(t, ctx, d, forge, task.ID)
	assertRecoveredOutside(t, st, ctx, task.ID)
}

func TestPendingGateMergeRequestIgnoresEarlierEpisode(t *testing.T) {
	request := core.JSONPayload(map[string]any{"head_sha": gateHead, "gate_kind": core.GateKindMergeApproval, "approving_operator_user_id": "usr-approver"})
	gate := core.JSONPayload(map[string]any{"command": core.TaskGateMerge})
	refused := core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})
	unconfirmed := core.JSONPayload(map[string]any{"reason_code": "merge_unconfirmed"})
	for _, tc := range []struct {
		name   string
		events []core.Event
		want   bool
	}{
		{name: "no gate", events: []core.Event{{Kind: "merge.requested", Payload: request}}},
		{name: "earlier episode", events: []core.Event{{Kind: "task.state_changed", Payload: gate}, {Kind: "merge.requested", Payload: request}, {Kind: "task.state_changed", Payload: gate}}},
		{name: "other command after gate", events: []core.Event{{Kind: "task.state_changed", Payload: gate}, {Kind: "merge.requested", Payload: request}, {Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"command": core.TaskMergeRecover})}}, want: true},
		{name: "current episode", events: []core.Event{{Kind: "task.state_changed", Payload: gate}, {Kind: "merge.requested", Payload: request}}, want: true},
		{name: "refused", events: []core.Event{{Kind: "task.state_changed", Payload: gate}, {Kind: "merge.requested", Payload: request}, {Kind: "merge.failed", Payload: refused}}},
		{name: "unconfirmed stays pending", events: []core.Event{{Kind: "task.state_changed", Payload: gate}, {Kind: "merge.requested", Payload: request}, {Kind: "merge.failed", Payload: unconfirmed}}, want: true},
	} {
		if got, pending := pendingGateMergeRequest(tc.events); pending != tc.want || (pending && got.OperatorUserID != "usr-approver") {
			t.Fatalf("%s: pending=%v request=%+v", tc.name, pending, got)
		}
	}
}

// TestGateEpisodeAfterRefreshRoundAcceptedThroughReviewAccept drives a gate-on
// task through a refresh round (review.accept, DEC-64(8)), then a first
// acceptance that enters the merge_approval gate, then an unconfirmed operator
// merge observed at the head. The refresh round leaves no gate.merge in the
// log, so the real gate episode owns its request (component-submission-merge v6).
func TestGateEpisodeAfterRefreshRoundAcceptedThroughReviewAccept(t *testing.T) {
	ctx := store.WithActor(forgeReconcileCtx(), gateOperator)
	st := mergeIdentityStore{store.NewMemory()}
	id := "gate-after-refresh"
	task := core.Task{
		ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id,
		State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "refresh-base", ApprovedHeadSHA: "refresh-base",
		RefreshBaselineSHA: "refresh-base", RefreshHeadSHA: "refresh-head", RefreshReviewScope: "delta",
		MergeApproval: true, PolicyVersion: 1, Level: core.L2, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	plane := taskops.New(st)
	acceptRound := func(round int, kind, head string) {
		t.Helper()
		jobID := fmt.Sprintf("%s-review-%d", id, round)
		job := core.Job{ID: jobID, TaskID: id, Stage: core.StageReview, State: core.JobPending}
		order := core.WorkOrder{ID: jobID, JobID: jobID, TaskID: id, Stage: core.StageReview, ReviewRound: round, ReviewSeat: 1,
			QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)}
		if err := storetest.CreateReviewRound(ctx, st, id, []core.Job{job}, []core.WorkOrder{order}); err != nil {
			t.Fatal(err)
		}
		decision := core.ReviewDecision{TaskID: id, JobID: jobID, ReviewWorkOrderID: jobID, ReviewRound: round, ReviewSeat: 1,
			Verdict: "approve", ReasonCode: "verified", Summary: "accepted", MaxBounces: 3, ReviewedCommitSHA: head,
			ReviewKind: kind, Level: core.L2, PolicyVersion: 1, MergeApproval: true}
		if kind == "refresh" {
			decision.BaselineSHA, decision.HeadSHA = "refresh-base", head
		}
		if err := plane.AcceptReviewDecision(ctx, decision); err != nil {
			t.Fatal(err)
		}
	}
	acceptRound(1, "refresh", "refresh-head")
	if command := lastStateCommand(t, st, ctx, id); command != core.TaskReviewAccept {
		t.Fatalf("refresh round command=%s, want review.accept", command)
	}
	for _, command := range []taskops.Command{
		{Kind: core.TaskRefreshReview, NextStage: core.StageReview},
		{Kind: core.TaskDispatchStart},
	} {
		if _, err := plane.Perform(ctx, id, command); err != nil {
			t.Fatalf("%s: %v", command.Kind, err)
		}
	}
	acceptRound(2, "", gateHead)
	assertGatePendingState(t, st, ctx, id)
	openPullRequest(t, st, ctx, id, 7, gateHead)
	d := newForgeDispatcher(st)
	forge := &gateForge{pr: githubtrigger.PullRequest{Number: 7, State: "open", Mergeable: "MERGEABLE", HeadSHA: gateHead, HeadRef: task.Branch, URL: "https://github.test/acme/app/pull/7"}}
	d.ViewPullRequest, d.RequestMerge = forge.view, forge.merge
	forge.answer = func(*githubtrigger.PullRequest) error { return errors.New("connection reset") }
	current, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.MergeAtGate(ctx, current, gateOperator)
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "conveyor-app"
	if matched, err := d.ReconcileObservedPullRequest(store.WithActor(ctx, store.SystemActor()), "app", "acme/app", id, forge.pr); err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	assertGateMergedBy(t, st, ctx, id, "usr-approver")
	events, err := st.ListEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var commands []core.TaskCommand
	for _, event := range events {
		var changed struct {
			Command core.TaskCommand `json:"command"`
		}
		if event.Kind == "task.state_changed" && json.Unmarshal(event.Payload, &changed) == nil &&
			(changed.Command == core.TaskGateMerge || changed.Command == core.TaskReviewAccept) {
			commands = append(commands, changed.Command)
		}
	}
	if want := []core.TaskCommand{core.TaskReviewAccept, core.TaskGateMerge}; !reflect.DeepEqual(commands, want) {
		t.Fatalf("acceptance commands=%v, want %v (no gate.merge from the refresh round)", commands, want)
	}
}

func assertGatePendingState(t *testing.T, st store.Store, ctx context.Context, id string) {
	t.Helper()
	current, err := st.GetTask(ctx, id)
	if err != nil || current.State != core.TaskAwaiting || current.GateKind != core.GateKindMergeApproval || current.ReviewedHeadSHA != gateHead {
		t.Fatalf("gate task=%+v err=%v", current, err)
	}
}

func TestMergeAtGateConcurrentMonitorObservation(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-race-monitor")
	entered, release := make(chan struct{}), make(chan struct{})
	forge.answer = func(pr *githubtrigger.PullRequest) error {
		pr.Merged, pr.State, pr.MergeCommitSHA, pr.MergedBy = true, "closed", "landed", "conveyor-app"
		close(entered)
		forge.mu.Unlock()
		<-release
		forge.mu.Lock()
		return nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.MergeAtGate(ctx, task, gateOperator)
		done <- err
	}()
	<-entered
	observed := make(chan error, 1)
	go func() {
		forge.mu.Lock()
		pr := forge.pr
		forge.mu.Unlock()
		_, err := d.ReconcileObservedPullRequest(store.WithActor(ctx, store.SystemActor()), "app", "acme/app", task.ID, pr)
		observed <- err
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	assertGateMergedBy(t, st, ctx, task.ID, "usr-approver")
	if kinds := eventKinds(t, st, ctx, task.ID); countKind(kinds, "task.state_changed") == 0 || countKind(kinds, "merge.reconciled") != 0 {
		t.Fatalf("events=%v", kinds)
	}
}

func TestMergeAtGateForgeMergedByOtherActor(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-other-actor")
	forge.answer = func(pr *githubtrigger.PullRequest) error {
		pr.Merged, pr.State, pr.MergeCommitSHA, pr.MergedBy = true, "closed", "landed", "github-operator"
		return githubtrigger.StatusError(http.StatusMethodNotAllowed, errors.New("405 already merged"))
	}
	if _, err := d.MergeAtGate(ctx, task, gateOperator); !errors.Is(err, ErrGateMergeRefused) {
		t.Fatalf("err=%v", err)
	}
	assertGatePending(t, st, ctx, task.ID)
	if matched, err := d.ReconcileObservedPullRequest(store.WithActor(ctx, store.SystemActor()), "app", "acme/app", task.ID, forge.pr); err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	assertRecoveredOutside(t, st, ctx, task.ID)
}

func TestMergeAtGateDoublePost(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-double")
	var wg sync.WaitGroup
	results := make([]core.Task, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = d.MergeAtGate(ctx, task, gateOperator)
		}()
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil || results[i].State != core.TaskMerged {
			t.Fatalf("post %d out=%+v err=%v", i, results[i], errs[i])
		}
	}
	kinds := eventKinds(t, st, ctx, task.ID)
	if countKind(kinds, "merge.requested") != 1 || forge.merges != 1 {
		t.Fatalf("events=%v merges=%d", kinds, forge.merges)
	}
	assertGateMergedBy(t, st, ctx, task.ID, "usr-approver")
}

func TestApproveAfterPendingGateMergeObserved(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-approve-seam")
	forge.answer = func(*githubtrigger.PullRequest) error { return errors.New("connection reset") }
	_, _ = d.MergeAtGate(ctx, task, gateOperator)
	forge.pr.Merged, forge.pr.State, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "landed", "conveyor-app"
	second := store.WithActor(ctx, core.Actor{ID: store.UserActorID("usr-second"), Role: core.ActorUser})
	err := d.HandleIntervention(second, task, core.Job{}, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove})
	var invalid *core.ErrInvalidTransition
	if !errors.As(err, &invalid) {
		t.Fatalf("approve err=%v", err)
	}
	assertGateMergedBy(t, st, ctx, task.ID, "usr-approver")
}

// TestApproveDoesNotCreditPendingRequestAtOtherHead is the counterexample to
// crediting: a pending one-act request at the reviewed head is not credited
// when the visible merged head differs (DEC-64(7)).
func TestApproveDoesNotCreditPendingRequestAtOtherHead(t *testing.T) {
	ctx, st, task, d, forge := gateMergeFixture(t, "gate-approve-other-head")
	forge.answer = func(*githubtrigger.PullRequest) error { return errors.New("connection reset") }
	_, _ = d.MergeAtGate(ctx, task, gateOperator)
	forge.pr.Merged, forge.pr.State, forge.pr.HeadSHA, forge.pr.MergeCommitSHA, forge.pr.MergedBy = true, "closed", "other-head", "landed", "conveyor-app"
	second := store.WithActor(ctx, core.Actor{ID: store.UserActorID("usr-second"), Role: core.ActorUser})
	_ = d.HandleIntervention(second, task, core.Job{}, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove})
	if kinds := eventKinds(t, st, ctx, task.ID); countKind(kinds, "merge.confirmed") != 0 {
		t.Fatalf("credited a merge at another head: %v", kinds)
	}
	if current, _ := st.GetTask(ctx, task.ID); current.State == core.TaskMerged {
		t.Fatalf("task merged: %+v", current)
	}
}
