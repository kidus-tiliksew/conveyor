package storetest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runMergeTaskAtGate is the shared one-act gate merge contract (DEC-64(1,7);
// component-task-lifecycle): one transaction records the operator's approve
// intervention, binds the reviewed head, appends merge.confirmed, and applies
// intervention.merge; it refuses outside the merge_approval gate and leaves
// nothing behind when any write fails.
func runMergeTaskAtGate(t *testing.T, x Fixture) {
	st, ctx, workspace := x.Backend, x.Context, x.Workspace
	plane := taskops.New(st)
	operator := store.WithActor(ctx, store.Actor{ID: store.UserActorID("gate-operator"), Role: core.ActorUser})
	gated := func(t *testing.T) string {
		t.Helper()
		id := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	approve := func(id string) core.Intervention {
		return core.Intervention{TaskID: id, Action: core.InterventionApprove, ReasonCode: "merge-at-gate"}
	}
	confirmed := func(id string) core.Event {
		return core.Event{TaskID: id, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"head_sha": "reviewed-head", "approving_operator_user_id": "gate-operator"})}
	}
	snapshot := func(t *testing.T, id string) (core.Task, int, int) {
		t.Helper()
		task, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		events, err := st.ListEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		interventions, err := st.ListInterventions(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return task, len(events), len(interventions)
	}

	t.Run("commits intervention, binding, confirmation, and edge", func(t *testing.T) {
		id := gated(t)
		_, beforeEvents, _ := snapshot(t, id)
		out, err := plane.MergeAtGate(operator, approve(id), "reviewed-head", confirmed(id))
		if err != nil {
			t.Fatal(err)
		}
		if out.State != core.TaskMerged || out.GateKind != "" || out.ApprovedHeadSHA != "reviewed-head" || out.ReviewedHeadSHA != "reviewed-head" {
			t.Fatalf("merged task=%+v", out)
		}
		stored, _, _ := snapshot(t, id)
		if stored.State != core.TaskMerged || stored.GateKind != "" || stored.ApprovedHeadSHA != "reviewed-head" {
			t.Fatalf("stored task=%+v", stored)
		}
		interventions, err := st.ListInterventions(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(interventions) != 1 || interventions[0].Action != core.InterventionApprove || interventions[0].ActorID != store.UserActorID("gate-operator") || interventions[0].ActorRole != core.ActorUser {
			t.Fatalf("interventions=%+v", interventions)
		}
		if userID, ok, err := store.ApprovingOperatorUserID(ctx, st, id); err != nil || !ok || userID != "gate-operator" {
			t.Fatalf("approving operator=%q ok=%v err=%v", userID, ok, err)
		}
		events, err := st.ListEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, event := range events[beforeEvents:] {
			kinds = append(kinds, event.Kind)
		}
		want := []string{"intervention.approve", "approval.bound", "merge.confirmed", "task.state_changed"}
		if len(kinds) < len(want) {
			t.Fatalf("appended=%v", kinds)
		}
		for i, kind := range want {
			if kinds[i] != kind {
				t.Fatalf("appended=%v want prefix %v", kinds, want)
			}
		}
		var changed struct {
			From    core.TaskState   `json:"from"`
			To      core.TaskState   `json:"to"`
			Command core.TaskCommand `json:"command"`
		}
		if err := json.Unmarshal(events[beforeEvents+3].Payload, &changed); err != nil || changed.From != core.TaskAwaiting || changed.To != core.TaskMerged || changed.Command != core.TaskInterventionMerge {
			t.Fatalf("state change=%+v err=%v", changed, err)
		}
		if _, err := plane.MergeAtGate(operator, approve(id), "reviewed-head", confirmed(id)); !isInvalidTransition(err) {
			t.Fatalf("second gate merge err=%v", err)
		}
	})

	t.Run("refuses outside the merge_approval gate", func(t *testing.T) {
		running := createRunningTask(t, st, ctx, workspace)
		planGate := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, planGate, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{running, planGate} {
			before, events, interventions := snapshot(t, id)
			if _, err := plane.MergeAtGate(operator, approve(id), "reviewed-head", confirmed(id)); !isInvalidTransition(err) {
				t.Fatalf("%s gate merge err=%v", before.State, err)
			}
			after, afterEvents, afterInterventions := snapshot(t, id)
			if after.State != before.State || after.GateKind != before.GateKind || after.ApprovedHeadSHA != "" || afterEvents != events || afterInterventions != interventions {
				t.Fatalf("refusal wrote: before=%+v after=%+v events %d->%d interventions %d->%d", before, after, events, afterEvents, interventions, afterInterventions)
			}
		}
	})

	t.Run("a failed event insert leaves no intervention and no state change", func(t *testing.T) {
		id := gated(t)
		before, events, interventions := snapshot(t, id)
		bad := confirmed(id)
		bad.ActorID = "user:bad\x00actor"
		if _, err := plane.MergeAtGate(operator, approve(id), "reviewed-head", bad); err == nil {
			t.Fatal("NUL audit actor accepted")
		}
		after, afterEvents, afterInterventions := snapshot(t, id)
		if after.State != core.TaskAwaiting || after.GateKind != core.GateKindMergeApproval || after.ApprovedHeadSHA != before.ApprovedHeadSHA || after.ReviewedHeadSHA != before.ReviewedHeadSHA || afterEvents != events || afterInterventions != interventions {
			t.Fatalf("rolled-back merge wrote: before=%+v after=%+v events %d->%d interventions %d->%d", before, after, events, afterEvents, interventions, afterInterventions)
		}
	})
}

func isInvalidTransition(err error) bool {
	var invalid *core.ErrInvalidTransition
	return errors.As(err, &invalid)
}

// runCheckStatusDeadline is the shared check-status storage contract
// (req-review-gates-evidence AC-1.5; component-submission-merge, Check status):
// the deadline and its event commit together, a later read of the same head
// neither moves it nor appends, clearing keeps the head, and a new head starts
// fresh. Every task read returns the columns.
func runCheckStatusDeadline(t *testing.T, x Fixture) {
	st, ctx, workspace := x.Backend, x.Context, x.Workspace
	event := func(id, head, reason string) core.Event {
		return core.Event{TaskID: id, Kind: store.EventCheckPending, Payload: core.JSONPayload(map[string]any{"head_sha": head, "reason": reason})}
	}
	count := func(t *testing.T, id string) int {
		t.Helper()
		events, err := st.ListCheckStatusEvents(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		return len(events[id])
	}
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	id := createRunningTask(t, st, ctx, workspace)

	fresh, err := st.GetTask(ctx, id)
	if err != nil || fresh.CheckStatusDeadline != nil || fresh.CheckStatusDeadlineHead != "" {
		t.Fatalf("new task deadline=%v head=%q err=%v", fresh.CheckStatusDeadline, fresh.CheckStatusDeadlineHead, err)
	}
	recorded, err := st.RecordCheckStatusDeadline(ctx, id, "head-a", base, event(id, "head-a", "pending"))
	if err != nil || !recorded {
		t.Fatalf("first record=%t err=%v", recorded, err)
	}
	read := func(t *testing.T, wantHead string, want *time.Time) {
		t.Helper()
		got, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.CheckStatusDeadlineHead != wantHead {
			t.Fatalf("head = %q, want %q", got.CheckStatusDeadlineHead, wantHead)
		}
		if (want == nil) != (got.CheckStatusDeadline == nil) || (want != nil && !got.CheckStatusDeadline.Equal(*want)) {
			t.Fatalf("deadline = %v, want %v", got.CheckStatusDeadline, want)
		}
		listed, err := st.ListTasksFiltered(ctx, store.TaskFilter{States: []core.TaskState{got.State}})
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range listed {
			if candidate.ID == id && candidate.CheckStatusDeadlineHead != wantHead {
				t.Fatalf("listed head = %q, want %q", candidate.CheckStatusDeadlineHead, wantHead)
			}
		}
	}
	read(t, "head-a", &base)
	if count(t, id) != 1 {
		t.Fatalf("events = %d, want 1", count(t, id))
	}

	later := base.Add(time.Hour)
	recorded, err = st.RecordCheckStatusDeadline(ctx, id, "head-a", later, event(id, "head-a", "pending"))
	if err != nil || recorded {
		t.Fatalf("second record=%t err=%v, want no change", recorded, err)
	}
	read(t, "head-a", &base)
	if count(t, id) != 1 {
		t.Fatalf("a later read appended: %d", count(t, id))
	}

	cleared, err := st.ClearCheckStatusDeadline(ctx, id)
	if err != nil || !cleared {
		t.Fatalf("clear=%t err=%v", cleared, err)
	}
	read(t, "head-a", nil)
	if cleared, err = st.ClearCheckStatusDeadline(ctx, id); err != nil || cleared {
		t.Fatalf("second clear=%t err=%v", cleared, err)
	}

	recorded, err = st.RecordCheckStatusDeadline(ctx, id, "head-a", later, event(id, "head-a", "pending"))
	if err != nil || !recorded {
		t.Fatalf("pending again record=%t err=%v", recorded, err)
	}
	read(t, "head-a", &later)

	newHead := later.Add(time.Hour)
	recorded, err = st.RecordCheckStatusDeadline(ctx, id, "head-b", newHead, event(id, "head-b", "read_failed"))
	if err != nil || !recorded {
		t.Fatalf("new head record=%t err=%v", recorded, err)
	}
	read(t, "head-b", &newHead)
	if count(t, id) != 3 {
		t.Fatalf("events = %d, want 3", count(t, id))
	}

	if _, err = st.RecordCheckStatusDeadline(ctx, id, " ", later, event(id, "", "")); !errors.Is(err, store.ErrCheckStatusHeadRequired) {
		t.Fatalf("blank head err = %v", err)
	}
	if _, err = st.RecordCheckStatusDeadline(ctx, "missing-task", "head", later, event("missing-task", "head", "")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task record err = %v", err)
	}
	if _, err = st.ClearCheckStatusDeadline(ctx, "missing-task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task clear err = %v", err)
	}

	// The retry enqueues one dispatch_task job, only for an approved gate-off
	// task, and a job that is still active holds the stream.
	if inserted, err := st.EnqueueCheckStatusRetry(ctx, id); err != nil || inserted {
		t.Fatalf("running task enqueue inserted=%t err=%v", inserted, err)
	}
	if _, err = st.EnqueueCheckStatusRetry(ctx, "missing-task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task enqueue err = %v", err)
	}
	approved := createTask(t, st, ctx, workspace, core.TaskApproved, "")
	stream := logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), approved)
	if inserted, err := st.EnqueueCheckStatusRetry(ctx, approved); err != nil || !inserted {
		t.Fatalf("approved task enqueue inserted=%t err=%v", inserted, err)
	}
	if inserted, err := st.EnqueueCheckStatusRetry(ctx, approved); err != nil || inserted {
		t.Fatalf("second enqueue inserted=%t err=%v", inserted, err)
	}
	job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil || job.Generation != 1 || job.State != logqueue.StateAvailable || job.MaxAttempts != queue.DispatchTaskMaxAttempts {
		t.Fatalf("job=%+v err=%v", job, err)
	}

	// The batch read returns only the check-status kinds in (at, id) order and
	// stays inside the workspace.
	for _, kind := range []string{store.EventCheckHold, store.EventCheckHoldCleared, "merge.requested", store.EventCheckOverride} {
		if err = st.AppendEvent(ctx, core.Event{TaskID: id, Kind: kind, Payload: core.JSONPayload(map[string]any{"head_sha": "head-b"})}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.ListCheckStatusEvents(ctx, []string{id, "missing-task"})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, recordedEvent := range events[id] {
		kinds = append(kinds, recordedEvent.Kind)
	}
	want := []string{store.EventCheckPending, store.EventCheckPending, store.EventCheckPending, store.EventCheckHold, store.EventCheckHoldCleared}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if _, listedMissing := events["missing-task"]; listedMissing {
		t.Fatal("a missing task was listed")
	}
	empty, err := st.ListCheckStatusEvents(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty read = %+v, %v", empty, err)
	}
	foreign, err := st.ListCheckStatusEvents(store.WithWorkspace(ctx, workspace+"-foreign"), []string{id})
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign workspace read = %+v, %v", foreign, err)
	}
}
